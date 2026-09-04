package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/server/internal/auth"
)

// The `__Host-` prefix and the attributes it requires have to move together.
// A browser silently discards a `__Host-` cookie that is not Secure, Path=/
// and Domain-less, and "login does nothing, no error anywhere" is the worst
// possible failure mode to debug.
func TestSessionCookieNameFollowsItsPreconditions(t *testing.T) {
	cases := []struct {
		name   string
		secure bool
		domain string
		want   string
	}{
		{"production: secure and host-only", true, "", "__Host-qa_session"},
		{"local dev over plain http", false, "", "qa_session"},
		{"a Domain is set, so __Host- is not allowed", true, "app.example.com", "qa_session"},
		{"neither precondition holds", false, "app.example.com", "qa_session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := auth.SessionCookieName(tc.secure, tc.domain); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// And the cookie the manager actually writes has to satisfy them.
func TestHostPrefixedCookieIsIssuedWithItsPreconditions(t *testing.T) {
	cfg := auth.DefaultSessionConfig() // Secure: true, no Domain
	s := auth.NewSessions(nil, cfg)

	if !strings.HasPrefix(s.CookieName(), auth.HostPrefix) {
		t.Fatalf("the production default is not host-prefixed: %q", s.CookieName())
	}

	for _, write := range []struct {
		name string
		fn   func(http.ResponseWriter)
	}{
		{"SetCookie", func(w http.ResponseWriter) { s.SetCookie(w, "token", timeSoon()) }},
		// ClearCookie's attributes must match SetCookie's or the browser keeps
		// the old cookie alongside the tombstone — and for a __Host- cookie a
		// mismatch means the tombstone is rejected outright.
		{"ClearCookie", func(w http.ResponseWriter) { s.ClearCookie(w) }},
	} {
		t.Run(write.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			write.fn(rec)
			cookies := rec.Result().Cookies() //nolint:bodyclose // httptest recorder
			if len(cookies) != 1 {
				t.Fatalf("expected one cookie, got %d", len(cookies))
			}
			c := cookies[0]
			if !strings.HasPrefix(c.Name, auth.HostPrefix) {
				t.Fatalf("cookie name %q lost the prefix", c.Name)
			}
			if !c.Secure {
				t.Error("a __Host- cookie must be Secure")
			}
			if c.Path != "/" {
				t.Errorf("a __Host- cookie must have Path=/, got %q", c.Path)
			}
			if c.Domain != "" {
				t.Errorf("a __Host- cookie must have no Domain, got %q", c.Domain)
			}
			if !c.HttpOnly {
				t.Error("the session cookie must stay httpOnly")
			}
		})
	}
}

// The dev configuration must not emit a prefixed cookie, or local login breaks.
func TestPlainHTTPDevelopmentGetsNoPrefix(t *testing.T) {
	cfg := auth.DefaultSessionConfig()
	cfg.Secure = false
	s := auth.NewSessions(nil, cfg)

	rec := httptest.NewRecorder()
	s.SetCookie(rec, "token", timeSoon())
	cookies := rec.Result().Cookies() //nolint:bodyclose // httptest recorder
	if len(cookies) != 1 {
		t.Fatalf("expected one cookie, got %d", len(cookies))
	}
	if strings.HasPrefix(cookies[0].Name, auth.HostPrefix) {
		t.Fatalf("a non-Secure cookie was given the __Host- prefix: %q — a browser would drop it",
			cookies[0].Name)
	}
}

// Regression: DefaultSessionConfig leaves CookieName empty because the name is
// derived. A caller that read the field directly got "" and built a header
// like `Cookie: =token`, which reads as an expired session rather than a bug —
// it broke the control-plane WebSocket tests and nothing else. Nothing may
// return an empty cookie name, whatever the configuration.
func TestEffectiveCookieNameIsNeverEmpty(t *testing.T) {
	configs := []struct {
		name string
		cfg  auth.SessionConfig
	}{
		{"zero value", auth.SessionConfig{}},
		{"production default", auth.DefaultSessionConfig()},
		{"plain http dev", func() auth.SessionConfig {
			c := auth.DefaultSessionConfig()
			c.Secure = false
			return c
		}()},
		{"with a domain", func() auth.SessionConfig {
			c := auth.DefaultSessionConfig()
			c.Domain = "app.example.com"
			return c
		}()},
	}
	for _, tc := range configs {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.EffectiveCookieName()
			if got == "" {
				t.Fatal("EffectiveCookieName returned an empty string")
			}
			// And it must agree with the name the manager actually issues under.
			if issued := auth.NewSessions(nil, tc.cfg).CookieName(); issued != got {
				t.Fatalf("EffectiveCookieName says %q but Sessions issues %q", got, issued)
			}
		})
	}
}

// An explicit name still wins, for a deployment mid-migration that has to keep
// an existing cookie name working.
func TestAnExplicitCookieNameIsHonoured(t *testing.T) {
	cfg := auth.DefaultSessionConfig()
	cfg.CookieName = "legacy_session"
	if got := auth.NewSessions(nil, cfg).CookieName(); got != "legacy_session" {
		t.Fatalf("got %q, want the configured name", got)
	}
}

func timeSoon() time.Time { return time.Now().Add(time.Hour) }
