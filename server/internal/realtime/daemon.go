package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// ControlPlane is everything a daemon connection needs from the domain layer.
// internal/run implements it.
//
// Every method takes the auth.RuntimeCaller the token resolved to, and none of
// them takes an organization id as data: that is the whole point of the
// interface's shape. A frame claiming another tenant's run reaches a method
// that was given this tenant's caller, and the lookup simply finds nothing.
type ControlPlane interface {
	// Hello records a daemon's capability report. The runtime id inside the
	// payload is checked against the token's before this is called.
	Hello(ctx context.Context, rc auth.RuntimeCaller, payload qaschema.HelloPayload) error
	// Heartbeat refreshes last_seen_at for the runtime and the lease on every
	// run it says it is still working on.
	Heartbeat(ctx context.Context, rc auth.RuntimeCaller, payload qaschema.HeartbeatPayload) error
	// RunEvent appends one event. It is idempotent on (runID, seq).
	//
	// The payload is handed over as raw JSON, already validated against
	// daemon-envelope@1. Both this and RunResult carry documents the backend
	// stores but does not own — an event's free-form `data`, a test case's
	// whole test-case@1 body — and decoding then re-encoding them would
	// reorder keys and silently drop anything a newer minor version added.
	RunEvent(ctx context.Context, rc auth.RuntimeCaller, runID uuid.UUID, seq int64, ts time.Time, payload json.RawMessage) error
	// RunResult ingests a terminal result: executions, artifacts, findings,
	// the application map and the test plan, then finishes the run.
	RunResult(ctx context.Context, rc auth.RuntimeCaller, runID uuid.UUID, payload json.RawMessage) error
	// RuntimeDisconnected is called once when a control-plane connection ends,
	// so the runs it was holding are dealt with without waiting out a lease.
	RuntimeDisconnected(ctx context.Context, rc auth.RuntimeCaller)
}

// DaemonHandler serves WS /api/v1/daemon.
type DaemonHandler struct {
	registry *Registry
	plane    ControlPlane
	logger   *slog.Logger
}

// NewDaemonHandler returns the control-plane handler.
func NewDaemonHandler(registry *Registry, plane ControlPlane, logger *slog.Logger) *DaemonHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &DaemonHandler{registry: registry, plane: plane, logger: logger}
}

// ServeHTTP upgrades an authenticated daemon request and runs its read loop
// until the connection ends.
//
// It must be mounted behind auth.RequireRuntime: the caller in the context is
// the only statement of which organization and runtime this socket belongs to,
// and nothing read off the wire ever revises it.
func (h *DaemonHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc, err := auth.MustRuntimeCaller(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// A daemon is not a browser: it sends no Origin, and the credential is
		// a bearer token rather than an ambient cookie, so there is no
		// cross-site request to forge here.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionContextTakeover,
	})
	if err != nil {
		// Accept has already written a response.
		h.logger.WarnContext(r.Context(), "daemon websocket upgrade failed", "err", err)
		return
	}

	c := newConn(ws)
	release := h.registry.register(Target{OrgID: rc.OrgID, RuntimeID: rc.RuntimeID}, c)

	// The request context is cancelled when ServeHTTP returns, which is what
	// stops the keepalive; the read loop below is what keeps it running.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	go c.keepalive(ctx)

	logger := h.logger.With(
		slog.String("org_id", rc.OrgID.String()),
		slog.String("runtime_id", rc.RuntimeID.String()),
	)
	logger.InfoContext(ctx, "daemon connected")

	h.readLoop(ctx, c, rc, logger)

	release()
	c.closeNormal("connection closed")
	logger.InfoContext(ctx, "daemon disconnected")

	// Deal with the runs this daemon was holding on a context of its own: the
	// one above is about to be cancelled by the deferred cancel.
	disconnectCtx, disconnectCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer disconnectCancel()
	h.plane.RuntimeDisconnected(disconnectCtx, rc)
}

func (h *DaemonHandler) readLoop(ctx context.Context, c *conn, rc auth.RuntimeCaller, logger *slog.Logger) {
	for {
		typ, raw, err := c.ws.Read(ctx)
		if err != nil {
			if !isExpectedClose(err) {
				logger.WarnContext(ctx, "daemon read failed", "err", err)
			}
			return
		}
		if typ != websocket.MessageText {
			c.close("control-plane frames must be text")
			return
		}

		if err := h.dispatch(ctx, rc, raw); err != nil {
			var protocolErr *ProtocolError
			if errors.As(err, &protocolErr) {
				// A frame the contract does not describe, or one naming a run
				// this runtime may not touch. Both are the daemon's bug and
				// both end the connection: there is no per-frame error channel
				// in contract D, and continuing would hide the problem.
				logger.WarnContext(ctx, "daemon protocol error", "err", protocolErr.Reason)
				c.close(protocolErr.Reason)
				return
			}
			// A storage failure. Do not close: the daemon's delivery is
			// at-least-once, so it will replay, and dropping the socket would
			// turn one failed insert into a whole reconnect cycle.
			logger.ErrorContext(ctx, "control-plane frame failed", "err", err)
		}
	}
}

// dispatch validates one frame and routes it. Every branch derives the
// organization from rc, never from the frame.
func (h *DaemonHandler) dispatch(ctx context.Context, rc auth.RuntimeCaller, raw []byte) error {
	envelope, err := ParseFrame(raw)
	if err != nil {
		return err
	}

	switch envelope.Type {
	case qaschema.EnvelopeTypeHello:
		payload, err := DecodePayload[qaschema.HelloPayload](envelope)
		if err != nil {
			return err
		}
		// The token already said which runtime this is. A hello that disagrees
		// is a misconfigured daemon holding the wrong token file, and letting
		// it through would write one machine's capabilities onto another's row.
		if payload.RuntimeID != rc.RuntimeID.String() {
			return &ProtocolError{Reason: "hello names a different runtime than the token"}
		}
		return h.plane.Hello(ctx, rc, payload)

	case qaschema.EnvelopeTypeHeartbeat:
		payload, err := DecodePayload[qaschema.HeartbeatPayload](envelope)
		if err != nil {
			return err
		}
		return h.plane.Heartbeat(ctx, rc, payload)

	case qaschema.EnvelopeTypeRunEvent:
		runID, err := FrameRunID(envelope)
		if err != nil {
			return err
		}
		ts, err := FrameTime(envelope)
		if err != nil {
			return err
		}
		return h.plane.RunEvent(ctx, rc, runID, int64(envelope.Seq), ts, envelope.Payload)

	case qaschema.EnvelopeTypeRunResult:
		runID, err := FrameRunID(envelope)
		if err != nil {
			return err
		}
		return h.plane.RunResult(ctx, rc, runID, envelope.Payload)

	case qaschema.EnvelopeTypeRunAssign, qaschema.EnvelopeTypeRunCancel:
		// Server-to-daemon frames. A daemon sending one is either confused or
		// probing; either way it is not a frame we have a handler for.
		return &ProtocolError{Reason: string(envelope.Type) + " is a server-to-daemon frame"}

	default:
		return &ProtocolError{Reason: "unknown frame type " + string(envelope.Type)}
	}
}

// isExpectedClose reports whether a read error is an ordinary end of
// connection rather than something worth logging as a fault.
func isExpectedClose(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
		return true
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway, websocket.StatusNoStatusRcvd,
		websocket.StatusAbnormalClosure:
		return true
	}
	return false
}
