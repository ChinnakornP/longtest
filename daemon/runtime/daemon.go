package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ChinnakornP/longtest/daemon/agent"
	"github.com/ChinnakornP/longtest/daemon/browser"
	"github.com/ChinnakornP/longtest/daemon/executor"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/workspace"
)

const (
	// heartbeatInterval is fixed by the task contract at 10 seconds.
	defaultHeartbeatInterval = 10 * time.Second
	// dialTimeout bounds one connection attempt. Longer than a healthy dial
	// needs, short enough that a black-holed route becomes a retry rather than
	// a hang.
	defaultDialTimeout = 15 * time.Second
	// cancelGrace is the budget a cancelled run gets to tear its processes
	// down. The contract requires everything dead within 5 seconds, so the
	// daemon reserves the last second for reporting.
	defaultCancelGrace = 4 * time.Second
	// readLimit bounds one inbound frame. A run.assign carrying 500 test cases
	// and an application map is the largest legitimate frame.
	readLimit = 16 << 20
	// healthyConnection is how long a connection must last before the
	// reconnect backoff is considered recovered.
	healthyConnection = 60 * time.Second
)

// ErrUnauthorized means the backend rejected this runtime's token. It stops
// the daemon instead of being retried: a revoked or mistyped token will not
// start working, and an unbounded retry loop against an auth failure is
// indistinguishable from an attack on the server's side.
var ErrUnauthorized = errors.New("runtime: the backend rejected this runtime token; run `qa-daemon pair` again")

// Capabilities is what this machine can do, as reported in the hello frame.
type Capabilities struct {
	Browsers []qaschema.HelloPayloadBrowsersItem
	Agents   []qaschema.AgentCapability
}

// ExecutorClient is the part of the Node sidecar the run orchestrator uses.
// It is an interface so a run can be tested without Playwright installed.
type ExecutorClient interface {
	SessionOpen(ctx context.Context, params executor.SessionOpenParams) (executor.SessionOpenResult, error)
	RunTestCase(ctx context.Context, params executor.TestcaseRunParams) (qaschema.ExecutionResult, error)
	SessionClose(ctx context.Context) error
	Close(ctx context.Context, grace time.Duration) error
}

// AgentTask is one file-exchange invocation of an AI CLI (ADR-003, contract E).
type AgentTask struct {
	// Agent is the CLI the run asked for; empty means the provider's default.
	Agent qaschema.AgentCapabilityName
	// Phase is the workspace phase whose directory holds the exchange.
	Phase workspace.Phase
	// Dir is that directory. The provider writes prompt.md and its inputs
	// there and reads out.json back from it; it is the agent's blast radius.
	Dir string
	// SchemaID names the contract out.json is validated against, e.g.
	// "application-map@1".
	SchemaID string
	// Inputs are files to place in Dir before the CLI runs, keyed by file
	// name. Large context is passed as files, never inlined into a prompt.
	Inputs map[string][]byte
	RunID  string
	// BaseURL is the application under test.
	BaseURL string
	// FixtureNames are the logins this run can establish, restated to the
	// model. Names only: the values are in the daemon's sealed store and are
	// never rendered into a prompt.
	FixtureNames []string
	// Review is the phase's own gate on a schema-valid answer, run inside the
	// retry loop so its findings become the next attempt's feedback. Nil means
	// the schema is the only bar, which is right for a phase whose output has
	// nothing outside the contract to check it against.
	Review func(output []byte) []string
}

// AgentRunner runs one agent phase and returns the validated out.json.
//
// The daemon owns when an agent runs and what it is given; the provider (T10)
// owns how a particular CLI is launched. A daemon with no provider configured
// reports agent_not_available rather than pretending a phase succeeded.
type AgentRunner interface {
	Run(ctx context.Context, task AgentTask) ([]byte, error)
}

// Deps are the collaborators a Daemon needs. Everything here has a working
// default except the state file and workspace manager, which the command
// builds from config.
type Deps struct {
	Logger     *slog.Logger
	Workspaces *workspace.Manager
	State      *StateFile

	// Detect reports this machine's capabilities. Called on every connect, so
	// installing Chromium does not require a daemon restart.
	Detect func(context.Context) Capabilities
	// NewExecutor starts the sidecar. Replaced in tests.
	NewExecutor func(executor.Options) (ExecutorClient, error)
	// Agent runs AI CLI phases; nil means this runtime cannot do them.
	Agent AgentRunner
	// HTTPClient is used for artifact uploads.
	HTTPClient *http.Client
	// DialOptions overrides the websocket dial options (tests point it at an
	// httptest server).
	DialOptions *websocket.DialOptions

	Now               func() time.Time
	Backoff           Backoff
	HeartbeatInterval time.Duration
	DialTimeout       time.Duration
	CancelGrace       time.Duration
}

// Daemon is the control loop.
type Daemon struct {
	cfg    Config
	deps   Deps
	logger *slog.Logger
	now    func() time.Time

	outbox *outbox
	seq    *seqAllocator
	runs   *runRegistry

	startedAt time.Time

	mu         sync.Mutex
	reconnects int
	completed  int
}

// New builds a daemon from validated config.
func New(cfg Config, deps Deps) (*Daemon, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Workspaces == nil {
		return nil, errors.New("runtime: no workspace manager")
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Detect == nil {
		deps.Detect = DetectCapabilities
	}
	if deps.NewExecutor == nil {
		deps.NewExecutor = func(opts executor.Options) (ExecutorClient, error) { return executor.Start(opts) }
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 10 * time.Minute}
	}
	if deps.HeartbeatInterval <= 0 {
		deps.HeartbeatInterval = defaultHeartbeatInterval
	}
	if deps.DialTimeout <= 0 {
		deps.DialTimeout = defaultDialTimeout
	}
	if deps.CancelGrace <= 0 {
		deps.CancelGrace = defaultCancelGrace
	}
	if deps.Backoff == (Backoff{}) {
		deps.Backoff = DefaultBackoff()
	}

	d := &Daemon{
		cfg:       cfg,
		deps:      deps,
		logger:    deps.Logger,
		now:       deps.Now,
		outbox:    newOutbox(2048),
		seq:       newSeqAllocator(),
		runs:      newRunRegistry(64),
		startedAt: deps.Now(),
	}

	// A provider that can narrate its retries gets wired into the run's event
	// stream. It is opt-in through an interface rather than a Deps field so
	// that a runner which has nothing to say — a mock, a test double — needs
	// to implement nothing.
	if narrator, ok := deps.Agent.(interface {
		AttachEvents(func(runID string, ev agent.Event))
	}); ok {
		narrator.AttachEvents(d.emitAgentEvent)
	}

	return d, nil
}

// DetectCapabilities is the default capability probe: which browsers this
// machine has and which AI CLIs are installed and usable.
func DetectCapabilities(ctx context.Context) Capabilities {
	caps := Capabilities{Agents: agent.Detect(ctx, agent.DetectOptions{})}
	if _, err := browser.Detect(browser.Options{}); err == nil {
		caps.Browsers = []qaschema.HelloPayloadBrowsersItem{qaschema.HelloPayloadBrowsersItemChromium}
	}
	return caps
}

// Run dials the backend and keeps the connection up until ctx is cancelled.
//
// A dropped connection is a normal state, not a failure: the daemon reconnects
// with backoff while its in-flight runs keep going, and the frames they
// produce queue in the outbox until a connection exists to carry them.
func (d *Daemon) Run(ctx context.Context) error {
	backoff := d.deps.Backoff

	// Runs are cancelled by shutdown, not by a lost connection, so they hang
	// off their own context.
	runCtx, stopRuns := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRuns()

	for {
		if err := ctx.Err(); err != nil {
			break
		}

		d.publish(func(s *State) { s.Connection = ConnectionConnecting })
		connectedAt := d.now()

		err := d.serve(ctx, runCtx)

		switch {
		case ctx.Err() != nil:
			// Shutdown, not a failure.
		case errors.Is(err, ErrUnauthorized):
			d.publish(func(s *State) {
				s.Connection = ConnectionStopped
				s.LastError = err.Error()
			})
			d.shutdown(stopRuns)
			return err
		default:
			d.logger.Warn("control-plane connection lost", "error", err,
				"queued", d.outbox.Len(), "activeRuns", d.runs.ActiveCount())
			d.publish(func(s *State) {
				s.Connection = ConnectionOffline
				s.LastError = errorText(err)
				s.Reconnects++
			})
			d.mu.Lock()
			d.reconnects++
			d.mu.Unlock()
		}

		if ctx.Err() != nil {
			break
		}
		if d.now().Sub(connectedAt) >= healthyConnection {
			backoff.Reset()
		}

		wait := backoff.Next()
		d.logger.Info("reconnecting", "in", wait.Round(time.Millisecond), "attempt", backoff.Attempt())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}

	d.shutdown(stopRuns)
	return nil
}

// drain cancels every in-flight run, waits for it to tear its processes down,
// and gives the writer a moment to get the resulting frames onto the still-open
// connection. It is called once from the connection's shutdown watcher and
// again from shutdown; both paths are safe, because cancelling an already
// cancelled run and waiting on an empty registry are both no-ops.
func (d *Daemon) drain() {
	active := d.runs.CancelAll(qaschema.RunCancelPayloadReasonShutdown, "the daemon is shutting down")
	if active > 0 {
		d.logger.Info("cancelling runs for shutdown", "runs", active)
	}
	d.runs.WaitAll(d.deps.CancelGrace + time.Second)

	// A bounded flush: the outbox survives a disconnect, so anything still
	// queued when this gives up is not lost, only late.
	deadline := time.Now().Add(2 * time.Second)
	for d.outbox.Len() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if queued := d.outbox.Len(); queued > 0 {
		d.logger.Warn("shutting down with frames still queued", "frames", queued)
	}
}

// shutdown ends every run and records that this daemon is no longer running.
// A daemon that exits while a Chromium is still up leaves the operator's
// machine in the state this whole package exists to avoid, so the wait is
// synchronous.
func (d *Daemon) shutdown(stopRuns context.CancelFunc) {
	d.drain()
	stopRuns()

	d.publish(func(s *State) {
		s.Connection = ConnectionStopped
		s.ActiveRuns = nil
	})
}

// serve owns one connection from dial to close.
func (d *Daemon) serve(ctx context.Context, runCtx context.Context) error {
	endpoint, err := d.cfg.WebSocketURL()
	if err != nil {
		return err
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, d.deps.DialTimeout)
	defer cancelDial()

	opts := &websocket.DialOptions{}
	if d.deps.DialOptions != nil {
		copied := *d.deps.DialOptions
		opts = &copied
	}
	if opts.HTTPHeader == nil {
		opts.HTTPHeader = http.Header{}
	}
	opts.HTTPHeader.Set("Authorization", "Bearer "+d.cfg.Token)
	opts.HTTPHeader.Set("User-Agent", "qa-daemon/"+Version)

	ws, resp, err := websocket.Dial(dialCtx, endpoint, opts)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("%w (%s)", ErrUnauthorized, resp.Status)
		}
		return fmt.Errorf("runtime: dial %s: %w", endpoint, err)
	}
	ws.SetReadLimit(readLimit)

	conn := newConnection(ws)
	defer func() {
		// Normal closure: the backend logs an abnormal close as an incident,
		// and a reconnect is not one.
		_ = ws.Close(websocket.StatusNormalClosure, "reconnecting")
	}()

	// The pumps outlive ctx by design. When the daemon is asked to stop, the
	// last thing it does is cancel its runs and report what happened to them —
	// and that report needs a connection to travel on. Closing the socket the
	// moment ctx ends would strand every one of those results in the outbox
	// and leave the backend with runs stuck in "running".
	connCtx, closeConn := context.WithCancel(context.WithoutCancel(ctx))
	defer closeConn()

	go func() {
		select {
		case <-connCtx.Done():
		case <-ctx.Done():
			d.drain()
			closeConn()
		}
	}()

	d.seq.ResetRuntime()
	if err := d.sendHello(connCtx, conn); err != nil {
		return err
	}

	d.logger.Info("connected to the control plane", "url", endpoint, "runtimeId", d.cfg.RuntimeID)
	connectedAt := d.now().UTC()
	d.publish(func(s *State) {
		s.Connection = ConnectionOnline
		s.ConnectedAt = &connectedAt
		s.LastError = ""
	})

	// The three pumps share a connection and die together: whichever fails
	// first decides the connection's fate, and the others are unblocked by
	// closing connCtx.
	pumps := []func(context.Context, *connection) error{
		func(ctx context.Context, c *connection) error { return d.readPump(ctx, runCtx, c) },
		d.writePump,
		d.heartbeatPump,
	}
	failures := make(chan error, len(pumps))
	var wg sync.WaitGroup
	for _, pump := range pumps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			failures <- pump(connCtx, conn)
		}()
	}

	first := <-failures
	closeConn()
	_ = ws.Close(websocket.StatusNormalClosure, "closing")
	wg.Wait()
	return first
}

// readPump decodes inbound frames and dispatches them.
func (d *Daemon) readPump(connCtx, runCtx context.Context, conn *connection) error {
	for {
		_, data, err := conn.ws.Read(connCtx)
		if err != nil {
			return fmt.Errorf("runtime: read frame: %w", err)
		}
		if err := d.handleFrame(runCtx, data); err != nil {
			// A frame the daemon cannot act on is logged and skipped: dropping
			// the connection over one malformed message would take every
			// in-flight run's event stream with it.
			d.logger.Warn("ignoring inbound frame", "error", err)
		}
	}
}

// writePump drains the outbox onto the connection.
func (d *Daemon) writePump(connCtx context.Context, conn *connection) error {
	for {
		select {
		case <-connCtx.Done():
			return connCtx.Err()
		case <-d.outbox.Wait():
		}

		for {
			head, ok := d.outbox.Head()
			if !ok {
				break
			}
			data, err := json.Marshal(head)
			if err != nil {
				// Unencodable frames are dropped rather than retried forever;
				// this is a programming error, not a transport one.
				d.logger.Error("dropping unencodable frame", "type", head.Type, "error", err)
				d.outbox.Ack(head.MsgID)
				continue
			}
			if err := conn.write(connCtx, data); err != nil {
				return fmt.Errorf("runtime: write %s: %w", head.Type, err)
			}
			d.outbox.Ack(head.MsgID)
		}
	}
}

// heartbeatPump reports liveness every 10 seconds.
//
// Heartbeats are written straight to the connection rather than queued: a
// heartbeat that was produced while offline says nothing true by the time a
// connection exists.
func (d *Daemon) heartbeatPump(connCtx context.Context, conn *connection) error {
	ticker := time.NewTicker(d.deps.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-connCtx.Done():
			return connCtx.Err()
		case <-ticker.C:
		}

		payload := qaschema.HeartbeatPayload{
			UptimeSec:  int(d.now().Sub(d.startedAt).Seconds()),
			ActiveRuns: d.runs.ActiveIDs(),
		}
		env, err := newEnvelope(qaschema.EnvelopeTypeHeartbeat, nil, d.seq.NextRuntime(), payload, d.now())
		if err != nil {
			return err
		}
		data, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("runtime: encode heartbeat: %w", err)
		}
		if err := conn.write(connCtx, data); err != nil {
			return fmt.Errorf("runtime: write heartbeat: %w", err)
		}
	}
}

func (d *Daemon) sendHello(ctx context.Context, conn *connection) error {
	caps := d.deps.Detect(ctx)

	payload := qaschema.HelloPayload{
		RuntimeID: d.cfg.RuntimeID,
		Version:   Version,
		Browsers:  caps.Browsers,
		Agents:    caps.Agents,
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		payload.Hostname = &name
	}
	if osName := helloOS(); osName != nil {
		payload.OS = osName
	}
	if len(payload.Browsers) == 0 {
		// The contract requires at least one browser. A runtime with no
		// browser is still worth reporting — the UI has to be able to say
		// "this machine cannot execute yet" — so chromium is claimed as the
		// intended engine and doctor is where the missing install shows up.
		payload.Browsers = []qaschema.HelloPayloadBrowsersItem{qaschema.HelloPayloadBrowsersItemChromium}
	}
	if payload.Agents == nil {
		payload.Agents = []qaschema.AgentCapability{}
	}

	env, err := newEnvelope(qaschema.EnvelopeTypeHello, nil, d.seq.NextRuntime(), payload, d.now())
	if err != nil {
		return err
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("runtime: encode hello: %w", err)
	}
	if err := conn.write(ctx, data); err != nil {
		return fmt.Errorf("runtime: send hello: %w", err)
	}
	return nil
}

// handleFrame validates and dispatches one inbound frame.
func (d *Daemon) handleFrame(runCtx context.Context, data []byte) error {
	if err := validateFrame(data); err != nil {
		return err
	}
	var env qaschema.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("runtime: decode frame: %w", err)
	}

	switch env.Type {
	case qaschema.EnvelopeTypeRunAssign:
		return d.handleAssign(runCtx, env)
	case qaschema.EnvelopeTypeRunCancel:
		return d.handleCancel(env)
	case qaschema.EnvelopeTypeHello, qaschema.EnvelopeTypeHeartbeat,
		qaschema.EnvelopeTypeRunEvent, qaschema.EnvelopeTypeRunResult:
		// Daemon-to-server frame types. The backend does not send them; a
		// frame like this is a bug on the other side, not work for us.
		return fmt.Errorf("runtime: unexpected inbound frame type %q", env.Type)
	default:
		return fmt.Errorf("runtime: unknown frame type %q", env.Type)
	}
}

func (d *Daemon) publish(mutate func(*State)) {
	if d.deps.State == nil {
		return
	}
	if err := d.deps.State.Update(mutate); err != nil {
		d.logger.Warn("could not publish daemon state", "error", err)
	}
}

// connection serialises writes onto one websocket.
type connection struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func newConnection(ws *websocket.Conn) *connection { return &connection{ws: ws} }

func (c *connection) write(ctx context.Context, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, data)
}

func helloOS() *qaschema.HelloPayloadOS {
	value := qaschema.HelloPayloadOS(goruntime.GOOS)
	if !value.IsValid() {
		return nil
	}
	return &value
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
