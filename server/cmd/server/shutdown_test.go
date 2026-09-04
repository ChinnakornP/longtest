package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
	runpkg "github.com/ChinnakornP/longtest/server/internal/run"
)

// A WebSocket is a hijacked connection, and http.Server does not track those:
// Shutdown returns without waiting for one and without closing it, so a daemon
// would be left holding a socket to a process that is about to exit and would
// only find out on the next write. The shutdown hook is what turns that into a
// close frame the daemon reads as "reconnect".
func TestShutdownClosesOpenControlPlaneSockets(t *testing.T) {
	// The pairing flow needs a running API; this one hands out the token.
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	_, token := env.pairedRuntime(t, owner)

	// A second, real http.Server — not httptest, whose Close() closes client
	// connections itself and would hide exactly what is under test.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	assembled := newAPI(authtest.Store(t), logger, config{
		SessionCookie:  authtest.SessionConfig(),
		RequestTimeout: 30 * time.Second,
		Run:            testRunConfig(),
		Scheduler:      runpkg.DefaultSchedulerConfig(),
	})

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: assembled.Handler, ReadHeaderTimeout: 10 * time.Second}
	srv.RegisterOnShutdown(assembled.Sockets.Shutdown)

	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()

	conn, resp, err := websocket.Dial(t.Context(),
		"ws://"+listener.Addr().String()+"/api/v1/daemon",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if status := closeHandshake(resp); err != nil {
		t.Fatalf("dial the control plane: %v (status %d)", err, status)
	}
	defer func() { _ = conn.CloseNow() }()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// The daemon's next read returns the close, promptly, instead of hanging
	// on a socket nobody is serving.
	readCtx, readCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer readCancel()

	_, _, err = conn.Read(readCtx)
	if err == nil {
		t.Fatal("the socket was still open after shutdown")
	}
	if readCtx.Err() != nil {
		t.Fatal("shutdown left the socket open; the daemon would hang on it")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusPolicyViolation {
		t.Fatalf("got close status %v, want a policy-violation close carrying a reason (%v)", status, err)
	}

	if err := <-served; err != nil && err != http.ErrServerClosed { //nolint:errorlint // sentinel identity
		t.Fatalf("serve: %v", err)
	}
}
