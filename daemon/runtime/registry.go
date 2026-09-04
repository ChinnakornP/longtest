package runtime

import (
	"sync"
	"time"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// claimStatus is what the registry knew about a run when a run.assign arrived.
type claimStatus int

const (
	// claimNew means this daemon has not seen the run before.
	claimNew claimStatus = iota
	// claimActive means the run is already executing here.
	claimActive
	// claimFinished means the run finished here and its result is remembered.
	claimFinished
)

// runRegistry is the daemon's idempotency gate.
//
// run.assign is delivered at-least-once, and a reconnecting daemon can be sent
// an assignment it already accepted. Re-running a test case because the
// connection blinked would double-submit whatever that case does to the
// application under test, so a run is executed at most once per daemon
// lifetime: the active set covers the reconnect case, and a bounded memory of
// finished runs covers the "assign arrived again after we answered" case,
// where the answer is to repeat the result rather than the work.
type runRegistry struct {
	mu       sync.Mutex
	active   map[string]*runController
	finished map[string]resultPayload
	order    []string
	limit    int

	wg sync.WaitGroup
}

func newRunRegistry(limit int) *runRegistry {
	if limit <= 0 {
		limit = 64
	}
	return &runRegistry{
		active:   map[string]*runController{},
		finished: map[string]resultPayload{},
		limit:    limit,
	}
}

// Claim reports what is known about a run and, when it is new, reserves it.
func (r *runRegistry) Claim(runID string) (claimStatus, resultPayload) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.active[runID]; ok {
		return claimActive, resultPayload{}
	}
	if result, ok := r.finished[runID]; ok {
		return claimFinished, result
	}
	r.active[runID] = nil
	return claimNew, resultPayload{}
}

// Attach stores the controller for a claimed run so cancel can find it.
func (r *runRegistry) Attach(runID string, rc *runController) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active[runID] = rc
	r.wg.Add(1)
}

// Release moves a run from active to finished, remembering its result.
// It returns the run ids whose bookkeeping was evicted, so their sequence
// counters can be dropped too.
func (r *runRegistry) Release(runID string, result resultPayload) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.active[runID]; ok {
		delete(r.active, runID)
		r.wg.Done()
	}
	if _, remembered := r.finished[runID]; !remembered {
		r.order = append(r.order, runID)
	}
	r.finished[runID] = result

	var evicted []string
	for len(r.order) > r.limit {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.finished, oldest)
		evicted = append(evicted, oldest)
	}
	return evicted
}

// Cancel asks one run to stop. It reports whether the run was here to cancel.
func (r *runRegistry) Cancel(runID string, reason qaschema.RunCancelPayloadReason, message string) bool {
	r.mu.Lock()
	rc := r.active[runID]
	r.mu.Unlock()
	if rc == nil {
		return false
	}
	rc.Cancel(reason, message)
	return true
}

// CancelAll stops every active run and returns how many were asked.
func (r *runRegistry) CancelAll(reason qaschema.RunCancelPayloadReason, message string) int {
	r.mu.Lock()
	controllers := make([]*runController, 0, len(r.active))
	for _, rc := range r.active {
		if rc != nil {
			controllers = append(controllers, rc)
		}
	}
	r.mu.Unlock()

	for _, rc := range controllers {
		rc.Cancel(reason, message)
	}
	return len(controllers)
}

// WaitAll blocks until every active run has finished or the timeout expires.
// It returns whether the runs finished in time; the caller logs the difference
// rather than hanging on a wedged run forever.
func (r *runRegistry) WaitAll(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// ActiveIDs lists the runs this daemon is working on, for the heartbeat.
func (r *runRegistry) ActiveIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.active))
	for id := range r.active {
		out = append(out, id)
	}
	return out
}

// ActiveCount is how many runs are in flight.
func (r *runRegistry) ActiveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active)
}
