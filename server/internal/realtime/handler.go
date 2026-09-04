package realtime

import (
	"net/http"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// Handler mounts the two WebSocket routes.
type Handler struct {
	browser  *BrowserHandler
	daemon   *DaemonHandler
	hub      *Hub
	registry *Registry
	store    auth.Store
	sessions *auth.Sessions
}

// NewHandler returns the WebSocket handler.
func NewHandler(hub *Hub, registry *Registry, browser *BrowserHandler, daemon *DaemonHandler, store auth.Store, sessions *auth.Sessions) *Handler {
	return &Handler{browser: browser, daemon: daemon, hub: hub, registry: registry, store: store, sessions: sessions}
}

// Shutdown closes every open socket.
//
// It is registered with http.Server.RegisterOnShutdown. A WebSocket is a
// hijacked connection and http.Server does not track those, so Shutdown
// neither waits for one nor closes it: without this hook a daemon would keep
// holding a socket to a process that is exiting and only discover it on the
// next write. Closing here sends a close frame it reads as "reconnect", which
// is what it already does after any blip.
func (h *Handler) Shutdown() {
	h.registry.CloseAll("the backend is shutting down, reconnect")
	h.hub.CloseAll()
}

// Mount registers both sockets.
//
// Neither route runs behind httpx.Timeout: these connections are meant to live
// for the length of a run, and a request deadline would cut them at thirty
// seconds. The keepalive and the read loop are what bound them instead — see
// conn.go — and the router applies the timeout to the REST routes only.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/ws", httpx.Chain(h.browser, h.browser.Middleware(h.sessions, h.store)...))

	// The daemon plane authenticates with a runtime bearer token, so the org
	// and the runtime are established before the upgrade and nothing on the
	// socket can revise them.
	mux.Handle("GET /api/v1/daemon", httpx.Chain(h.daemon, auth.RequireRuntime(h.store)))
}
