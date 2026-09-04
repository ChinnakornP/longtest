package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/internal/org"
	"github.com/ChinnakornP/longtest/server/internal/project"
	"github.com/ChinnakornP/longtest/server/internal/realtime"
	"github.com/ChinnakornP/longtest/server/internal/report"
	runpkg "github.com/ChinnakornP/longtest/server/internal/run"
	runtimepkg "github.com/ChinnakornP/longtest/server/internal/runtime"
	"github.com/ChinnakornP/longtest/server/internal/testcase"
)

// routerDeps is what the HTTP surface needs. It is a struct rather than a
// growing parameter list because every task adds services to it.
type routerDeps struct {
	Store    *db.Store
	Logger   *slog.Logger
	Config   config
	Auth     *auth.Handler
	Org      *org.Handler
	Project  *project.Handler
	TestCase *testcase.Handler
	Run      *runpkg.Handler
	Runtime  *runtimepkg.Handler
	Report   *report.Handler
	Realtime *realtime.Handler
	Timeout  time.Duration
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
//	Timeout    - bounds REST handlers and, through the context, every query
//
// The WebSocket routes are mounted on a second mux that skips the timeout:
// a control-plane socket is meant to outlive any request deadline, and its
// keepalive bounds it instead.
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
	deps.Project.Mount(mux)
	deps.TestCase.Mount(mux)
	deps.Run.Mount(mux)
	deps.Runtime.Mount(mux)
	deps.Report.Mount(mux)

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

	// The sockets share every middleware except the request timeout.
	root := http.NewServeMux()
	deps.Realtime.Mount(root)
	root.Handle("/", httpx.Chain(mux, httpx.Timeout(timeout)))

	return httpx.Chain(root,
		httpx.RequestID(deps.Logger),
		httpx.Recover(),
		httpx.AccessLog(),
		httpx.SecurityHeaders(),
		httpx.CORS(httpx.CORSConfig{AllowedOrigins: deps.Config.CORSOrigins}),
	)
}

// api is the assembled HTTP surface plus the background loop that goes with
// it. The scheduler is returned rather than started here so main owns its
// lifetime and the tests can drive it by hand.
type api struct {
	Handler   http.Handler
	Scheduler *runpkg.Scheduler
	// Sockets is registered with the server's shutdown hook so long-lived
	// WebSockets do not hold a deploy open for the whole grace period.
	Sockets *realtime.Handler
}

// newAPI wires every service and returns the handler. Splitting it out of main
// is what lets the end-to-end tests in this package exercise the real router
// rather than a hand-assembled subset of it.
func newAPI(store *db.Store, logger *slog.Logger, cfg config) api {
	sessions := auth.NewSessions(store, cfg.SessionCookie)
	hasher := auth.NewHasher(auth.DefaultPasswordParams())

	hub := realtime.NewHub()
	registry := realtime.NewRegistry()
	artifacts := cfg.Artifacts

	orgService := org.NewService(store)
	authService := auth.NewService(store, hasher, sessions, orgService)
	projectService := project.NewService(store)
	testCaseService := testcase.NewService(store)
	runService := runpkg.NewService(store, projectService, hub, registry, artifacts, cfg.Run)
	runtimeService := runtimepkg.NewService(store, registry, cfg.Run.OnlineWithin)
	reportService := report.NewService(store, runService, artifacts)

	scheduler := runpkg.NewScheduler(runService, registry, logger, cfg.Scheduler)
	sockets := realtime.NewHandler(hub, registry,
		realtime.NewBrowserHandler(hub, runService, cfg.CORSOrigins, logger),
		realtime.NewDaemonHandler(registry, runService, logger),
		store, sessions,
	)

	handler := newRouter(routerDeps{
		Store:    store,
		Logger:   logger,
		Config:   cfg,
		Auth:     auth.NewHandler(authService, sessions),
		Org:      org.NewHandler(orgService, store, sessions),
		Project:  project.NewHandler(projectService, testCaseService, store, sessions),
		TestCase: testcase.NewHandler(testCaseService, store, sessions),
		Run:      runpkg.NewHandler(runService, store, sessions),
		Runtime:  runtimepkg.NewHandler(runtimeService, store, sessions),
		Report:   report.NewHandler(reportService, store, sessions),
		Realtime: sockets,
		Timeout:  cfg.RequestTimeout,
	})

	return api{Handler: handler, Scheduler: scheduler, Sockets: sockets}
}
