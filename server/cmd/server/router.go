package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/internal/org"
)

// routerDeps is what the HTTP surface needs. It is a struct rather than a
// growing parameter list because T08 adds four more services to it.
type routerDeps struct {
	Store   *db.Store
	Logger  *slog.Logger
	Config  config
	Auth    *auth.Handler
	Org     *org.Handler
	Timeout time.Duration
}

// newRouter assembles the whole HTTP surface.
//
// Route-level middleware (session, organization, role) is attached by each
// package's Mount, because which role a route needs is knowledge that belongs
// next to the route. What is assembled here is only the process-wide chain
// every request runs through, in this order:
//
//	RequestID  - so everything below it can log one
//	Recover    - so a panic below it is a 500, not a dropped connection
//	AccessLog  - so the line it writes reports the status the client saw
//	SecurityHeaders / CORS
//	Timeout    - bounds the handler and, through the context, every query
func newRouter(deps routerDeps) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /healthz", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		httpx.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
		return nil
	}))

	// readyz reports whether this process can actually serve: a backend that
	// cannot reach Postgres is live but useless, and a load balancer needs to
	// tell those apart.
	mux.Handle("GET /readyz", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		if err := deps.Store.Pool().Ping(r.Context()); err != nil {
			// The DSN is not echoed: pool errors already carry a redacted one.
			return httpx.AsError(err).WithCause(err)
		}
		httpx.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
		return nil
	}))

	deps.Auth.Mount(mux)
	deps.Org.Mount(mux)

	// Unknown paths get the same envelope as everything else. Note that a
	// known path with the wrong method is answered by net/http's ServeMux with
	// its own plain-text 405; that is a client bug rather than a case the web
	// app handles, so it is left alone.
	mux.Handle("/", httpx.Handler(func(_ http.ResponseWriter, r *http.Request) error {
		return httpx.NotFound("no route for %s %s", r.Method, r.URL.Path)
	}))

	timeout := deps.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return httpx.Chain(mux,
		httpx.RequestID(deps.Logger),
		httpx.Recover(),
		httpx.AccessLog(),
		httpx.SecurityHeaders(),
		httpx.CORS(httpx.CORSConfig{AllowedOrigins: deps.Config.CORSOrigins}),
		httpx.Timeout(timeout),
	)
}

// newAPI wires the services and returns the handler. Splitting it out of main
// is what lets the end-to-end tests in this package exercise the real router
// rather than a hand-assembled subset of it.
func newAPI(store *db.Store, logger *slog.Logger, cfg config) http.Handler {
	sessions := auth.NewSessions(store, cfg.SessionCookie)
	hasher := auth.NewHasher(auth.DefaultPasswordParams())

	orgService := org.NewService(store)
	authService := auth.NewService(store, hasher, sessions, orgService)

	return newRouter(routerDeps{
		Store:   store,
		Logger:  logger,
		Config:  cfg,
		Auth:    auth.NewHandler(authService, sessions),
		Org:     org.NewHandler(orgService, store, sessions),
		Timeout: cfg.RequestTimeout,
	})
}
