package org_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/org"
)

func redeemBody(code, name string) map[string]any {
	return map[string]any{
		"pairingCode": code,
		"runtimeName": name,
		"hostInfo": map[string]string{
			"hostname": "qa-macbook", "os": "darwin", "arch": "arm64", "version": "0.3.1",
		},
	}
}

// Acceptance criterion 4, the happy half: a code pairs a daemon into the
// organization that issued it, and the org comes from the code rather than
// from anything the daemon sent.
func TestPairingRoundTrip(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	var code pairingView
	owner.Post(t, pairPath(owner.OrgID), nil).
		ExpectStatus(t, http.StatusCreated).JSON(t, &code)

	if code.PairingCode == "" {
		t.Fatal("no pairing code in the response")
	}
	// 15 minutes, per the contract. A minute of slack for the round trip.
	ttl := time.Until(code.ExpiresAt)
	if ttl > org.PairingCodeTTL+time.Minute || ttl < org.PairingCodeTTL-time.Minute {
		t.Fatalf("pairing code TTL is %s, want about %s", ttl, org.PairingCodeTTL)
	}

	name := "runtime-" + uuid.NewString()
	var redeemed redeemView
	env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem", redeemBody(code.PairingCode, name)).
		ExpectStatus(t, http.StatusCreated).JSON(t, &redeemed)

	if redeemed.OrgID != owner.OrgID {
		t.Fatalf("organization: got %s, want %s - the org must come from the code",
			redeemed.OrgID, owner.OrgID)
	}
	if redeemed.RuntimeName != name {
		t.Fatalf("runtime name: got %q, want %q", redeemed.RuntimeName, name)
	}
	if !strings.HasPrefix(redeemed.RuntimeToken, auth.RuntimeTokenPrefix) {
		t.Fatalf("runtime token %q does not carry the expected prefix", redeemed.RuntimeToken)
	}

	// The runtime landed in the issuing organization, with the reported host
	// facts on it.
	runtime, err := env.Store.GetRuntime(t.Context(), dbgen.GetRuntimeParams{
		OrgID: owner.OrgID, ID: redeemed.RuntimeID,
	})
	if err != nil {
		t.Fatalf("read the paired runtime: %v", err)
	}
	if runtime.Version != "0.3.1" {
		t.Fatalf("version: got %q, want 0.3.1", runtime.Version)
	}
	if !strings.Contains(string(runtime.HostInfo), "qa-macbook") {
		t.Fatalf("host info was not recorded: %s", runtime.HostInfo)
	}

	// Only the hash is stored: the token itself must not be recoverable.
	tokens, err := env.Store.ListRuntimeTokens(t.Context(), dbgen.ListRuntimeTokensParams{
		OrgID: owner.OrgID, RuntimeID: redeemed.RuntimeID,
	})
	if err != nil {
		t.Fatalf("list runtime tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("got %d runtime tokens, want 1", len(tokens))
	}
}

// Acceptance criterion 4, the other half: single use, and expiry.
func TestPairingCodeIsSingleUse(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	var code pairingView
	owner.Post(t, pairPath(owner.OrgID), nil).
		ExpectStatus(t, http.StatusCreated).JSON(t, &code)

	env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem",
		redeemBody(code.PairingCode, "first-"+uuid.NewString())).
		ExpectStatus(t, http.StatusCreated)

	// A second redeem of the same code fails, and does not create a runtime.
	env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem",
		redeemBody(code.PairingCode, "second-"+uuid.NewString())).
		ExpectError(t, http.StatusNotFound, "not_found")

	runtimes, err := env.Store.ListRuntimes(t.Context(), dbgen.ListRuntimesParams{
		OrgID: owner.OrgID, OnlineWithin: intervalOf(t, time.Minute),
	})
	if err != nil {
		t.Fatalf("list runtimes: %v", err)
	}
	if len(runtimes) != 1 {
		t.Fatalf("got %d runtimes, want 1 - the failed redeem left a row behind", len(runtimes))
	}
}

func TestExpiredPairingCodeIsRefused(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	var code pairingView
	owner.Post(t, pairPath(owner.OrgID), nil).
		ExpectStatus(t, http.StatusCreated).JSON(t, &code)

	// Expiry is filtered in SQL, so ageing the row is the whole test.
	tag, err := env.Store.Pool().Exec(t.Context(),
		`UPDATE pairing_codes SET expires_at = now() - interval '1 second' WHERE org_id = $1`,
		owner.OrgID)
	if err != nil {
		t.Fatalf("expire pairing code: %v", err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatal("no pairing code row to expire")
	}

	env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem",
		redeemBody(code.PairingCode, "runtime-"+uuid.NewString())).
		ExpectError(t, http.StatusNotFound, "not_found")
}

func TestRedeemRejectsBadCodes(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	var code pairingView
	owner.Post(t, pairPath(owner.OrgID), nil).
		ExpectStatus(t, http.StatusCreated).JSON(t, &code)

	tests := []struct {
		name string
		code string
	}{
		{"empty", ""},
		{"not shaped like a code", "hello"},
		{"right shape, never issued", "K7Q2-9FMR-3XT8"},
		{"ambiguous characters", "ILOU-ILOU-ILOU"},
		{"one character changed", flipPairingCode(code.PairingCode)},
		{"absurdly long", strings.Repeat("A", 500)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem",
				redeemBody(tt.code, "runtime-"+uuid.NewString())).
				ExpectError(t, http.StatusNotFound, "not_found")
		})
	}

	// The real code is still usable, so nothing above consumed it.
	env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem",
		redeemBody(code.PairingCode, "runtime-"+uuid.NewString())).
		ExpectStatus(t, http.StatusCreated)
}

// A person retyping a code off a screen gets the case and the dashes wrong;
// the normalisation has to make those work end to end.
func TestRedeemAcceptsAnyCasingOfTheCode(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	for _, transform := range []struct {
		name string
		fn   func(string) string
	}{
		{"lower case", strings.ToLower},
		{"no dashes", func(s string) string { return strings.ReplaceAll(s, "-", "") }},
		{"spaces instead of dashes", func(s string) string { return strings.ReplaceAll(s, "-", " ") }},
		{"padded with whitespace", func(s string) string { return "  " + s + "\t" }},
	} {
		t.Run(transform.name, func(t *testing.T) {
			var code pairingView
			owner.Post(t, pairPath(owner.OrgID), nil).
				ExpectStatus(t, http.StatusCreated).JSON(t, &code)

			env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem",
				redeemBody(transform.fn(code.PairingCode), "runtime-"+uuid.NewString())).
				ExpectStatus(t, http.StatusCreated)
		})
	}
}

func TestRedeemValidation(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	newCode := func(t *testing.T) string {
		t.Helper()
		var code pairingView
		owner.Post(t, pairPath(owner.OrgID), nil).
			ExpectStatus(t, http.StatusCreated).JSON(t, &code)
		return code.PairingCode
	}

	tests := []struct {
		name       string
		body       func(t *testing.T) any
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing runtime name",
			body:       func(t *testing.T) any { return redeemBody(newCode(t), "  ") },
			wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_failed",
		},
		{
			name: "runtime name too long",
			body: func(t *testing.T) any {
				return redeemBody(newCode(t), strings.Repeat("n", org.MaxRuntimeNameLength+1))
			},
			wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_failed",
		},
		{
			name: "control characters in the runtime name",
			body: func(t *testing.T) any {
				return redeemBody(newCode(t), "runtime\x00name")
			},
			wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_failed",
		},
		{
			name: "an operating system outside the contract",
			body: func(t *testing.T) any {
				body := redeemBody(newCode(t), "runtime-"+uuid.NewString())
				body["hostInfo"] = map[string]string{"os": "plan9"}
				return body
			},
			wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_failed",
		},
		{
			name:       "unknown field",
			body:       func(*testing.T) any { return map[string]any{"pairing_code": "x"} },
			wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_failed",
		},
		{
			name:       "malformed json",
			body:       func(*testing.T) any { return `{"pairingCode":` },
			wantStatus: http.StatusBadRequest, wantCode: "bad_request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem", tt.body(t)).
				ExpectError(t, tt.wantStatus, tt.wantCode)
		})
	}
}

// A daemon may not choose which organization it joins; only the code decides.
func TestRedeemedRuntimeIsInvisibleToOtherTenants(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)
	other := env.NewOrg(t)

	var code pairingView
	owner.Post(t, pairPath(owner.OrgID), nil).
		ExpectStatus(t, http.StatusCreated).JSON(t, &code)

	var redeemed redeemView
	env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem",
		redeemBody(code.PairingCode, "runtime-"+uuid.NewString())).
		ExpectStatus(t, http.StatusCreated).JSON(t, &redeemed)

	// The org-scoped read finds nothing for the other tenant, which is the
	// guarantee the whole query layer is built on.
	if _, err := env.Store.GetRuntime(t.Context(), dbgen.GetRuntimeParams{
		OrgID: other.OrgID, ID: redeemed.RuntimeID,
	}); err == nil {
		t.Fatal("another organization can read the paired runtime")
	}
}

// Two daemons racing on the same code must produce exactly one runtime: the
// claim is the UPDATE, not a read followed by a write.
func TestConcurrentRedeemProducesOneWinner(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	var code pairingView
	owner.Post(t, pairPath(owner.OrgID), nil).
		ExpectStatus(t, http.StatusCreated).JSON(t, &code)

	const racers = 6
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses []int
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := env.Anonymous(t).Do(t, http.MethodPost, "/api/v1/runtimes/redeem",
				redeemBody(code.PairingCode, "racer-"+uuid.NewString()))
			mu.Lock()
			statuses = append(statuses, resp.Status)
			mu.Unlock()
		}()
	}
	wg.Wait()

	won := 0
	for _, status := range statuses {
		switch status {
		case http.StatusCreated:
			won++
		case http.StatusNotFound, http.StatusConflict:
			// Lost the claim, or lost a serialization race. Both are honest
			// "try again" answers.
		default:
			t.Fatalf("unexpected status from a racing redeem: %d", status)
		}
	}
	if won != 1 {
		t.Fatalf("%d of %d concurrent redeems succeeded, want exactly 1", won, racers)
	}

	runtimes, err := env.Store.ListRuntimes(t.Context(), dbgen.ListRuntimesParams{
		OrgID: owner.OrgID, OnlineWithin: intervalOf(t, time.Minute),
	})
	if err != nil {
		t.Fatalf("list runtimes: %v", err)
	}
	if len(runtimes) != 1 {
		t.Fatalf("got %d runtimes after the race, want 1 - a loser left a row behind", len(runtimes))
	}
}

// A runtime name is unique inside an organization, so re-pairing a machine
// under a name that is taken is a conflict rather than a silent second entry.
func TestRedeemRefusesADuplicateRuntimeName(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	name := "duplicate-" + uuid.NewString()
	for i, wantStatus := range []int{http.StatusCreated, http.StatusConflict} {
		var code pairingView
		owner.Post(t, pairPath(owner.OrgID), nil).
			ExpectStatus(t, http.StatusCreated).JSON(t, &code)

		resp := env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem", redeemBody(code.PairingCode, name))
		if resp.Status != wantStatus {
			t.Fatalf("redeem %d: got status %d, want %d\n%s", i+1, resp.Status, wantStatus, resp.Text())
		}
	}

	// The same name in a different organization is fine: uniqueness is
	// per-tenant.
	other := env.NewOrg(t)
	var otherCode pairingView
	other.Post(t, pairPath(other.OrgID), nil).
		ExpectStatus(t, http.StatusCreated).JSON(t, &otherCode)
	env.Anonymous(t).Post(t, "/api/v1/runtimes/redeem", redeemBody(otherCode.PairingCode, name)).
		ExpectStatus(t, http.StatusCreated)
}

func TestPairingCodeRequiresAdmin(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	for _, role := range []auth.Role{auth.RoleViewer, auth.RoleMember} {
		t.Run(string(role), func(t *testing.T) {
			env.NewMember(t, owner.OrgID, role).Post(t, pairPath(owner.OrgID), nil).
				ExpectError(t, http.StatusForbidden, "forbidden")
		})
	}
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOwner} {
		t.Run(string(role), func(t *testing.T) {
			env.NewMember(t, owner.OrgID, role).Post(t, pairPath(owner.OrgID), nil).
				ExpectStatus(t, http.StatusCreated)
		})
	}

	env.Anonymous(t).Post(t, pairPath(owner.OrgID), nil).
		ExpectError(t, http.StatusUnauthorized, "unauthorized")
}

func flipPairingCode(code string) string {
	// Swap the last character for another one in the same alphabet, so the
	// result is still shaped like a code but is not this one.
	if code == "" {
		return "AAAA-AAAA-AAAA"
	}
	last := code[len(code)-1]
	replacement := byte('2')
	if last == '2' {
		replacement = '3'
	}
	return code[:len(code)-1] + string(replacement)
}
