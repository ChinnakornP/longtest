package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/daemon/executor"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/workspace"
)

// fakeBackend is the control plane the daemon dials. It validates every frame
// the daemon sends against contract D, so a test that passes here is a test
// against the real wire format rather than against this package's structs.
type fakeBackend struct {
	t   *testing.T
	srv *httptest.Server

	frames chan qaschema.Envelope

	mu         sync.Mutex
	conn       *websocket.Conn
	authHeader string
	dials      int
	rejectWith int
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	fb := &fakeBackend{t: t, frames: make(chan qaschema.Envelope, 512)}

	mux := http.NewServeMux()
	mux.HandleFunc(DaemonPath, func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.dials++
		fb.authHeader = r.Header.Get("Authorization")
		reject := fb.rejectWith
		fb.mu.Unlock()

		if reject != 0 {
			http.Error(w, "no", reject)
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		conn.SetReadLimit(readLimit)

		fb.mu.Lock()
		fb.conn = conn
		fb.mu.Unlock()

		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := validateFrame(data); err != nil {
				fb.t.Errorf("daemon sent a frame that does not match contract D: %v\n%s", err, data)
				continue
			}
			var env qaschema.Envelope
			if err := json.Unmarshal(data, &env); err != nil {
				fb.t.Errorf("undecodable frame: %v", err)
				continue
			}
			select {
			case fb.frames <- env:
			default:
				fb.t.Error("fake backend frame buffer overflowed")
			}
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	fb.srv = httptest.NewServer(mux)
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeBackend) URL() string { return fb.srv.URL }

func (fb *fakeBackend) Dials() int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.dials
}

func (fb *fakeBackend) AuthHeader() string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.authHeader
}

func (fb *fakeBackend) RejectWith(status int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.rejectWith = status
}

// Drop kills the current connection the way a backend restart does.
func (fb *fakeBackend) Drop() {
	fb.mu.Lock()
	conn := fb.conn
	fb.conn = nil
	fb.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusGoingAway, "backend restarting")
	}
}

// Send delivers a server-to-daemon frame on the live connection.
func (fb *fakeBackend) Send(env qaschema.Envelope) {
	fb.t.Helper()

	data, err := json.Marshal(env)
	if err != nil {
		fb.t.Fatalf("encode frame: %v", err)
	}
	// The test's own frames go through the same validator: a fixture that
	// does not match the contract would otherwise fail as a daemon bug.
	if err := validateFrame(data); err != nil {
		fb.t.Fatalf("test frame does not match contract D: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		fb.mu.Lock()
		conn := fb.conn
		fb.mu.Unlock()
		if conn != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := conn.Write(ctx, websocket.MessageText, data)
			cancel()
			if err == nil {
				return
			}
		}
		if time.Now().After(deadline) {
			fb.t.Fatalf("no live connection to send %s on", env.Type)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Expect waits for the next frame matching a predicate, and reports what it
// saw instead when it gives up.
func (fb *fakeBackend) Expect(timeout time.Duration, match func(qaschema.Envelope) bool) qaschema.Envelope {
	fb.t.Helper()

	var skipped []string
	deadline := time.After(timeout)
	for {
		select {
		case env := <-fb.frames:
			if match(env) {
				return env
			}
			skipped = append(skipped, string(env.Type))
		case <-deadline:
			fb.t.Fatalf("timed out after %s; frames seen meanwhile: %v", timeout, skipped)
			return qaschema.Envelope{}
		}
	}
}

// ExpectType waits for the next frame of a type.
func (fb *fakeBackend) ExpectType(timeout time.Duration, frameType qaschema.EnvelopeType) qaschema.Envelope {
	fb.t.Helper()
	return fb.Expect(timeout, func(env qaschema.Envelope) bool { return env.Type == frameType })
}

// fakeExecutor stands in for the Node sidecar.
type fakeExecutor struct {
	mu sync.Mutex

	openErr  error
	runErr   error
	runDelay time.Duration
	// blockUntil, when non-nil, holds RunTestCase until it is closed or the
	// context ends — the shape a long-running test case has.
	blockUntil chan struct{}
	// onRun builds the result for a case; nil means a passing case with no
	// evidence.
	onRun func(executor.TestcaseRunParams) qaschema.ExecutionResult

	ranCases    []string
	opened      int
	closed      int
	sessionEnds int
	closedAt    time.Time
}

func (f *fakeExecutor) SessionOpen(_ context.Context, _ executor.SessionOpenParams) (executor.SessionOpenResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	if f.openErr != nil {
		return executor.SessionOpenResult{}, f.openErr
	}
	return executor.SessionOpenResult{SessionID: "default", ProtocolVersion: executor.ProtocolVersion}, nil
}

func (f *fakeExecutor) RunTestCase(ctx context.Context, params executor.TestcaseRunParams) (qaschema.ExecutionResult, error) {
	f.mu.Lock()
	f.ranCases = append(f.ranCases, params.TestCase.ID)
	block, delay, runErr, onRun := f.blockUntil, f.runDelay, f.runErr, f.onRun
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return qaschema.ExecutionResult{}, ctx.Err()
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return qaschema.ExecutionResult{}, ctx.Err()
		}
	}
	if runErr != nil {
		return qaschema.ExecutionResult{}, runErr
	}
	if onRun != nil {
		return onRun(params), nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	return qaschema.ExecutionResult{
		Version:    1,
		TestCaseID: params.TestCase.ID,
		Result:     qaschema.OutcomePass,
		Steps:      []qaschema.StepResult{},
		Artifacts:  []qaschema.Artifact{},
		StartedAt:  now,
		EndedAt:    now,
	}, nil
}

func (f *fakeExecutor) SessionClose(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionEnds++
	return nil
}

func (f *fakeExecutor) Close(context.Context, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	f.closedAt = time.Now()
	return nil
}

func (f *fakeExecutor) RanCases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ranCases...)
}

func (f *fakeExecutor) Closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// harness wires a daemon to a fake backend and a fake executor.
type harness struct {
	t             *testing.T
	backend       *fakeBackend
	daemon        *Daemon
	executor      *fakeExecutor
	state         *StateFile
	config        Config
	workspaceRoot string
	cancel        context.CancelFunc

	// exited is closed once Run has returned, and exitErr holds what it
	// returned. A channel of one error would be consumed by whichever of the
	// test body and the cleanup read it first.
	exited  chan struct{}
	exitErr error
}

type harnessOptions struct {
	workspaceRoot string
	agent         AgentRunner
	detect        func(context.Context) Capabilities
	heartbeat     time.Duration
	newExec       func(executor.Options) (ExecutorClient, error)
	retention     workspace.Retention
	backoff       Backoff
	httpClient    *http.Client
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()

	backend := newFakeBackend(t)
	root := opts.workspaceRoot
	if root == "" {
		root = t.TempDir()
	}
	retention := opts.retention
	if retention == (workspace.Retention{}) {
		retention = workspace.DefaultRetention()
	}
	manager, err := workspace.NewManager(root, retention)
	if err != nil {
		t.Fatalf("workspace.NewManager: %v", err)
	}

	cfg := Config{
		ServerURL:   backend.URL(),
		RuntimeID:   uuid.NewString(),
		RuntimeName: "test-runtime",
		Token:       "qart_test_token",
	}

	state, err := NewStateFile(t.TempDir()+"/state.json", State{
		RuntimeID: cfg.RuntimeID, ServerURL: cfg.ServerURL, Connection: ConnectionConnecting,
	}, nil)
	if err != nil {
		t.Fatalf("NewStateFile: %v", err)
	}

	fake := &fakeExecutor{}
	newExec := opts.newExec
	if newExec == nil {
		newExec = func(executor.Options) (ExecutorClient, error) { return fake, nil }
	}
	detect := opts.detect
	if detect == nil {
		detect = func(context.Context) Capabilities {
			return Capabilities{
				Browsers: []qaschema.HelloPayloadBrowsersItem{qaschema.HelloPayloadBrowsersItemChromium},
				Agents: []qaschema.AgentCapability{
					{Name: qaschema.AgentCapabilityNameClaude, Ok: true, Version: ptr("1.2.3")},
					{Name: qaschema.AgentCapabilityNameOpencode, Ok: false, Error: ptr("opencode is not on PATH")},
				},
			}
		}
	}
	heartbeat := opts.heartbeat
	if heartbeat <= 0 {
		heartbeat = 50 * time.Millisecond
	}
	backoff := opts.backoff
	if backoff == (Backoff{}) {
		backoff = Backoff{Min: 10 * time.Millisecond, Max: 100 * time.Millisecond, Factor: 2}
	}

	daemon, err := New(cfg, Deps{
		Logger:            testLogger(t),
		Workspaces:        manager,
		State:             state,
		Detect:            detect,
		NewExecutor:       newExec,
		Agent:             opts.agent,
		HTTPClient:        opts.httpClient,
		HeartbeatInterval: heartbeat,
		Backoff:           backoff,
		CancelGrace:       500 * time.Millisecond,
		DialTimeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{
		t: t, backend: backend, daemon: daemon, executor: fake,
		state: state, config: cfg, workspaceRoot: root,
		exited: make(chan struct{}), cancel: cancel,
	}
	go func() {
		h.exitErr = daemon.Run(ctx)
		close(h.exited)
	}()
	t.Cleanup(h.Stop)
	return h
}

// Stop shuts the daemon down and waits for it, so no test leaks a run. It is
// safe to call more than once, which is what lets a test both assert on the
// exit and register the same call as cleanup.
func (h *harness) Stop() {
	h.cancel()
	select {
	case <-h.exited:
	case <-time.After(10 * time.Second):
		h.t.Error("the daemon did not shut down")
	}
}

// WaitExit blocks until Run returns and reports what it returned.
func (h *harness) WaitExit(timeout time.Duration) error {
	h.t.Helper()
	select {
	case <-h.exited:
		return h.exitErr
	case <-time.After(timeout):
		h.t.Fatal("the daemon did not exit")
		return nil
	}
}

func ptr[T any](v T) *T { return &v }
