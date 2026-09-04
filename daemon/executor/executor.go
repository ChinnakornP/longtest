// Package executor is the Go side of the browser execution engine.
//
// The engine itself is a Node sidecar (see ./package.json and ./src) that this
// package spawns and talks to over line-delimited JSON-RPC on stdio (ADR-001);
// Go never drives Playwright directly. This package owns the sidecar's
// lifecycle — including killing its whole process tree, because the sidecar
// forks Chromium and a run cancel that leaves a browser behind is a leak the
// operator sees as a stuck machine.
package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/proc"
)

// ProtocolVersion is the wire version this client speaks. The sidecar reports
// its own in session.open; a mismatch is a hard error rather than a hopeful
// call, because a method skew shows up as a silently skipped assertion.
const ProtocolVersion = 1

// maxFrameBytes bounds one JSON-RPC line. An execution result with 500 steps
// and its evidence index is large but nowhere near this; a line past it means
// the sidecar is streaming something it should have written to a file.
const maxFrameBytes = 32 << 20

// DefaultCommand is the sidecar invocation when config names none.
var DefaultCommand = []string{"qa-executor"}

// Error is a failure the sidecar reported. Code is one of its stable
// RpcErrorCode values, which the daemon maps to a run error.
type Error struct {
	Code    string
	Message string
	Data    map[string]any
}

func (e *Error) Error() string { return fmt.Sprintf("executor: %s: %s", e.Code, e.Message) }

// ErrClosed is returned by a call made after the sidecar exited.
var ErrClosed = errors.New("executor: sidecar is not running")

// Event is an unsolicited frame: a step landed, a screenshot was saved.
type Event struct {
	Event string         `json:"event"`
	Data  map[string]any `json:"data"`
}

// Options configure a sidecar process.
type Options struct {
	// Command is the sidecar argv. Empty means DefaultCommand.
	Command []string
	// Dir is the working directory, normally the run's execution workspace.
	Dir string
	// Env replaces the environment when non-nil.
	Env []string
	// Logger receives sidecar stderr and protocol-level diagnostics.
	Logger *slog.Logger
	// OnEvent receives sidecar events. It is called from the read loop, so it
	// must not block for long and must not call back into the client.
	OnEvent func(Event)
	// CallTimeout bounds a single request when the caller's context has no
	// deadline of its own. Zero means 10 minutes: a test case can legitimately
	// take minutes, and the run-level context is the real bound.
	CallTimeout time.Duration
}

// Client is a running sidecar.
type Client struct {
	cmd     *proc.Cmd
	enc     *json.Encoder
	logger  *slog.Logger
	onEvent func(Event)
	timeout time.Duration

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcResponse
	closed  bool
	readErr error

	readDone chan struct{}
}

type rpcResponse struct {
	result json.RawMessage
	err    *Error
}

// Start launches the sidecar and begins reading its stdout.
func Start(opts Options) (*Client, error) {
	command := opts.Command
	if len(command) == 0 {
		command = DefaultCommand
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	timeout := opts.CallTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	stderr := &lineLogger{logger: logger}
	cmd, err := proc.Start(proc.Options{
		Name:   command[0],
		Args:   command[1:],
		Dir:    opts.Dir,
		Env:    opts.Env,
		Stderr: stderr,
		Pipe:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("executor: start sidecar: %w", err)
	}

	c := &Client{
		cmd:      cmd,
		enc:      json.NewEncoder(cmd.Stdin()),
		logger:   logger,
		onEvent:  opts.OnEvent,
		timeout:  timeout,
		pending:  make(map[int]chan rpcResponse),
		readDone: make(chan struct{}),
	}
	go c.readLoop(cmd.Stdout())
	return c, nil
}

// PID is the sidecar's process id, for logs and for a cancel that has to
// report what it killed.
func (c *Client) PID() int { return c.cmd.PID() }

// SessionOpenParams opens a browser context pointed at the app under test.
type SessionOpenParams struct {
	BaseURL                   string `json:"baseUrl"`
	StorageState              any    `json:"storageState,omitempty"`
	Locale                    string `json:"locale,omitempty"`
	TimezoneID                string `json:"timezoneId,omitempty"`
	DefaultStepTimeoutMs      int    `json:"defaultStepTimeoutMs,omitempty"`
	DefaultAssertionTimeoutMs int    `json:"defaultAssertionTimeoutMs,omitempty"`
}

// SessionOpenResult is what the sidecar reports back.
type SessionOpenResult struct {
	SessionID       string `json:"sessionId"`
	BaseURL         string `json:"baseUrl"`
	StorageState    any    `json:"storageState"`
	ProtocolVersion int    `json:"protocolVersion"`
}

// TestcaseRunParams is one test case plus where its evidence goes.
type TestcaseRunParams struct {
	TestCase qaschema.TestCase `json:"testCase"`
	// AppMap is not a pointer: the sidecar requires the field, and a run that
	// reached execution always has a map (assigned or discovered).
	AppMap             qaschema.ApplicationMap `json:"appMap"`
	ArtifactDir        string                  `json:"artifactDir"`
	StorageKeyPrefix   string                  `json:"storageKeyPrefix"`
	RunID              string                  `json:"runId,omitempty"`
	StepTimeoutMs      int                     `json:"stepTimeoutMs,omitempty"`
	AssertionTimeoutMs int                     `json:"assertionTimeoutMs,omitempty"`
}

// SessionOpen starts a browser session.
func (c *Client) SessionOpen(ctx context.Context, params SessionOpenParams) (SessionOpenResult, error) {
	var out SessionOpenResult
	if err := c.Call(ctx, "session.open", params, &out); err != nil {
		return SessionOpenResult{}, err
	}
	if out.ProtocolVersion != ProtocolVersion {
		return out, fmt.Errorf("executor: sidecar speaks protocol %d, this daemon speaks %d",
			out.ProtocolVersion, ProtocolVersion)
	}
	return out, nil
}

// RunTestCase executes one case and returns its execution-result document.
func (c *Client) RunTestCase(ctx context.Context, params TestcaseRunParams) (qaschema.ExecutionResult, error) {
	var out qaschema.ExecutionResult
	if err := c.Call(ctx, "testcase.run", params, &out); err != nil {
		return qaschema.ExecutionResult{}, err
	}
	return out, nil
}

// SessionClose closes the browser session but leaves the sidecar running.
func (c *Client) SessionClose(ctx context.Context) error {
	return c.Call(ctx, "session.close", nil, nil)
}

// Call sends one request and waits for its response.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	id, reply, err := c.register()
	if err != nil {
		return err
	}
	defer c.forget(id)

	request := struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{ID: id, Method: method, Params: params}

	c.writeMu.Lock()
	err = c.enc.Encode(request)
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("executor: send %s: %w", method, err)
	}

	select {
	case resp := <-reply:
		if resp.err != nil {
			return resp.err
		}
		if result == nil || len(resp.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(resp.result, result); err != nil {
			return fmt.Errorf("executor: decode %s result: %w", method, err)
		}
		return nil
	case <-c.readDone:
		return c.exitError(method)
	case <-ctx.Done():
		return fmt.Errorf("executor: %s: %w", method, ctx.Err())
	}
}

// Close ends the session politely and then tears down the process tree.
//
// The grace budget is deliberately short: Close is on the cancel path, where
// the daemon has five seconds in total to leave nothing running.
func (c *Client) Close(ctx context.Context, grace time.Duration) error {
	// Best-effort: a sidecar that is wedged mid-step will not answer, and the
	// process-group kill below is what actually guarantees teardown.
	closeCtx, cancel := context.WithTimeout(ctx, min(grace, 2*time.Second))
	if err := c.SessionClose(closeCtx); err != nil {
		c.logger.Debug("session.close before teardown failed", "error", err)
	}
	cancel()

	err := c.cmd.Terminate(ctx, grace)
	<-c.readDone
	return err
}

func (c *Client) register() (int, chan rpcResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, nil, ErrClosed
	}
	c.nextID++
	id := c.nextID
	reply := make(chan rpcResponse, 1)
	c.pending[id] = reply
	return id, reply, nil
}

func (c *Client) forget(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, id)
}

func (c *Client) exitError(method string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return fmt.Errorf("executor: %s: %w", method, c.readErr)
	}
	return fmt.Errorf("executor: %s: %w", method, ErrClosed)
}

// readLoop decodes one frame per line and routes it: a frame with an id is a
// response, a frame with an event name is progress.
func (c *Client) readLoop(stdout io.Reader) {
	defer close(c.readDone)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var frame struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Data    map[string]any `json:"data"`
			} `json:"error"`
			Event string         `json:"event"`
			Data  map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			c.logger.Warn("unparsable executor frame", "error", err)
			continue
		}

		switch {
		case frame.Event != "":
			if c.onEvent != nil {
				c.onEvent(Event{Event: frame.Event, Data: frame.Data})
			}
		case frame.ID != nil:
			resp := rpcResponse{result: frame.Result}
			if frame.Error != nil {
				resp.err = &Error{Code: frame.Error.Code, Message: frame.Error.Message, Data: frame.Error.Data}
			}
			c.deliver(*frame.ID, resp)
		default:
			c.logger.Warn("executor frame has neither id nor event")
		}
	}

	err := scanner.Err()
	c.mu.Lock()
	c.closed = true
	c.readErr = err
	pending := c.pending
	c.pending = map[int]chan rpcResponse{}
	c.mu.Unlock()

	// Everything still waiting has to be released, or a cancelled run's
	// goroutine would block until its context expires.
	for _, reply := range pending {
		reply <- rpcResponse{err: &Error{Code: "INTERNAL", Message: "sidecar exited before answering"}}
	}
}

func (c *Client) deliver(id int, resp rpcResponse) {
	c.mu.Lock()
	reply, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		// A response to a call that already gave up (its context expired).
		c.logger.Debug("executor answered an abandoned request", "id", id)
		return
	}
	reply <- resp
}

// lineLogger forwards sidecar stderr to the run log one line at a time.
// stderr is a debugging channel, never the source of truth (ADR-003).
type lineLogger struct {
	logger *slog.Logger
	buf    []byte
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		idx := strings.IndexByte(string(l.buf), '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimSpace(string(l.buf[:idx]))
		l.buf = l.buf[idx+1:]
		if line != "" {
			l.logger.Debug("executor stderr", "line", line)
		}
	}
	// A sidecar that never emits a newline must not grow this buffer forever.
	if len(l.buf) > 64*1024 {
		l.buf = l.buf[:0]
	}
	return len(p), nil
}
