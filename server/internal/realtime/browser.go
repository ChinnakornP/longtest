package realtime

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// StreamSource is what the browser handler needs from the domain layer:
// permission to watch a run, and whatever the watcher missed.
// internal/run implements it.
type StreamSource interface {
	// OpenRunStream authorises scope to read runID and returns the frames it
	// has not seen. A run in another organization is a not-found error, never
	// a different one: confirming that a run id exists elsewhere is itself a
	// cross-tenant leak.
	//
	// It takes the auth.OrgScope the middleware resolved, not an org id: the
	// scope cannot be built from anything on the request (ADR-007), so this
	// signature is what stops a future caller from passing the runId's owner
	// instead of the subscriber's organization.
	OpenRunStream(ctx context.Context, scope auth.OrgScope, runID uuid.UUID, since int64) (RunStream, error)
}

// RunStream is the opening state of a browser subscription.
type RunStream struct {
	// Open is the first frame written: a snapshot of the run, so a client that
	// connects to a finished run does not sit waiting for an event that will
	// never come.
	Open []byte
	// Backlog is every event after the client's `since`, in sequence order.
	Backlog []Message
}

// BrowserHandler serves WS /api/v1/ws?runId=…, read-only.
type BrowserHandler struct {
	hub     *Hub
	source  StreamSource
	origins []string
	logger  *slog.Logger
}

// NewBrowserHandler returns the browser stream handler. allowedOrigins is the
// same exact-match allowlist the CORS middleware uses.
func NewBrowserHandler(hub *Hub, source StreamSource, allowedOrigins []string, logger *slog.Logger) *BrowserHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &BrowserHandler{hub: hub, source: source, origins: allowedOrigins, logger: logger}
}

// OrgQueryParam lets a browser select its organization on a WebSocket.
//
// The native WebSocket API cannot set a request header, so X-Org-ID — the ONLY
// source of an org id on every other route (ADR-006) — is unreachable here.
// The middleware still reads the header and nothing else; ServeHTTP copies
// this parameter into it before the chain runs, so there is exactly one place
// where a query string can become an org id and it is this one.
const OrgQueryParam = "orgId"

// Middleware returns the chain this handler must be mounted behind. It is a
// method rather than something the router assembles so the org-header shim
// above cannot be wired up without it.
func (h *BrowserHandler) Middleware(sessions *auth.Sessions, store auth.Store) []httpx.Middleware {
	return []httpx.Middleware{
		h.adoptOrgQueryParam(),
		auth.RequireUser(sessions),
		auth.RequireOrg(store),
		auth.RequireRole(auth.RoleViewer),
	}
}

func (h *BrowserHandler) adoptOrgQueryParam() httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(auth.OrgHeader) == "" {
				if raw := strings.TrimSpace(r.URL.Query().Get(OrgQueryParam)); raw != "" {
					r.Header.Set(auth.OrgHeader, raw)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ServeHTTP authorises the subscription, upgrades, and streams until the
// client goes away or the process shuts down.
func (h *BrowserHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	runID, err := uuid.Parse(strings.TrimSpace(r.URL.Query().Get("runId")))
	if err != nil {
		httpx.WriteError(w, r, httpx.BadRequest("runId must be a uuid"))
		return
	}
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// Subscribe BEFORE reading the backlog. The other order has a hole in it:
	// an event published between the read and the subscribe is in neither.
	// The overlap this creates is removed by the seq filter below.
	sub := h.hub.Subscribe(runID)
	defer sub.Close()

	stream, err := h.source.OpenRunStream(r.Context(), scope, runID, since)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The session is an ambient cookie, so the Origin check is the only
		// thing standing between this stream and any page the user visits:
		// WebSocket upgrades are not subject to the CORS policy that guards
		// every other route.
		OriginPatterns:  h.origins,
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		h.logger.WarnContext(r.Context(), "browser websocket upgrade failed", "err", err)
		return
	}
	c := newConn(ws)

	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	go c.keepalive(ctx)
	// A read-only stream still has to read: it is how a close frame and a
	// disconnected client are noticed at all.
	go drain(ctx, c, cancel)

	h.stream(ctx, c, sub, stream)
}

func (h *BrowserHandler) stream(ctx context.Context, c *conn, sub *Subscription, stream RunStream) {
	highest := NoSequence
	write := func(msg Message) bool {
		if msg.Seq != NoSequence && msg.Seq <= highest {
			// Already replayed from the backlog. This is the overlap that
			// subscribing before reading deliberately creates.
			return true
		}
		if err := c.send(ctx, msg.Frame); err != nil {
			return false
		}
		if msg.Seq > highest {
			highest = msg.Seq
		}
		return true
	}

	if !write(Message{Seq: NoSequence, Frame: stream.Open}) {
		return
	}
	for _, msg := range stream.Backlog {
		if !write(msg) {
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			c.closeNormal("server shutting down")
			return
		case <-sub.Lagged():
			// The client fell behind far enough that its stream would have a
			// hole. TryAgainLater is the signal to reconnect with ?since.
			c.closeWith(websocket.StatusTryAgainLater, "stream fell behind, reconnect with ?since")
			return
		case msg, ok := <-sub.C():
			if !ok {
				c.closeNormal("stream closed")
				return
			}
			if !write(msg) {
				return
			}
		}
	}
}

// drain reads and discards client frames. A read-only stream has no inbound
// messages, but without a reader a close frame is never observed and the
// connection lingers until the keepalive notices.
func drain(ctx context.Context, c *conn, cancel context.CancelFunc) {
	defer cancel()
	for {
		if _, _, err := c.ws.Read(ctx); err != nil {
			return
		}
	}
}

// parseSince reads the resume cursor. Absent means "from the beginning";
// events are numbered from 0, so the sentinel for that is -1.
func parseSince(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return NoSequence, nil
	}
	since, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || since < NoSequence {
		return 0, httpx.BadRequest("since must be a sequence number")
	}
	return since, nil
}
