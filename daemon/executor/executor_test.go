package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// The tests drive a fake sidecar rather than the real Node one: this package
// is the protocol, and a test that needs `pnpm install` to run is a test
// nobody runs. The fake is this test binary re-executed with a mode flag, the
// standard helper-process pattern.
const sidecarModeEnv = "QA_TEST_SIDECAR_MODE"

func TestMain(m *testing.M) {
	if mode := os.Getenv(sidecarModeEnv); mode != "" {
		os.Exit(runFakeSidecar(mode))
	}
	os.Exit(m.Run())
}

// runFakeSidecar answers JSON-RPC on stdio the way daemon/executor/src does.
func runFakeSidecar(mode string) int {
	switch mode {
	case "silent":
		// Reads requests and never answers: exercises the caller's timeout.
		// It must stay alive, so this is a sleep rather than a block on an
		// empty select, which the runtime would report as a deadlock.
		time.Sleep(time.Minute)
		return 0
	case "exit-immediately":
		return 3
	case "spawner":
		// Holds a grandchild so a teardown test can prove the group died.
		child := exec.CommandContext(context.Background(), "sleep", "300")
		_ = child.Start()
		fmt.Fprintln(os.Stderr, "grandchild", child.Process.Pid)
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		var req struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			return 1
		}
		switch req.Method {
		case "session.open":
			version := ProtocolVersion
			if mode == "old-protocol" {
				version = 99
			}
			emit(map[string]any{"event": "progress", "data": map[string]any{"phase": "session"}})
			emit(map[string]any{"id": req.ID, "result": map[string]any{
				"sessionId": "default", "baseUrl": "http://app.test", "storageState": nil,
				"protocolVersion": version,
			}})
		case "testcase.run":
			if mode == "run-error" {
				emit(map[string]any{"id": req.ID, "error": map[string]any{
					"code": "BROWSER_LAUNCH_FAILED", "message": "no chromium", "data": map[string]any{"path": "/nope"},
				}})
				continue
			}
			if mode == "die-mid-call" {
				return 7
			}
			emit(map[string]any{"event": "step", "data": map[string]any{"index": 0}})
			emit(map[string]any{"id": req.ID, "result": map[string]any{
				"version": 1, "testCaseId": "TC-001", "result": "pass",
				"steps": []any{}, "artifacts": []any{},
				"startedAt": "2026-09-04T12:00:00Z", "endedAt": "2026-09-04T12:00:01Z",
			}})
		case "session.close":
			emit(map[string]any{"id": req.ID, "result": nil})
		default:
			emit(map[string]any{"id": req.ID, "error": map[string]any{
				"code": "INVALID_METHOD", "message": "unknown method " + req.Method,
			}})
		}
	}
	return 0
}

func emit(frame map[string]any) {
	data, _ := json.Marshal(frame)
	_, _ = fmt.Fprintf(os.Stdout, "%s\n", data)
}

func startFake(t *testing.T, mode string, onEvent func(Event)) *Client {
	t.Helper()
	c, err := Start(Options{
		Command:     []string{os.Args[0]},
		Env:         append(os.Environ(), sidecarModeEnv+"="+mode),
		OnEvent:     onEvent,
		CallTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background(), time.Second) })
	return c
}

func TestSessionAndTestCaseRoundTrip(t *testing.T) {
	var (
		mu     sync.Mutex
		events []Event
	)
	client := startFake(t, "ok", func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	session, err := client.SessionOpen(t.Context(), SessionOpenParams{BaseURL: "http://app.test"})
	if err != nil {
		t.Fatalf("SessionOpen: %v", err)
	}
	if session.SessionID != "default" {
		t.Fatalf("sessionId = %q", session.SessionID)
	}

	result, err := client.RunTestCase(t.Context(), TestcaseRunParams{
		TestCase:         qaschema.TestCase{Version: 1, ID: "TC-001", Name: "Login"},
		AppMap:           qaschema.ApplicationMap{Version: 1, BaseURL: "http://app.test"},
		ArtifactDir:      t.TempDir(),
		StorageKeyPrefix: "orgs/o/runs/2026-09-04/r/",
	})
	if err != nil {
		t.Fatalf("RunTestCase: %v", err)
	}
	if result.TestCaseID != "TC-001" || result.Result != qaschema.OutcomePass {
		t.Fatalf("result = %+v", result)
	}

	if err := client.SessionClose(t.Context()); err != nil {
		t.Fatalf("SessionClose: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("events = %+v, want the progress and step frames", events)
	}
}

func TestSidecarErrorIsTyped(t *testing.T) {
	client := startFake(t, "run-error", nil)
	if _, err := client.SessionOpen(t.Context(), SessionOpenParams{BaseURL: "http://app.test"}); err != nil {
		t.Fatalf("SessionOpen: %v", err)
	}

	_, err := client.RunTestCase(t.Context(), TestcaseRunParams{
		TestCase: qaschema.TestCase{Version: 1, ID: "TC-001"},
		AppMap:   qaschema.ApplicationMap{Version: 1, BaseURL: "http://app.test"},
	})
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want *executor.Error", err)
	}
	if rpcErr.Code != "BROWSER_LAUNCH_FAILED" {
		t.Fatalf("code = %q", rpcErr.Code)
	}
	if !strings.Contains(rpcErr.Error(), "no chromium") {
		t.Fatalf("message lost: %v", rpcErr)
	}
}

func TestProtocolSkewIsAnError(t *testing.T) {
	client := startFake(t, "old-protocol", nil)
	_, err := client.SessionOpen(t.Context(), SessionOpenParams{BaseURL: "http://app.test"})
	if err == nil {
		t.Fatal("a protocol mismatch must not be accepted")
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("error = %v", err)
	}
}

func TestPendingCallIsReleasedWhenSidecarDies(t *testing.T) {
	client := startFake(t, "die-mid-call", nil)
	if _, err := client.SessionOpen(t.Context(), SessionOpenParams{BaseURL: "http://app.test"}); err != nil {
		t.Fatalf("SessionOpen: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := client.RunTestCase(t.Context(), TestcaseRunParams{
			TestCase: qaschema.TestCase{Version: 1, ID: "TC-001"},
			AppMap:   qaschema.ApplicationMap{Version: 1, BaseURL: "http://app.test"},
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when the sidecar exits mid-call")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call outlived the sidecar it was waiting on")
	}
}

func TestCallTimesOut(t *testing.T) {
	client := startFake(t, "silent", nil)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err := client.Call(ctx, "session.open", SessionOpenParams{BaseURL: "http://app.test"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want DeadlineExceeded", err)
	}
}

func TestCloseKillsTheProcessTree(t *testing.T) {
	client := startFake(t, "spawner", nil)
	if _, err := client.SessionOpen(t.Context(), SessionOpenParams{BaseURL: "http://app.test"}); err != nil {
		t.Fatalf("SessionOpen: %v", err)
	}
	pid := client.PID()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Close(ctx, 500*time.Millisecond); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Close took %s, the cancel budget is 5s", elapsed)
	}
	if processAlive(pid) {
		t.Fatalf("sidecar %d survived Close", pid)
	}
}

func TestCallAfterCloseFails(t *testing.T) {
	client := startFake(t, "ok", nil)
	if err := client.Close(context.Background(), time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Call(context.Background(), "session.open", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("error = %v, want ErrClosed", err)
	}
}

func TestStartReportsMissingCommand(t *testing.T) {
	if _, err := Start(Options{Command: []string{"qa-executor-that-does-not-exist"}}); err == nil {
		t.Fatal("expected an error for a missing sidecar binary")
	}
}

func processAlive(pid int) bool {
	return exec.CommandContext(context.Background(), "kill", "-0", fmt.Sprint(pid)).Run() == nil
}
