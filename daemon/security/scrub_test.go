package security_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/agent/prompts"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// The credential used throughout. It is a fake, and it is deliberately not a
// dictionary word: a scrubber test that passes because the value never
// appeared is a test that proves nothing.
//
// The `gitleaks:allow` is narrow on purpose. The secret scan flags this line —
// correctly, it looks exactly like a credential — and the fix is to exempt the
// one line rather than to exempt test files, which would let a real credential
// through in the place people are most tempted to paste one.
const fakePassword = "Tr0ub4dor-and-3-horse-battery" // gitleaks:allow
const fakeUser = "admin@example.test"
const fakeTOTP = "JBSWY3DPEHPK3PXP"

func TestScrubberReplacesEveryEncoding(t *testing.T) {
	s := security.NewScrubber()
	if err := s.Add(fakePassword); err != nil {
		t.Fatalf("add: %v", err)
	}

	jsonEscaped := strings.Trim(string(mustJSON(t, fakePassword)), `"`)
	cases := []struct {
		name, in string
	}{
		{"plain", "login failed for " + fakePassword},
		{"url query", "POST /login?p=" + url.QueryEscape(fakePassword)},
		{"url path", "/reset/" + url.PathEscape(fakePassword)},
		{"json escaped", `{"password":"` + jsonEscaped + `"}`},
		{"base64 aligned", base64.StdEncoding.EncodeToString([]byte(fakePassword))},
		{"base64 offset 1", base64.StdEncoding.EncodeToString([]byte("x" + fakePassword))},
		{"base64 offset 2", base64.StdEncoding.EncodeToString([]byte("xy" + fakePassword))},
		{"basic auth header", "Authorization: Basic " +
			base64.StdEncoding.EncodeToString([]byte(fakeUser+":"+fakePassword))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := s.String(tc.in)
			if s.Contains(out) {
				t.Fatalf("a registered encoding survived scrubbing: %q", out)
			}
			if out == tc.in {
				t.Fatalf("nothing was replaced in %q", tc.in)
			}
		})
	}
}

func TestScrubberTokenIsStableAndDoesNotDiscloseTheSecret(t *testing.T) {
	s := security.NewScrubber()
	if err := s.Add(fakePassword); err != nil {
		t.Fatal(err)
	}
	a := s.String("first " + fakePassword)
	b := s.String("second " + fakePassword)
	tokenA := strings.TrimPrefix(a, "first ")
	tokenB := strings.TrimPrefix(b, "second ")
	if tokenA != tokenB {
		t.Fatalf("the same credential redacted to two different tokens: %q vs %q", tokenA, tokenB)
	}
	if strings.Contains(tokenA, fakePassword) {
		t.Fatalf("the redaction token contains the secret: %q", tokenA)
	}
	// Correlatable across a log without being reversible.
	if !strings.HasPrefix(tokenA, "[redacted:") {
		t.Fatalf("unexpected token shape %q", tokenA)
	}
}

func TestScrubberRefusesAShortSecret(t *testing.T) {
	s := security.NewScrubber()
	err := s.Add("abc")
	if !errors.Is(err, security.ErrSecretTooShort) {
		t.Fatalf("expected ErrSecretTooShort, got %v", err)
	}
	// And it must not have half-registered it: "abc" is a substring of half
	// the words in a log.
	if got := s.String("abcdef"); got != "abcdef" {
		t.Fatalf("a rejected secret was still registered: %q", got)
	}
}

// A secret that arrives split across two Write calls is the normal case for a
// CLI's stdout, where chunk boundaries follow the pipe rather than the text.
func TestScrubWriterHandlesASecretSplitAcrossWrites(t *testing.T) {
	s := security.NewScrubber()
	if err := s.Add(fakePassword); err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	w := s.Writer(&sink)

	line := "filling password field with " + fakePassword + " now\n"
	for i := 0; i < len(line); i += 7 {
		end := min(i+7, len(line))
		if _, err := w.Write([]byte(line[i:end])); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if strings.Contains(sink.String(), fakePassword) {
		t.Fatalf("the secret survived a chunked write: %q", sink.String())
	}
	if !strings.Contains(sink.String(), "filling password field with") {
		t.Fatalf("surrounding text was lost: %q", sink.String())
	}
}

func TestScrubJSONWalksTheStructure(t *testing.T) {
	s := security.NewScrubber()
	if err := s.Add(fakePassword); err != nil {
		t.Fatal(err)
	}
	// The credential appears as a value, inside a nested array, and as a key.
	doc := map[string]any{
		"steps":      []any{map[string]any{"value": fakePassword}},
		fakePassword: "used as a key",
		"nested":     map[string]any{"deep": []any{[]any{fakePassword}}},
		"unrelated":  42,
	}
	out, err := s.JSON(mustJSON(t, doc))
	if err != nil {
		t.Fatalf("scrub json: %v", err)
	}
	if strings.Contains(string(out), fakePassword) {
		t.Fatalf("credential survived JSON scrubbing: %s", out)
	}
	var round map[string]any
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("scrubbed json no longer parses: %v", err)
	}
	if round["unrelated"] != float64(42) {
		t.Fatalf("scrubbing damaged an unrelated value: %v", round["unrelated"])
	}
}

// The acceptance test for LONG-14's credential requirement: seed a fixture
// with a fake credential, drive a run end to end, then scan every surface a
// credential could reach.
func TestNothingLeaksACredentialAcrossAWholeRun(t *testing.T) {
	store, err := security.NewFixtureStore(nil)
	if err != nil {
		t.Fatalf("fixture store: %v", err)
	}
	if err := store.Set("logged_in_as_admin", security.Credential{
		Username: fakeUser, Password: fakePassword, TOTPSecret: fakeTOTP,
	}); err != nil {
		t.Fatalf("set fixture: %v", err)
	}

	guard, err := security.NewRunGuard(filepath.Join(t.TempDir(), "run-1"), store)
	if err != nil {
		t.Fatalf("run guard: %v", err)
	}
	defer guard.Close() //nolint:errcheck // test cleanup

	// 1. The prompt. The page under test echoes the credential back — a login
	// form that renders the password on a validation error is not exotic.
	prompt, err := prompts.Build(prompts.Input{
		Phase:          prompts.PhasePlanning,
		Nonce:          guard.Nonce,
		OutputSchema:   "test-plan@1",
		AllowedOrigins: []string{"demo.example.test"},
		FixtureNames:   store.Names(),
		Scrubber:       guard.Scrubber,
		Untrusted: []security.Block{{
			Kind:   security.KindDOMText,
			Source: "https://demo.example.test/login",
			Content: "Sign in failed for " + fakeUser + " with password " + fakePassword +
				". TOTP " + fakeTOTP + " was rejected.",
		}},
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}
	if err := guard.WriteFile("planning/prompt.md", []byte(prompt)); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	// 2. A workspace JSON file: the application map, holding what the crawler
	// typed and what the page said back.
	if err := guard.WriteJSON("planning/application-map.json", map[string]any{
		"pages": []any{map[string]any{
			"path":  "/login",
			"title": "Sign in",
			"observed": []any{
				"password=" + fakePassword,
				"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(fakeUser+":"+fakePassword)),
			},
		}},
	}); err != nil {
		t.Fatalf("write map: %v", err)
	}

	// 3. The run log, arriving in whatever chunks the CLI's pipe produced.
	var runLog bytes.Buffer
	lw := guard.LogWriter(&runLog)
	chunks := []string{
		"[executor] fill #password with ",
		fakePassword[:8], fakePassword[8:],
		"\n[executor] POST /login?u=" + url.QueryEscape(fakeUser) +
			"&p=" + url.QueryEscape(fakePassword) + "\n",
	}
	for _, c := range chunks {
		if _, err := lw.Write([]byte(c)); err != nil {
			t.Fatalf("log write: %v", err)
		}
	}
	if err := lw.Close(); err != nil {
		t.Fatalf("log close: %v", err)
	}

	// 4. A run.event payload.
	event, err := guard.Event(map[string]any{
		"code":   "step.failed",
		"detail": "fill failed: expected value " + fakePassword,
		"nested": map[string]any{"totp": fakeTOTP},
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}

	// 5. Artifact bodies: the network log and the console log.
	network := guard.Artifact(mustJSON(t, []any{map[string]any{
		"method": "POST", "url": "https://demo.example.test/login",
		"body": "username=" + fakeUser + "&password=" + url.QueryEscape(fakePassword),
	}}))
	console := guard.Artifact([]byte("[warn] login rejected for " + fakePassword + "\n"))
	if err := guard.WriteFile("planning/network.json", network); err != nil {
		t.Fatalf("write network artifact: %v", err)
	}
	if err := guard.WriteFile("planning/console.log", console); err != nil {
		t.Fatalf("write console artifact: %v", err)
	}

	// Now scan every surface for every secret the fixture holds.
	surfaces := map[string]string{
		"prompt":           prompt,
		"run log":          runLog.String(),
		"run.event":        string(event),
		"network artifact": string(network),
		"console artifact": string(console),
	}
	for name, body := range surfaces {
		for label, secret := range map[string]string{
			"password": fakePassword, "username": fakeUser, "totp": fakeTOTP,
		} {
			if strings.Contains(body, secret) {
				t.Errorf("%s contains the fixture %s verbatim", name, label)
			}
		}
	}

	// And the workspace, which is the surface a human debugging a failed run
	// actually opens.
	leaks, err := guard.Verify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(leaks) != 0 {
		t.Fatalf("credentials found on disk after the run: %+v", leaks)
	}
}

// Verify has to actually find something, or the test above passes on a
// workspace it never really read.
func TestVerifyDetectsAFileWrittenAroundTheGuard(t *testing.T) {
	store, _ := security.NewFixtureStore(nil)
	if err := store.Set("logged_in_as_admin", security.Credential{
		Username: fakeUser, Password: fakePassword,
	}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "run-2")
	guard, err := security.NewRunGuard(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close() //nolint:errcheck // test cleanup

	// Bypass the guard the way a careless future call site would.
	if err := guard.Workspace.WriteFile("out.json", []byte(`{"p":"`+fakePassword+`"}`)); err != nil {
		t.Fatal(err)
	}
	leaks, err := guard.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if len(leaks) != 1 || leaks[0].Path != "out.json" {
		t.Fatalf("expected Verify to find out.json, got %+v", leaks)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
