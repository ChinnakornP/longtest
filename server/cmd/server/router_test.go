package main

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
)

// These tests drive the REAL router - the same newAPI main calls - so the
// acceptance criteria are checked against the wiring that ships, not against a
// hand-assembled subset of it.
func TestMain(m *testing.M) { authtest.Main(m) }

func newTestEnv(t *testing.T) *authtest.Env {
	t.Helper()

	store := authtest.Store(t)
	cfg := config{
		SessionCookie:  authtest.SessionConfig(),
		CORSOrigins:    []string{"http://localhost:3000"},
		RequestTimeout: 30 * time.Second,
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return authtest.New(t, newAPI(store, logger, cfg))
}

func TestHealthAndReadiness(t *testing.T) {
	env := newTestEnv(t)
	client := env.Anonymous(t)

	client.Get(t, "/healthz").ExpectStatus(t, http.StatusOK)
	// readyz actually reaches Postgres, so it is the check a load balancer can
	// trust to mean "this process can serve".
	client.Get(t, "/readyz").ExpectStatus(t, http.StatusOK)
}

// An unknown path gets the same envelope as everything else, so a client only
// ever has to parse one error shape.
func TestUnknownRouteUsesTheErrorEnvelope(t *testing.T) {
	env := newTestEnv(t)
	env.Anonymous(t).Get(t, "/api/v1/nope").ExpectError(t, http.StatusNotFound, "not_found")
}

// The whole story the product needs on day one: sign up, invite a colleague,
// they join, an admin pairs a daemon, and the daemon authenticates with the
// token it was given.
func TestFullTenancyJourney(t *testing.T) {
	env := newTestEnv(t)

	// 1. An owner signs up and gets an organization.
	founder := env.Anonymous(t)
	founderEmail := "founder-" + uuid.NewString() + "@example.test"

	var signedUp struct {
		Org struct {
			ID uuid.UUID `json:"id"`
		} `json:"org"`
		Role auth.Role `json:"role"`
	}
	founder.Post(t, "/api/v1/auth/signup", map[string]string{
		"email": founderEmail, "password": "a-long-enough-passphrase",
		"name": "A Founder", "orgName": "Journey QA",
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &signedUp)

	if signedUp.Role != auth.RoleOwner {
		t.Fatalf("role: got %q, want owner", signedUp.Role)
	}
	orgID := signedUp.Org.ID

	// 2. The owner invites a colleague as an admin.
	colleagueEmail := "colleague-" + uuid.NewString() + "@example.test"
	var invite struct {
		Token string `json:"token"`
	}
	founder.AsOrg(orgID).Post(t, "/api/v1/orgs/"+orgID.String()+"/invites", map[string]string{
		"email": colleagueEmail, "role": "admin",
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &invite)

	// 3. The colleague signs up for their own account (which creates an org of
	//    their own) and then accepts the invite.
	colleague := env.Anonymous(t)
	colleague.Post(t, "/api/v1/auth/signup", map[string]string{
		"email": colleagueEmail, "password": "another-long-passphrase",
		"name": "A Colleague", "orgName": "Colleague's Own Org",
	}).ExpectStatus(t, http.StatusCreated)

	colleague.Post(t, "/api/v1/invites/accept", map[string]string{"token": invite.Token}).
		ExpectStatus(t, http.StatusOK)

	// /me now lists both organizations with the right role in each.
	var me struct {
		Orgs []struct {
			ID   uuid.UUID `json:"id"`
			Role auth.Role `json:"role"`
		} `json:"orgs"`
	}
	colleague.Get(t, "/api/v1/me").ExpectStatus(t, http.StatusOK).JSON(t, &me)
	if len(me.Orgs) != 2 {
		t.Fatalf("got %d organizations, want 2", len(me.Orgs))
	}
	roles := map[uuid.UUID]auth.Role{}
	for _, o := range me.Orgs {
		roles[o.ID] = o.Role
	}
	if roles[orgID] != auth.RoleAdmin {
		t.Fatalf("role in the invited org: got %q, want admin", roles[orgID])
	}

	// 4. As an admin of the founder's org, the colleague pairs a daemon.
	var pairing struct {
		PairingCode string `json:"pairingCode"`
	}
	colleague.AsOrg(orgID).Post(t, "/api/v1/orgs/"+orgID.String()+"/runtimes/pair", nil).
		ExpectStatus(t, http.StatusCreated).JSON(t, &pairing)

	var redeemed struct {
		RuntimeID    uuid.UUID `json:"runtimeId"`
		RuntimeToken string    `json:"runtimeToken"`
		OrgID        uuid.UUID `json:"orgId"`
	}
	env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem", map[string]any{
		"pairingCode": pairing.PairingCode,
		"runtimeName": "journey-" + uuid.NewString(),
		"hostInfo":    map[string]string{"hostname": "ci-box", "os": "linux", "version": "0.1.0"},
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &redeemed)

	// The daemon lands in the org that issued the code, not in the org of
	// whoever happened to redeem it.
	if redeemed.OrgID != orgID {
		t.Fatalf("the runtime joined %s, want %s", redeemed.OrgID, orgID)
	}
	if redeemed.RuntimeToken == "" {
		t.Fatal("no runtime token was issued")
	}

	// 5. The member list shows both people.
	var members struct {
		Members []struct {
			Email string    `json:"email"`
			Role  auth.Role `json:"role"`
		} `json:"members"`
	}
	founder.AsOrg(orgID).Get(t, "/api/v1/orgs/"+orgID.String()+"/members").
		ExpectStatus(t, http.StatusOK).JSON(t, &members)
	if len(members.Members) != 2 {
		t.Fatalf("got %d members, want 2", len(members.Members))
	}
}

// Acceptance criteria 2 and 3, against the shipping router: no organization
// header, or somebody else's organization, is never a 200.
func TestRouterRefusesCrossTenantAccess(t *testing.T) {
	env := newTestEnv(t)
	orgA := env.NewOrg(t)
	orgB := env.NewOrg(t)

	paths := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/orgs/" + orgB.OrgID.String() + "/members", nil},
		{http.MethodGet, "/api/v1/orgs/" + orgB.OrgID.String() + "/invites", nil},
		{http.MethodPost, "/api/v1/orgs/" + orgB.OrgID.String() + "/invites",
			map[string]string{"email": "x@example.test", "role": "member"}},
		{http.MethodPost, "/api/v1/orgs/" + orgB.OrgID.String() + "/runtimes/pair", nil},
	}

	for _, p := range paths {
		t.Run("no org header: "+p.path, func(t *testing.T) {
			orgA.WithoutOrg().Do(t, p.method, p.path, p.body).
				ExpectError(t, http.StatusForbidden, "forbidden")
		})
		t.Run("another tenant: "+p.path, func(t *testing.T) {
			orgA.Do(t, p.method, p.path, p.body).
				ExpectError(t, http.StatusForbidden, "forbidden")
			orgA.AsOrg(orgB.OrgID).Do(t, p.method, p.path, p.body).
				ExpectError(t, http.StatusForbidden, "forbidden")
		})
	}
}

// A 5xx must never carry a driver message, and a request id must always be
// available to quote in a bug report.
func TestErrorResponsesCarryARequestID(t *testing.T) {
	env := newTestEnv(t)

	raw := env.Anonymous(t).Get(t, "/api/v1/me")
	raw.ExpectError(t, http.StatusUnauthorized, "unauthorized")

	if raw.Header.Get("X-Request-ID") == "" {
		t.Fatal("no X-Request-ID on the response")
	}
	if got := raw.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options: got %q", got)
	}
}

// The web app sends a cookie cross-origin, so the CORS allowlist has to work
// end to end - including advertising X-Org-ID on the preflight.
func TestCORSOnTheRealRouter(t *testing.T) {
	env := newTestEnv(t)
	client := env.Anonymous(t)

	preflight := client.DoWithHeaders(t, http.MethodOptions, "/api/v1/auth/login", nil, map[string]string{
		"Origin":                         "http://localhost:3000",
		"Access-Control-Request-Method":  http.MethodPost,
		"Access-Control-Request-Headers": "content-type,x-org-id",
	})
	preflight.ExpectStatus(t, http.StatusNoContent)

	if got := preflight.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("allow-origin: got %q", got)
	}
	if !strings.Contains(preflight.Header.Get("Access-Control-Allow-Headers"), "X-Org-ID") {
		t.Fatalf("allow-headers: got %q", preflight.Header.Get("Access-Control-Allow-Headers"))
	}
	if got := preflight.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("allow-credentials: got %q", got)
	}

	// An origin that is not on the list gets no CORS headers, so the browser
	// refuses to hand it the response.
	blocked := client.DoWithHeaders(t, http.MethodGet, "/healthz", nil, map[string]string{
		"Origin": "https://evil.example.com",
	})
	if got := blocked.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow-origin was set for an unlisted origin: %q", got)
	}
}
