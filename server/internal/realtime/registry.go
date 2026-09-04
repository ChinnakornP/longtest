package realtime

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// ErrRuntimeOffline means no daemon for that runtime is connected to THIS
// process. It is a routing failure, not an authorization one: the caller
// decides whether to leave a run queued or to report it.
var ErrRuntimeOffline = errors.New("runtime is not connected")

// Target identifies one connected daemon. The org is part of the key, not a
// property of the runtime id, so a lookup can never cross a tenant boundary
// even if two organizations somehow held the same runtime uuid.
type Target struct {
	OrgID     uuid.UUID
	RuntimeID uuid.UUID
}

// sender is the write half of a daemon connection. It exists so the registry
// can be tested, and so the scheduler depends on "something I can send a frame
// to" rather than on a WebSocket.
type sender interface {
	send(ctx context.Context, frame []byte) error
	close(reason string)
}

// Registry tracks the daemon control-plane connections this process holds.
//
// It is process-local by design: a run is assigned to a runtime whose daemon is
// connected HERE, so a second API instance simply schedules onto the daemons it
// holds. Fan-out across instances would need Postgres LISTEN/NOTIFY or a broker,
// and the MVP deploys one instance (see ADR-005).
type Registry struct {
	mu    sync.RWMutex
	conns map[Target]sender
	// waiters are notified whenever a daemon connects, so the scheduler can
	// react to a new runtime within a tick instead of waiting one out.
	waiters map[chan struct{}]struct{}
}

// NewRegistry returns an empty registry. One is created per process.
func NewRegistry() *Registry {
	return &Registry{conns: map[Target]sender{}, waiters: map[chan struct{}]struct{}{}}
}

// register adds a connection and returns the function that removes it.
//
// A second connection for the same runtime displaces the first: that is a
// daemon reconnecting through a half-open socket the OS has not reaped, and
// keeping both would mean assigning the same run down a dead pipe half the
// time. The release function is idempotent and only removes the connection it
// registered, so the displaced one's deferred release cannot evict its
// successor.
func (r *Registry) register(t Target, s sender) func() {
	r.mu.Lock()
	previous := r.conns[t]
	r.conns[t] = s
	r.notifyLocked()
	r.mu.Unlock()

	if previous != nil {
		previous.close("replaced by a newer connection from this runtime")
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.conns[t] == s {
				delete(r.conns, t)
			}
		})
	}
}

// Send writes one frame to a runtime's daemon.
func (r *Registry) Send(ctx context.Context, t Target, frame []byte) error {
	r.mu.RLock()
	conn := r.conns[t]
	r.mu.RUnlock()

	if conn == nil {
		return ErrRuntimeOffline
	}
	if err := conn.send(ctx, frame); err != nil {
		return err
	}
	return nil
}

// Online reports whether a runtime's daemon is connected here.
func (r *Registry) Online(t Target) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conns[t] != nil
}

// Connected is the scheduler's work list: every daemon this process can hand a
// run to, right now.
func (r *Registry) Connected() []Target {
	r.mu.RLock()
	defer r.mu.RUnlock()
	targets := make([]Target, 0, len(r.conns))
	for t := range r.conns {
		targets = append(targets, t)
	}
	return targets
}

// Notify returns a channel that receives (non-blocking, at most one pending)
// whenever a daemon connects, and the function that unregisters it.
func (r *Registry) Notify() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	r.mu.Lock()
	r.waiters[ch] = struct{}{}
	r.mu.Unlock()

	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.waiters, ch)
	}
}

func (r *Registry) notifyLocked() {
	for ch := range r.waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// CloseAll ends every daemon connection. See Handler.Shutdown for why this is
// not something http.Server.Shutdown does for us.
func (r *Registry) CloseAll(reason string) {
	r.mu.Lock()
	conns := make([]sender, 0, len(r.conns))
	for _, conn := range r.conns {
		conns = append(conns, conn)
	}
	r.mu.Unlock()

	// Outside the lock: close() writes a close frame, and a slow peer must not
	// hold the registry while the next connection is being closed.
	for _, conn := range conns {
		conn.close(reason)
	}
}

// AsAPIError renders a routing failure for a client. Only ErrRuntimeOffline is
// translated; anything else is a genuine write failure and stays a 500.
func AsAPIError(err error) error {
	if errors.Is(err, ErrRuntimeOffline) {
		return httpx.Conflict("that runtime is not connected")
	}
	return err
}
