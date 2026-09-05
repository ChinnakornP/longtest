// Package auth_test exercises the auth surface end to end, against a real
// Postgres. It is an external test package so that it can use
// internal/auth/authtest (which imports auth) without an import cycle.
package auth_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/internal/org"
)

func TestMain(m *testing.M) { authtest.Main(m) }

// newAPI builds the routes under test plus a set of probe routes that exercise
// each middleware in isolation.
//
// The probes exist because the middleware contract ("no X-Org-ID is a 403",
// "a viewer cannot write") has to hold for every route T08 adds, not only for
// the handful of endpoints that happen to exist today.
func newAPI(t *testing.T) http.Handler {
	t.Helper()

	store := authtest.Store(t)
	sessions := auth.NewSessions(store, authtest.SessionConfig())
	// Cheap parameters: these tests hash a password on nearly every case, and
	// the cost setting is covered by the unit tests in package auth.
	hasher := auth.NewHasher(auth.FastPasswordParams())

	orgService := org.NewService(store)
	authService := auth.NewService(store, hasher, sessions, orgService)

	mux := http.NewServeMux()
	auth.NewHandler(authService, sessions).Mount(mux)
	org.NewHandler(orgService, store, sessions).Mount(mux)
	mountProbes(mux, store, sessions)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return httpx.Chain(mux, httpx.RequestID(logger), httpx.Recover(), httpx.AccessLog())
}

// probeResponse is what every probe route echoes back, so a test can assert on
// what the middleware actually put into the context.
type probeResponse struct {
	UserID    uuid.UUID `json:"userId"`
	OrgID     uuid.UUID `json:"orgId"`
	Role      auth.Role `json:"role"`
	RuntimeID uuid.UUID `json:"runtimeId"`
}

func mountProbes(mux *http.ServeMux, store auth.Store, sessions *auth.Sessions) {
	var (
		user    = auth.RequireUser(sessions)
		orgMW   = auth.RequireOrg(store)
		runtime = auth.RequireRuntime(store)
	)

	echoScope := httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		scope, err := auth.MustOrgScope(r.Context())
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, r, http.StatusOK, probeResponse{
			UserID: scope.UserID(), OrgID: scope.OrgID(), Role: scope.Role(),
		})
		return nil
	})

	mux.Handle("GET /probe/user", httpx.Chain(httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		caller, ok := auth.CallerFrom(r.Context())
		if !ok {
			return httpx.Unauthorized("no caller")
		}
		httpx.WriteJSON(w, r, http.StatusOK, probeResponse{UserID: caller.UserID()})
		return nil
	}), user))

	mux.Handle("GET /probe/org", httpx.Chain(echoScope, user, orgMW, auth.RequireRole(auth.RoleViewer)))
	mux.Handle("POST /probe/member", httpx.Chain(echoScope, user, orgMW, auth.RequireRole(auth.RoleMember)))
	mux.Handle("POST /probe/admin", httpx.Chain(echoScope, user, orgMW, auth.RequireRole(auth.RoleAdmin)))
	mux.Handle("POST /probe/owner", httpx.Chain(echoScope, user, orgMW, auth.RequireRole(auth.RoleOwner)))

	// A route whose org id also appears in the path, like the real
	// /api/v1/orgs/{orgID}/... routes.
	mux.Handle("GET /probe/orgs/{orgID}/thing",
		httpx.Chain(echoScope, user, orgMW, auth.RequireOrgMatchesPath("orgID"), auth.RequireRole(auth.RoleViewer)))

	// A route that has RequireOrg but no RequireUser above it: it must fail
	// closed rather than serve an unauthenticated request.
	mux.Handle("GET /probe/misconfigured", httpx.Chain(echoScope, orgMW))

	// A handler that asks for a scope on a route with no tenancy middleware at
	// all - MustOrgScope is the last line of defence.
	mux.Handle("GET /probe/unscoped", echoScope)

	mux.Handle("GET /probe/runtime", httpx.Chain(httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		rc, err := auth.MustRuntimeCaller(r.Context())
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, r, http.StatusOK, probeResponse{OrgID: rc.OrgID(), RuntimeID: rc.RuntimeID()})
		return nil
	}), runtime))
}
