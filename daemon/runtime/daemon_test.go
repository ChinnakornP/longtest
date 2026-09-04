package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/executor"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

func TestHelloReportsWhatThisMachineCanDo(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	env := h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	payload := decodeAs[qaschema.HelloPayload](t, env.Payload)

	if payload.RuntimeID != h.config.RuntimeID {
		t.Fatalf("runtimeId = %q, want %q", payload.RuntimeID, h.config.RuntimeID)
	}
	if payload.Version != Version {
		t.Fatalf("version = %q", payload.Version)
	}
	if len(payload.Browsers) != 1 || payload.Browsers[0] != qaschema.HelloPayloadBrowsersItemChromium {
		t.Fatalf("browsers = %v", payload.Browsers)
	}
	// A CLI that is installed and one that is not both appear: the UI has to
	// be able to say why an agent is not offered.
	if len(payload.Agents) != 2 {
		t.Fatalf("agents = %+v, want both the usable and the unusable one", payload.Agents)
	}
	if !payload.Agents[0].Ok || payload.Agents[1].Ok {
		t.Fatalf("agent ok flags = %v/%v", payload.Agents[0].Ok, payload.Agents[1].Ok)
	}
	if payload.Agents[1].Error == nil || *payload.Agents[1].Error == "" {
		t.Fatal("an unusable agent must carry the reason")
	}

	if got := h.backend.AuthHeader(); got != "Bearer "+h.config.Token {
		t.Fatalf("Authorization = %q", got)
	}
	if env.RunID != nil {
		t.Fatalf("hello carried a runId: %v", *env.RunID)
	}
}

func TestHeartbeatsKeepComing(t *testing.T) {
	h := newHarness(t, harnessOptions{heartbeat: 30 * time.Millisecond})

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	for i := range 3 {
		env := h.backend.ExpectType(3*time.Second, qaschema.EnvelopeTypeHeartbeat)
		payload := decodeAs[qaschema.HeartbeatPayload](t, env.Payload)
		if payload.UptimeSec < 0 {
			t.Fatalf("heartbeat %d reported uptime %d", i, payload.UptimeSec)
		}
	}
}

// The acceptance criterion: kill the backend, bring it back, and the daemon
// reconnects on its own inside 30 seconds.
func TestReconnectsAfterTheBackendGoesAway(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	if dials := h.backend.Dials(); dials != 1 {
		t.Fatalf("dials = %d", dials)
	}

	start := time.Now()
	h.backend.Drop()

	// A second hello is the daemon re-introducing itself on a new connection.
	h.backend.ExpectType(30*time.Second, qaschema.EnvelopeTypeHello)
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("reconnect took %s, budget is 30s", elapsed)
	}
	if dials := h.backend.Dials(); dials < 2 {
		t.Fatalf("dials = %d, want a redial", dials)
	}

	state := h.state.Snapshot()
	if state.Reconnects < 1 {
		t.Fatalf("state did not record the reconnect: %+v", state)
	}
}

// A run keeps going while the connection is down, and everything it produced
// meanwhile arrives once a connection exists again.
func TestRunSurvivesAReconnectAndReportsAfterwards(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, harnessOptions{})
	h.executor.blockUntil = release

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)

	assign := assignFrame(t, assignOptions{
		withMap:   true,
		testCases: []any{testCase("TC-001", "Create employee")},
	})
	h.backend.Send(assign)

	// Wait until the run is actually executing before pulling the plug.
	waitFor(t, 5*time.Second, func() bool { return len(h.executor.RanCases()) == 1 })

	h.backend.Drop()
	close(release)

	result := h.backend.ExpectType(30*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusCompleted {
		t.Fatalf("status = %q, error = %+v", payload.Status, payload.Error)
	}
	if len(payload.Executions) != 1 {
		t.Fatalf("executions = %d", len(payload.Executions))
	}
	if ran := h.executor.RanCases(); len(ran) != 1 {
		t.Fatalf("the reconnect re-ran test cases: %v", ran)
	}
}

// run.assign is delivered at-least-once. Accepting it twice would run whatever
// the test case does to the application under test twice.
func TestDuplicateAssignNeverRunsTwice(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)

	assign := assignFrame(t, assignOptions{
		withMap:   true,
		testCases: []any{testCase("TC-001", "Create employee")},
	})
	h.backend.Send(assign)
	h.backend.Send(assign)

	first := h.backend.ExpectType(10*time.Second, qaschema.EnvelopeTypeRunResult)
	if decodeAs[qaschema.RunResultPayload](t, first.Payload).Status != qaschema.RunResultPayloadStatusCompleted {
		t.Fatalf("first result: %s", first.Payload)
	}

	// A third assignment after the run finished is answered by repeating the
	// result, not by executing it again.
	h.backend.Send(assign)
	second := h.backend.ExpectType(10*time.Second, qaschema.EnvelopeTypeRunResult)
	if *second.RunID != *first.RunID {
		t.Fatalf("second result is for a different run")
	}
	if second.Seq <= first.Seq {
		t.Fatalf("re-reported result reused seq %d (first was %d); the server would deduplicate it away",
			second.Seq, first.Seq)
	}

	if ran := h.executor.RanCases(); len(ran) != 1 {
		t.Fatalf("test cases ran %v, want exactly one execution of TC-001", ran)
	}
}

// The acceptance criterion: cancel kills the executor and the AI CLI inside
// five seconds and the run reports what happened.
func TestCancelStopsTheRunWithinTheBudget(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.executor.blockUntil = make(chan struct{}) // never closed: only cancel ends it

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)

	assign := assignFrame(t, assignOptions{
		withMap:   true,
		testCases: []any{testCase("TC-001", "Create employee")},
	})
	h.backend.Send(assign)
	waitFor(t, 5*time.Second, func() bool { return len(h.executor.RanCases()) == 1 })

	runID := decodeAs[qaschema.RunAssignPayload](t, assign.Payload).RunID
	start := time.Now()
	h.backend.Send(cancelFrame(t, runID, qaschema.RunCancelPayloadReasonUserRequested, "stopped from the UI"))

	result := h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeRunResult)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("cancel took %s, the contract budget is 5s", elapsed)
	}

	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusCancelled {
		t.Fatalf("status = %q, want cancelled", payload.Status)
	}
	if payload.Error != nil {
		t.Fatalf("a cancelled run is not a failed one: %+v", payload.Error)
	}
	if closes := h.executor.Closes(); closes != 1 {
		t.Fatalf("executor Close called %d times; the sidecar and its browser must be torn down", closes)
	}
}

func TestCancelForAnUnknownRunIsReportedNotIgnored(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)

	h.backend.Send(cancelFrame(t, "6f1c2f2b-1c3f-4f3a-8f1a-2c3d4e5f6a7b",
		qaschema.RunCancelPayloadReasonUserRequested, "stop"))

	env := h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeRunEvent)
	payload := decodeAs[qaschema.RunEventPayload](t, env.Payload)
	if payload.Code != "cancel_ignored" {
		t.Fatalf("code = %q", payload.Code)
	}
}

// Shutdown must not leave a browser running on the operator's machine, and it
// must tell the backend what happened to the run it was working on — otherwise
// the run sits in "running" until something times it out.
func TestShutdownCancelsInFlightRunsAndReportsThem(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.executor.blockUntil = make(chan struct{})

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	assign := assignFrame(t, assignOptions{
		withMap:   true,
		testCases: []any{testCase("TC-001", "Create employee")},
	})
	h.backend.Send(assign)
	waitFor(t, 5*time.Second, func() bool { return len(h.executor.RanCases()) == 1 })

	go h.Stop()

	// The result travels on the connection the daemon is about to close: the
	// shutdown path cancels runs first and closes the socket last.
	result := h.backend.ExpectType(10*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusCancelled {
		t.Fatalf("status = %q, want cancelled", payload.Status)
	}
	if *result.RunID != decodeAs[qaschema.RunAssignPayload](t, assign.Payload).RunID {
		t.Fatal("the result is for a different run")
	}

	if err := h.WaitExit(10 * time.Second); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if closes := h.executor.Closes(); closes != 1 {
		t.Fatalf("executor Close called %d times during shutdown", closes)
	}
	if state := h.state.Snapshot(); state.Connection != ConnectionStopped {
		t.Fatalf("state after shutdown = %q", state.Connection)
	}
}

func TestRejectedTokenStopsTheDaemon(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)

	// A revoked token: retrying forever would be pointless and noisy.
	h.backend.RejectWith(401)
	h.backend.Drop()

	err := h.WaitExit(15 * time.Second)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Run returned %v, want ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "qa-daemon pair") {
		t.Fatalf("error should tell the operator what to do: %v", err)
	}
	if state := h.state.Snapshot(); state.Connection != ConnectionStopped {
		t.Fatalf("state = %q", state.Connection)
	}
}

// A run whose mode needs an AI CLI on a runtime that has none must fail with
// the contract's own code, not with a generic internal error.
func TestPhaseWithoutAnAgentFailsAsUnavailable(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)

	h.backend.Send(assignFrame(t, assignOptions{mode: qaschema.RunAssignPayloadModeDiscover}))

	result := h.backend.ExpectType(10*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusFailed {
		t.Fatalf("status = %q", payload.Status)
	}
	if payload.Error == nil || payload.Error.Code != qaschema.RunErrorCodeAgentNotAvailable {
		t.Fatalf("error = %+v, want agent_not_available", payload.Error)
	}
}

func TestExecuteModeWithoutTestCasesFails(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)

	h.backend.Send(assignFrame(t, assignOptions{withMap: true}))

	result := h.backend.ExpectType(10*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusFailed {
		t.Fatalf("status = %q", payload.Status)
	}
	if payload.Error == nil || !strings.Contains(payload.Error.Message, "test cases") {
		t.Fatalf("error = %+v", payload.Error)
	}
}

func TestSessionOpenFailureIsClassified(t *testing.T) {
	failing := &fakeExecutor{openErr: &executor.Error{Code: "BROWSER_LAUNCH_FAILED", Message: "no chromium"}}
	h := newHarness(t, harnessOptions{
		newExec: func(executor.Options) (ExecutorClient, error) { return failing, nil },
	})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)

	h.backend.Send(assignFrame(t, assignOptions{
		withMap:   true,
		testCases: []any{testCase("TC-001", "Create employee")},
	}))

	result := h.backend.ExpectType(10*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Error == nil || payload.Error.Code != qaschema.RunErrorCodeBrowserLaunchFailed {
		t.Fatalf("error = %+v, want browser_launch_failed", payload.Error)
	}
	// The sidecar's own message may name a local path; only the daemon's
	// summary is forwarded.
	if strings.Contains(payload.Error.Message, "no chromium") {
		t.Fatalf("the sidecar's raw message reached the backend: %q", payload.Error.Message)
	}
}

// Sequence numbers are what the server deduplicates on. Restarting them at
// zero on reconnect would make post-reconnect events collide with the ones
// already stored and be silently dropped.
func TestRunSequenceNumbersNeverRestart(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, harnessOptions{})
	h.executor.blockUntil = release

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	assign := assignFrame(t, assignOptions{
		withMap:   true,
		testCases: []any{testCase("TC-001", "Create employee")},
	})
	h.backend.Send(assign)
	runID := decodeAs[qaschema.RunAssignPayload](t, assign.Payload).RunID

	// Collect the seqs from before the drop.
	before := h.backend.Expect(10*time.Second, func(env qaschema.Envelope) bool {
		return env.Type == qaschema.EnvelopeTypeRunEvent && env.RunID != nil && *env.RunID == runID
	})

	waitFor(t, 5*time.Second, func() bool { return len(h.executor.RanCases()) == 1 })
	h.backend.Drop()
	close(release)

	after := h.backend.Expect(30*time.Second, func(env qaschema.Envelope) bool {
		return env.Type == qaschema.EnvelopeTypeRunResult && env.RunID != nil && *env.RunID == runID
	})
	if after.Seq <= before.Seq {
		t.Fatalf("seq went from %d to %d across a reconnect", before.Seq, after.Seq)
	}
}

func TestUnknownFrameTypeDoesNotDropTheConnection(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	dials := h.backend.Dials()

	// A frame the daemon has no business receiving. It is logged and skipped;
	// dropping the connection would take every in-flight run's stream with it.
	env := assignFrame(t, assignOptions{withMap: true})
	env.Type = qaschema.EnvelopeTypeHeartbeat
	env.RunID = nil
	env.Payload = []byte(`{"uptimeSec":1}`)
	h.backend.Send(env)

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHeartbeat)
	if h.backend.Dials() != dials {
		t.Fatal("the connection was dropped over one unusable frame")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
