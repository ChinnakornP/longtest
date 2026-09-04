// Command server is the HTTP/WebSocket backend of the AI QA platform.
//
// The full route-by-route reference is docs/api/openapi.yaml, which a test in
// this package checks against the router. In outline:
//
//	auth/tenancy   /api/v1/auth/*, /api/v1/me, /api/v1/orgs/**, /api/v1/invites/*
//	projects       /api/v1/projects[/{id}[/appmap|/test-cases]]
//	test cases     /api/v1/test-cases/{id}            GET, PATCH
//	runs           /api/v1/runs[/{id}[/cancel|/events|/report|/artifacts/presign]]
//	runtimes       /api/v1/runtimes, /api/v1/runtimes/redeem
//	streams        WS /api/v1/ws?runId=…   browser, read-only
//	               WS /api/v1/daemon       runtime control plane
//
// The process runs one background loop next to the server: the run scheduler,
// which claims queued runs and hands them to connected daemons. Both stop on
// the same signal.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ChinnakornP/longtest/server/internal/db"
	pgdb "github.com/ChinnakornP/longtest/server/pkg/db"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		// The DSN never reaches this line unredacted: pkg/db redacts it at the
		// point every error that mentions it is created.
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	// Bound the start-up connect so a wrong DSN fails the process instead of
	// hanging a deployment.
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pool, err := pgdb.NewPool(connectCtx, cfg.DatabaseURL, pgdb.DefaultPoolConfig())
	if err != nil {
		return err
	}
	defer pool.Close()

	store := db.NewStore(pool)

	assembled := newAPI(store, logger, cfg)

	// The scheduler is the process's only background loop. It is started with
	// the process context, so it stops when a signal arrives and there is no
	// path where it outlives the server.
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		assembled.Scheduler.Run(ctx)
	}()

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: assembled.Handler,
		// Slowloris protection: a client that opens a connection and never
		// finishes its headers must not hold a goroutine indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: the WebSocket control plane (T09) mounts on this
		// server, and a write deadline would cut long-lived connections. The
		// per-request Timeout middleware bounds the REST handlers instead.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	// A WebSocket is a hijacked connection, which Shutdown neither waits for
	// nor closes. Without this the daemons would be left holding sockets to a
	// process that is exiting.
	srv.RegisterOnShutdown(assembled.Sockets.Shutdown)

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("server listening",
			"addr", cfg.Addr,
			"cors_origins", cfg.CORSOrigins,
			"session_cookie_secure", cfg.SessionCookie.Secure,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("listen on %s: %w", cfg.Addr, err)
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	// Shutdown gets a context of its own: ctx is already cancelled, which is
	// why we are here.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownGrace)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}
	<-schedulerDone
	return <-serveErr
}
