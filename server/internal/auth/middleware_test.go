package auth_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
)

// Acceptance criterion 2: a request with no X-Org-ID, or with an organization
// the caller is not a member of, is a 403 in every case.
func TestRequireOrgRejectsEverythingButRealMembership(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	member := env.NewOrg(t)
	stranger := env.NewOrg(t) // a second tenant, with its own owner

	tests := []struct {
		name   string
		client *authtest.Client
	}{
		{"no X-Org-ID header at all", member.WithoutOrg()},
		{"an organization the caller does not belong to", member.AsOrg(stranger.OrgID)},
		{"an organization that does not exist", member.AsOrg(uuid.New())},
		{"the nil uuid", member.AsOrg(uuid.UUID{})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.client.Get(t, "/probe/org").ExpectError(t, http.StatusForbidden, "forbidden")
		})
	}

	// The happy path still works, so the test above is not passing vacuously.
	var scope probeResponse
	member.Get(t, "/probe/org").ExpectStatus(t, http.StatusOK).JSON(t, &scope)
	if scope.OrgID != member.OrgID || scope.UserID != member.UserID {
		t.Fatalf("scope: got org=%s user=%s, want org=%s user=%s",
			scope.OrgID, scope.UserID, member.OrgID, member.UserID)
	}
	if scope.Role != auth.RoleOwner {
		t.Fatalf("role: got %q, want owner", scope.Role)
	}
}

func TestRequireOrgRejectsAMalformedHeader(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	member := env.NewOrg(t)

	for _, value := range []string{"not-a-uuid", "1 OR 1=1", "  "} {
		t.Run("header="+value, func(t *testing.T) {
			resp := member.WithoutOrg().DoWithHeaders(t, http.MethodGet, "/probe/org", nil,
				map[string]string{auth.OrgHeader: value})
			resp.ExpectError(t, http.StatusForbidden, "forbidden")
		})
	}
}

// Acceptance criterion 6: a viewer cannot reach a route that writes.
func TestRequireRole(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	viewer := env.NewMember(t, owner.OrgID, auth.RoleViewer)
	member := env.NewMember(t, owner.OrgID, auth.RoleMember)
	admin := env.NewMember(t, owner.OrgID, auth.RoleAdmin)

	tests := []struct {
		route  string
		method string
		allow  map[auth.Role]bool
	}{
		{"/probe/org", http.MethodGet, map[auth.Role]bool{
			auth.RoleViewer: true, auth.RoleMember: true, auth.RoleAdmin: true, auth.RoleOwner: true,
		}},
		{"/probe/member", http.MethodPost, map[auth.Role]bool{
			auth.RoleViewer: false, auth.RoleMember: true, auth.RoleAdmin: true, auth.RoleOwner: true,
		}},
		{"/probe/admin", http.MethodPost, map[auth.Role]bool{
			auth.RoleViewer: false, auth.RoleMember: false, auth.RoleAdmin: true, auth.RoleOwner: true,
		}},
		{"/probe/owner", http.MethodPost, map[auth.Role]bool{
			auth.RoleViewer: false, auth.RoleMember: false, auth.RoleAdmin: false, auth.RoleOwner: true,
		}},
	}

	clients := map[auth.Role]*authtest.Client{
		auth.RoleViewer: viewer,
		auth.RoleMember: member,
		auth.RoleAdmin:  admin,
		auth.RoleOwner:  owner,
	}

	for _, tt := range tests {
		for role, client := range clients {
			t.Run(tt.route+"/"+string(role), func(t *testing.T) {
				resp := client.Do(t, tt.method, tt.route, nil)
				if tt.allow[role] {
					resp.ExpectStatus(t, http.StatusOK)
					return
				}
				resp.ExpectError(t, http.StatusForbidden, "forbidden")
			})
		}
	}
}

// The org id in a path is an assertion, never a source. A caller whose header
// says org A cannot operate on org B by changing the URL.
func TestRequireOrgMatchesPath(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)
	other := env.NewOrg(t)

	owner.Get(t, "/probe/orgs/"+owner.OrgID.String()+"/thing").ExpectStatus(t, http.StatusOK)

	// Header says the caller's own org, path names somebody else's.
	owner.Get(t, "/probe/orgs/"+other.OrgID.String()+"/thing").
		ExpectError(t, http.StatusForbidden, "forbidden")

	// Header says another org the caller is not in: rejected before the path
	// is even considered.
	owner.AsOrg(other.OrgID).Get(t, "/probe/orgs/"+other.OrgID.String()+"/thing").
		ExpectError(t, http.StatusForbidden, "forbidden")

	owner.Get(t, "/probe/orgs/not-a-uuid/thing").ExpectError(t, http.StatusBadRequest, "bad_request")
}

// A route wired without RequireUser, or a handler that asks for a scope on a
// route with no tenancy middleware, must fail closed.
func TestTenancyMiddlewareFailsClosedWhenMisconfigured(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	// RequireOrg with no RequireUser above it.
	owner.Get(t, "/probe/misconfigured").ExpectError(t, http.StatusUnauthorized, "unauthorized")
	// MustOrgScope on a route with no middleware at all.
	owner.Get(t, "/probe/unscoped").ExpectError(t, http.StatusForbidden, "forbidden")
}

func TestRequireUserRejectsBadSessions(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	tests := []struct {
		name   string
		client *authtest.Client
	}{
		{"no cookie", env.Anonymous(t)},
		{"a forged token", env.WithSession(t, "not-a-real-session-token")},
		{"an empty token", env.WithSession(t, "")},
		{"a very long token", env.WithSession(t, strings.Repeat("a", 600))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.client.Get(t, "/probe/user").ExpectError(t, http.StatusUnauthorized, "unauthorized")
		})
	}

	owner.Get(t, "/probe/user").ExpectStatus(t, http.StatusOK)
}

func TestExpiredSessionIsRejected(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	owner.Get(t, "/probe/user").ExpectStatus(t, http.StatusOK)

	// Expire the row behind the cookie. The filter is in SQL, so this is the
	// only thing that has to change for the session to stop working.
	tag, err := env.Store.Pool().Exec(t.Context(),
		`UPDATE sessions SET expires_at = now() - interval '1 second' WHERE user_id = $1`, owner.UserID)
	if err != nil {
		t.Fatalf("expire session: %v", err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatal("no session row to expire")
	}

	owner.Get(t, "/probe/user").ExpectError(t, http.StatusUnauthorized, "unauthorized")
}

// Acceptance criterion 5: a forged or revoked runtime token cannot connect.
func TestRequireRuntime(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	runtime, token := newRuntimeWithToken(t, env, owner.OrgID, "runtime-"+uuid.NewString())

	t.Run("a live token authenticates as its own runtime", func(t *testing.T) {
		var got probeResponse
		env.Anonymous(t).WithBearer(token).Get(t, "/probe/runtime").
			ExpectStatus(t, http.StatusOK).JSON(t, &got)

		// The org and runtime come from the token row. Nothing the daemon
		// sends is consulted, which is what makes a forged `hello` harmless.
		if got.OrgID != owner.OrgID || got.RuntimeID != runtime.ID {
			t.Fatalf("got org=%s runtime=%s, want org=%s runtime=%s",
				got.OrgID, got.RuntimeID, owner.OrgID, runtime.ID)
		}
	})

	rejected := []struct {
		name  string
		token string
	}{
		{"no token", ""},
		{"a forged token", "qart_" + uuid.NewString()},
		{"a token from another namespace", "Bearer-looking-nonsense"},
		{"the runtime id itself", runtime.ID.String()},
		{"a truncated token", token[:len(token)-1]},
	}

	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			client := env.Anonymous(t)
			if tt.token != "" {
				client = client.WithBearer(tt.token)
			}
			client.Get(t, "/probe/runtime").ExpectError(t, http.StatusUnauthorized, "unauthorized")
		})
	}

	t.Run("a revoked token stops working", func(t *testing.T) {
		if _, err := env.Store.RevokeRuntimeTokensForRuntime(t.Context(),
			dbgen.RevokeRuntimeTokensForRuntimeParams{OrgID: owner.OrgID, RuntimeID: runtime.ID}); err != nil {
			t.Fatalf("revoke runtime tokens: %v", err)
		}
		env.Anonymous(t).WithBearer(token).Get(t, "/probe/runtime").
			ExpectError(t, http.StatusUnauthorized, "unauthorized")
	})

	t.Run("a disabled runtime cannot connect with a live token", func(t *testing.T) {
		disabled, disabledToken := newRuntimeWithToken(t, env, owner.OrgID, "disabled-"+uuid.NewString())
		if _, err := env.Store.SetRuntimeDisabled(t.Context(), dbgen.SetRuntimeDisabledParams{
			OrgID: owner.OrgID, ID: disabled.ID, Disabled: true,
		}); err != nil {
			t.Fatalf("disable runtime: %v", err)
		}
		env.Anonymous(t).WithBearer(disabledToken).Get(t, "/probe/runtime").
			ExpectError(t, http.StatusUnauthorized, "unauthorized")
	})
}

// A runtime token is not a session, and a session is not a runtime token:
// neither credential may be used on the other's routes.
func TestCredentialsAreNotInterchangeable(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)
	_, token := newRuntimeWithToken(t, env, owner.OrgID, "runtime-"+uuid.NewString())

	// A runtime token in the Authorization header does not sign a user in.
	env.Anonymous(t).WithBearer(token).Get(t, "/probe/user").
		ExpectError(t, http.StatusUnauthorized, "unauthorized")
	// A user session does not authenticate a daemon.
	owner.Get(t, "/probe/runtime").ExpectError(t, http.StatusUnauthorized, "unauthorized")
	// Nor does a session token pasted into the Authorization header.
	env.Anonymous(t).WithBearer(owner.SessionToken).Get(t, "/probe/runtime").
		ExpectError(t, http.StatusUnauthorized, "unauthorized")
}

// newRuntimeWithToken creates a runtime and a live token for it, the way
// redeeming a pairing code would.
func newRuntimeWithToken(t *testing.T, env *authtest.Env, orgID uuid.UUID, name string) (dbgen.Runtime, string) {
	t.Helper()

	runtime, err := env.Store.CreateRuntime(t.Context(), dbgen.CreateRuntimeParams{
		OrgID: orgID, Name: name, Version: "0.0.0-test", HostInfo: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}

	token, hash, err := auth.NewRuntimeToken()
	if err != nil {
		t.Fatalf("new runtime token: %v", err)
	}
	if _, err := env.Store.CreateRuntimeToken(t.Context(), dbgen.CreateRuntimeTokenParams{
		OrgID: orgID, RuntimeID: runtime.ID, TokenHash: hash,
	}); err != nil {
		t.Fatalf("create runtime token: %v", err)
	}
	return runtime, token
}

// Sessions record activity, but not on every request: the touch is what turns
// an authenticated GET into a write, so it is rate-limited by design.
func TestSessionActivityIsRecordedOnce(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	owner.Get(t, "/probe/user").ExpectStatus(t, http.StatusOK)
	first := lastUsedAt(t, env, owner.UserID)
	if !first.Valid {
		t.Fatal("the first authenticated request did not record activity")
	}

	owner.Get(t, "/probe/user").ExpectStatus(t, http.StatusOK)
	second := lastUsedAt(t, env, owner.UserID)
	if !second.Time.Equal(first.Time) {
		t.Fatal("last_used_at was written twice inside the touch interval")
	}
}

func lastUsedAt(t *testing.T, env *authtest.Env, userID uuid.UUID) pgtype.Timestamptz {
	t.Helper()

	var out pgtype.Timestamptz
	err := env.Store.Pool().QueryRow(t.Context(),
		`SELECT last_used_at FROM sessions WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`,
		userID).Scan(&out)
	if err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	return out
}
