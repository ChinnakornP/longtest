// Command server is the HTTP/WebSocket backend of the AI QA platform.
//
// It currently serves the authentication and tenancy surface (LONG-7):
//
//	POST   /api/v1/auth/signup
//	POST   /api/v1/auth/login
//	POST   /api/v1/auth/logout
//	GET    /api/v1/me
//	POST   /api/v1/orgs
//	GET    /api/v1/orgs/{orgID}/members
//	POST   /api/v1/orgs/{orgID}/invites
//	GET    /api/v1/orgs/{orgID}/invites
//	DELETE /api/v1/orgs/{orgID}/invites/{inviteID}
//	POST   /api/v1/invites/accept
//	POST   /api/v1/orgs/{orgID}/runtimes/pair
//	POST   /api/v1/runtimes/redeem
//
// The project, run and WebSocket control-plane routes (T08/T09) mount onto the
// same router and behind the same middleware.
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

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: newAPI(store, logger, cfg),
		// Slowloris protection: a client that opens a connection and never
		// finishes its headers must not hold a goroutine indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: the WebSocket control plane (T09) mounts on this
		// server, and a write deadline would cut long-lived connections. The
		// per-request Timeout middleware bounds the REST handlers instead.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

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
	return <-serveErr
}
