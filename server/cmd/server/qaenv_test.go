package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/artifact"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/realtime"
	runpkg "github.com/ChinnakornP/longtest/server/internal/run"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// The harness every contract test in this package runs against.
//
// It builds the REAL api — the same newAPI main calls, the same middleware
// chain, the same scheduler — so a test that passes here passes against what
// ships. Nothing below hand-assembles a subset of the router.

type qaEnv struct {
	*authtest.Env
	scheduler interface{ Wake() }
}

// newQAEnv starts the API and its scheduler for one test.
func newQAEnv(t *testing.T, options ...func(*config)) *qaEnv {
	t.Helper()

	store := authtest.Store(t)
	artifacts, err := artifact.NewService(artifact.Config{
		Endpoint: "http://127.0.0.1:9000",
		Region:   "us-east-1",
		Bucket:   "qa-artifacts",
		// Fixture values. They only ever sign URLs this test inspects; nothing
		// is uploaded. The live round trip is internal/artifact's Live test.
		Credentials: artifact.Credentials{AccessKeyID: "test-access-key", SecretAccessKey: "test-secret-key"},
		PathStyle:   true,
		PresignTTL:  15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("build artifact service: %v", err)
	}

	cfg := config{
		SessionCookie:  authtest.SessionConfig(),
		CORSOrigins:    []string{"http://localhost:3000"},
		RequestTimeout: 30 * time.Second,
		Run:            testRunConfig(),
		Scheduler:      runpkg.DefaultSchedulerConfig(),
		Artifacts:      artifacts,
	}
	for _, apply := range options {
		apply(&cfg)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	assembled := newAPI(store, logger, cfg)

	// The scheduler is what turns "queued" into "assigned"; a test that did not
	// run it would be testing a queue nothing drains.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		assembled.Scheduler.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return &qaEnv{Env: authtest.New(t, assembled.Handler), scheduler: assembled.Scheduler}
}

// testRunConfig is the production default plus the one value a deployment must
// supply: the origin a daemon reaches this API on, which ends up inside every
// run.assign frame as the base of the artifact upload endpoint.
func testRunConfig() runpkg.Config {
	cfg := runpkg.DefaultConfig()
	cfg.PresignBaseURL = "http://127.0.0.1:8080"
	return cfg
}

// wsURL rewrites the test server's origin for a WebSocket dial.
func (e *qaEnv) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(e.Server.URL, "http") + path
}

// --- fixtures -------------------------------------------------------------

// project creates a project through the real endpoint and returns its id.
func (e *qaEnv) project(t *testing.T, client *authtest.Client, baseURL string) uuid.UUID {
	t.Helper()

	resp := client.Post(t, "/api/v1/projects", map[string]string{
		"name":    "project-" + uuid.NewString()[:8],
		"baseUrl": baseURL,
	})
	resp.ExpectStatus(t, http.StatusCreated)

	var view struct {
		ID uuid.UUID `json:"id"`
	}
	resp.JSON(t, &view)
	return view.ID
}

// pairedRuntime walks the real pairing flow — issue a code, redeem it — and
// returns the runtime and the bearer token its daemon will use. Writing the
// rows directly would skip exactly the step that establishes which
// organization the token belongs to.
func (e *qaEnv) pairedRuntime(t *testing.T, admin *authtest.Client) (uuid.UUID, string) {
	t.Helper()

	code := admin.Post(t, "/api/v1/orgs/"+admin.OrgID.String()+"/runtimes/pair", nil)
	code.ExpectStatus(t, http.StatusCreated)
	var issued struct {
		PairingCode string `json:"pairingCode"`
	}
	code.JSON(t, &issued)

	redeemed := e.Anonymous(t).Post(t, "/api/v1/runtimes/redeem", map[string]any{
		"pairingCode": issued.PairingCode,
		"runtimeName": "runtime-" + uuid.NewString()[:8],
		"hostInfo":    map[string]string{"hostname": "test-host", "os": "linux", "arch": "amd64"},
	})
	redeemed.ExpectStatus(t, http.StatusCreated)
	var paired struct {
		RuntimeID    uuid.UUID `json:"runtimeId"`
		RuntimeToken string    `json:"runtimeToken"`
	}
	redeemed.JSON(t, &paired)

	return paired.RuntimeID, paired.RuntimeToken
}

// --- daemon control-plane client ------------------------------------------

// daemonClient is a test double for the QA daemon (LONG-11), speaking the same
// daemon-envelope@1 contract the real one will.
type daemonClient struct {
	conn      *websocket.Conn
	runtimeID uuid.UUID
	seq       int64
}

// dialDaemon opens a control-plane connection with a runtime token.
func (e *qaEnv) dialDaemon(t *testing.T, runtimeID uuid.UUID, token string) *daemonClient {
	t.Helper()

	conn, resp, err := websocket.Dial(t.Context(), e.wsURL("/api/v1/daemon"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	// On a failed upgrade Dial returns the handshake response, and its body
	// carries the error envelope that says why.
	status := closeHandshake(resp)
	if err != nil {
		t.Fatalf("dial daemon control plane: %v (status %d)", err, status)
	}
	conn.SetReadLimit(realtime.MaxFrameBytes)

	client := &daemonClient{conn: conn, runtimeID: runtimeID}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return client
}

// send writes one frame. seq is per-run and monotonic, which is what the
// server deduplicates on.
func (d *daemonClient) send(t *testing.T, typ qaschema.EnvelopeType, runID *uuid.UUID, seq int64, payload any) {
	t.Helper()

	frame, err := realtime.NewFrame(typ, runID, seq, payload)
	if err != nil {
		t.Fatalf("build %s frame: %v", typ, err)
	}
	if err := d.conn.Write(t.Context(), websocket.MessageText, frame); err != nil {
		t.Fatalf("write %s frame: %v", typ, err)
	}
}

// hello sends the capability report a daemon opens with.
func (d *daemonClient) hello(t *testing.T) {
	t.Helper()
	d.send(t, qaschema.EnvelopeTypeHello, nil, d.next(), qaschema.HelloPayload{
		RuntimeID: d.runtimeID.String(),
		Version:   "0.1.0-test",
		Browsers:  []qaschema.HelloPayloadBrowsersItem{qaschema.HelloPayloadBrowsersItemChromium},
		Agents:    []qaschema.AgentCapability{{Name: qaschema.AgentCapabilityNameClaude, Ok: true}},
	})
}

func (d *daemonClient) next() int64 {
	d.seq++
	return d.seq
}

// receive waits for one frame. A timeout is a failure: every test that calls
// this is asserting that the server sends something.
func (d *daemonClient) receive(t *testing.T, within time.Duration) qaschema.Envelope {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()

	typ, raw, err := d.conn.Read(ctx)
	if err != nil {
		t.Fatalf("read a control-plane frame within %s: %v", within, err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("got a %v frame, want text", typ)
	}

	envelope, err := realtime.ParseFrame(raw)
	if err != nil {
		t.Fatalf("the server sent a frame that does not match the contract: %v", err)
	}
	return envelope
}

// expectClosed asserts that the server hung up. It is how a test states "that
// frame was rejected": contract D has no per-frame error channel, so closing
// the connection IS the rejection.
func (d *daemonClient) expectClosed(t *testing.T, within time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()

	for {
		if _, _, err := d.conn.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatalf("the connection was still open after %s", within)
			}
			return
		}
	}
}

// --- browser stream client ------------------------------------------------

type browserClient struct{ conn *websocket.Conn }

// dialBrowser opens a read-only run stream as a signed-in user.
func (e *qaEnv) dialBrowser(t *testing.T, client *authtest.Client, runID uuid.UUID, since string) *browserClient {
	t.Helper()

	path := "/api/v1/ws?runId=" + runID.String() + "&orgId=" + client.OrgID.String()
	if since != "" {
		path += "&since=" + since
	}

	header := http.Header{}
	// The session travels as a cookie, exactly as it does from a browser.
	header.Set("Cookie", authtest.SessionCookieName()+"="+client.SessionToken)

	conn, resp, err := websocket.Dial(t.Context(), e.wsURL(path), &websocket.DialOptions{HTTPHeader: header})
	status := closeHandshake(resp)
	if err != nil {
		t.Fatalf("dial run stream: %v (status %d)", err, status)
	}

	t.Cleanup(func() { _ = conn.CloseNow() })
	return &browserClient{conn: conn}
}

// streamFrame is the browser stream's wire shape, redeclared here so the test
// asserts on the JSON contract rather than on the server's own struct.
type streamFrame struct {
	Type  string          `json:"type"`
	RunID uuid.UUID       `json:"runId"`
	Run   json.RawMessage `json:"run"`
	Event *struct {
		Seq     int64  `json:"seq"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Level   string `json:"level"`
		Phase   string `json:"phase"`
	} `json:"event"`
	LastSeq *int64 `json:"lastSeq"`
}

func (b *browserClient) receive(t *testing.T, within time.Duration) streamFrame {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()

	_, raw, err := b.conn.Read(ctx)
	if err != nil {
		t.Fatalf("read a stream frame within %s: %v", within, err)
	}
	var frame streamFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("stream frame is not decodable: %v\nframe: %s", err, raw)
	}
	return frame
}

// nextEvent skips status frames and returns the next numbered event.
func (b *browserClient) nextEvent(t *testing.T, within time.Duration) streamFrame {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		frame := b.receive(t, time.Until(deadline))
		if frame.Type == "run.event" {
			return frame
		}
	}
	t.Fatalf("no run.event within %s", within)
	return streamFrame{}
}

// quiet asserts that nothing else arrives. It is how the redelivery test states
// "one hundred copies produced one browser event".
func (b *browserClient) quiet(t *testing.T, window time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), window)
	defer cancel()

	for {
		_, raw, err := b.conn.Read(ctx)
		if err != nil {
			return // the deadline passed: nothing more arrived
		}
		var frame streamFrame
		if err := json.Unmarshal(raw, &frame); err == nil && frame.Type != "run.event" {
			continue // a status change is not a duplicate event
		}
		t.Fatalf("an extra frame arrived: %s", raw)
	}
}

// closeHandshake drains and closes a WebSocket handshake response, returning
// its status. On a successful upgrade there is no body to read; on a failure
// there is, and leaking it would hold a connection for the rest of the suite.
func closeHandshake(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return resp.StatusCode
}

// --- test-case fixtures ---------------------------------------------------

// testCaseDocument is a minimal, contract-valid test-case@1 body.
//
// It is validated on the way in, so a fixture that drifts from the contract
// fails the test that uses it rather than being silently stored.
func testCaseDocument(t *testing.T, ref string) json.RawMessage {
	t.Helper()

	document := json.RawMessage(`{
		"version": 1,
		"id": "` + ref + `",
		"name": "Log in with valid credentials",
		"priority": "high",
		"category": "functional",
		"steps": [
			{"action": "navigate", "url": "/login"},
			{"action": "fill", "target": {"ref": "login.email"}, "value": "user@example.test"},
			{"action": "click", "target": {"ref": "login.submit"}}
		],
		"assertions": [
			{"type": "urlMatches", "value": "/dashboard"}
		]
	}`)
	if err := qaschema.MustBeValid("test-case@1", document); err != nil {
		t.Fatalf("the test fixture is not a valid test-case@1 document: %v", err)
	}
	return document
}

// seedTestCase writes a draft case straight to the database.
//
// There is no endpoint that creates one: cases come from the planner, over the
// control plane. Going through a whole planner round trip to get a fixture
// would make every test that needs a case depend on the ingest path as well.
func seedTestCase(t *testing.T, env *qaEnv, client *authtest.Client, projectID uuid.UUID, ref string) uuid.UUID {
	t.Helper()

	created, err := env.Store.CreateTestCase(t.Context(), dbgen.CreateTestCaseParams{
		OrgID:     client.OrgID,
		ProjectID: projectID,
		Ref:       ref,
		Name:      "Log in with valid credentials",
		Priority:  dbgen.TestPriorityHigh,
		Category:  dbgen.TestCategoryFunctional,
		Status:    dbgen.TestCaseStatusDraft,
		Payload:   testCaseDocument(t, ref),
	})
	if err != nil {
		t.Fatalf("seed test case %s: %v", ref, err)
	}
	return created.ID
}

// waitFor polls until condition holds, and fails the test if it never does.
//
// It exists because assignment, hello and heartbeat are asynchronous by design:
// asserting on them with a sleep would be either flaky or slow, and asserting
// immediately would test a race.
func waitFor(t *testing.T, within time.Duration, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the condition did not hold within %s", within)
}
