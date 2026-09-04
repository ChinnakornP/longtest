package runtime

import (
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

func testEvent(t *testing.T, runID string, level qaschema.RunEventPayloadLevel, code string) qaschema.Envelope {
	t.Helper()
	env, err := newEnvelope(qaschema.EnvelopeTypeRunEvent, &runID, 0, qaschema.RunEventPayload{
		Phase: qaschema.RunEventPayloadPhaseExecute, Level: level, Code: code, Message: code,
	}, time.Now())
	if err != nil {
		t.Fatalf("newEnvelope: %v", err)
	}
	return env
}

func testResult(t *testing.T, runID string) qaschema.Envelope {
	t.Helper()
	env, err := newEnvelope(qaschema.EnvelopeTypeRunResult, &runID, 1, qaschema.RunResultPayload{
		Status: qaschema.RunResultPayloadStatusCompleted,
	}, time.Now())
	if err != nil {
		t.Fatalf("newEnvelope: %v", err)
	}
	return env
}

func TestOutboxIsFIFOAndAcksTheHead(t *testing.T) {
	o := newOutbox(10)
	first := testEvent(t, "run-1", qaschema.RunEventPayloadLevelInfo, "one")
	second := testEvent(t, "run-1", qaschema.RunEventPayloadLevelInfo, "two")
	o.Push(first)
	o.Push(second)

	head, ok := o.Head()
	if !ok || head.MsgID != first.MsgID {
		t.Fatalf("head = %+v, want the first frame", head)
	}
	// Head does not remove: a frame stays queued until a write succeeded, so a
	// connection that dies mid-write replays it.
	if o.Len() != 2 {
		t.Fatalf("len = %d after Head", o.Len())
	}
	o.Ack(first.MsgID)
	head, _ = o.Head()
	if head.MsgID != second.MsgID {
		t.Fatalf("head after ack = %+v", head)
	}
	// Acking something that is not the head is a no-op, so a late ack from a
	// dead connection cannot drop a frame the new one has not sent.
	o.Ack("00000000-0000-0000-0000-000000000000")
	if o.Len() != 1 {
		t.Fatalf("len = %d after a stale ack", o.Len())
	}
}

func TestOutboxDropsChatterBeforeResults(t *testing.T) {
	o := newOutbox(3)
	o.Push(testEvent(t, "run-1", qaschema.RunEventPayloadLevelDebug, "chatter"))
	o.Push(testEvent(t, "run-1", qaschema.RunEventPayloadLevelError, "phase_failed"))
	result := testResult(t, "run-1")
	o.Push(result)

	// Full: the next push must evict the debug event, not the result.
	warn := testEvent(t, "run-1", qaschema.RunEventPayloadLevelWarn, "run_cancelled")
	o.Push(warn)

	if o.Len() != 3 {
		t.Fatalf("len = %d, want the limit", o.Len())
	}
	if o.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", o.Dropped())
	}
	codes := map[string]bool{}
	for {
		head, ok := o.Head()
		if !ok {
			break
		}
		if head.Type == qaschema.EnvelopeTypeRunResult {
			codes["result"] = true
		} else {
			var payload qaschema.RunEventPayload
			if err := decodePayload(head.Payload, &payload); err != nil {
				t.Fatalf("decode: %v", err)
			}
			codes[payload.Code] = true
		}
		o.Ack(head.MsgID)
	}
	if codes["chatter"] {
		t.Fatal("the debug event survived and something more important was dropped")
	}
	if !codes["result"] || !codes["phase_failed"] || !codes["run_cancelled"] {
		t.Fatalf("queue kept %v", codes)
	}
}

func TestOutboxRefusesToDropTerminalFrames(t *testing.T) {
	o := newOutbox(2)
	o.Push(testResult(t, "run-1"))
	o.Push(testResult(t, "run-2"))
	o.Push(testResult(t, "run-3"))

	if o.Len() != 2 {
		t.Fatalf("len = %d", o.Len())
	}
	// The third result is refused rather than displacing one already queued:
	// dropping a result loses a run's only terminal signal.
	head, _ := o.Head()
	runID := ""
	if head.RunID != nil {
		runID = *head.RunID
	}
	if runID != "run-1" {
		t.Fatalf("head run = %q, want the oldest result kept", runID)
	}
	if o.Dropped() != 1 {
		t.Fatalf("dropped = %d", o.Dropped())
	}
}

func TestOutboxWaitWakesOnPush(t *testing.T) {
	o := newOutbox(4)

	select {
	case <-o.Wait():
		t.Fatal("an empty outbox should not report ready")
	default:
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		o.Push(testEvent(t, "run-1", qaschema.RunEventPayloadLevelInfo, "hello"))
	}()

	select {
	case <-o.Wait():
	case <-time.After(2 * time.Second):
		t.Fatal("a waiting writer was not woken by a push")
	}

	// A non-empty outbox reports ready immediately, so a writer that drained
	// only part of the queue comes straight back.
	select {
	case <-o.Wait():
	default:
		t.Fatal("a non-empty outbox should report ready")
	}
}
