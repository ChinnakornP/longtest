package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// Store is the slice of internal/db this package needs. Declaring it here
// rather than taking *db.Store keeps the dependency visible and makes the
// service's surface obvious: reads through the generated Querier, writes that
// touch more than one row through WithTx.
type Store interface {
	dbgen.Querier
	WithTx(ctx context.Context, fn func(q *dbgen.Queries) error) error
}

// SessionConfig configures cookie issuance.
type SessionConfig struct {
	// CookieName is the browser-visible name of the session cookie.
	CookieName string
	// TTL is the absolute lifetime of a session. There is no sliding renewal:
	// a stolen cookie is usable for at most this long, whatever the thief does
	// with it.
	TTL time.Duration
	// Secure sets the cookie's Secure flag. It is true everywhere except plain
	// http://localhost development, where the browser would drop the cookie.
	Secure bool
	// Domain scopes the cookie. Empty means host-only, which is what a
	// same-origin deployment wants.
	Domain string
	// TouchInterval bounds how often a request updates sessions.last_used_at.
	// Writing it on every request would turn every authenticated GET into a
	// write on a hot row for no operational benefit.
	TouchInterval time.Duration
}

// DefaultSessionConfig is the production shape: a 7-day absolute lifetime and
// a Secure, httpOnly, SameSite=Lax cookie.
//
// SameSite=Lax rather than Strict: Strict would drop the cookie on a
// top-level navigation from an e-mail link back into the app, which is the
// invite flow. Lax still blocks the cross-site POST that CSRF needs, and every
// state-changing route in this API is a POST/PATCH/DELETE.
func DefaultSessionConfig() SessionConfig {
	return SessionConfig{
		CookieName:    "qa_session",
		TTL:           7 * 24 * time.Hour,
		Secure:        true,
		TouchInterval: 5 * time.Minute,
	}
}

// SessionCookieSameSite is the mode used for the session cookie. It is a
// constant rather than configuration: relaxing it to None would make every
// state-changing endpoint CSRF-reachable.
const SessionCookieSameSite = http.SameSiteLaxMode

// OrgHeader carries the active organization. Per ADR-006 this is the ONLY
// source of an org id on a request: handlers never read one from a body, a
// query string, or (see RequireOrgMatchesPath) a path segment.
const OrgHeader = "X-Org-ID"

// Sessions issues, verifies and revokes login sessions.
//
// The cookie carries a random secret; the database stores only its SHA-256, so
// a dump of `sessions` cannot be replayed as a login.
type Sessions struct {
	store Store
	cfg   SessionConfig
}

// NewSessions returns a session manager over store.
func NewSessions(store Store, cfg SessionConfig) *Sessions {
	if cfg.CookieName == "" {
		cfg.CookieName = DefaultSessionConfig().CookieName
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultSessionConfig().TTL
	}
	if cfg.TouchInterval <= 0 {
		cfg.TouchInterval = DefaultSessionConfig().TouchInterval
	}
	return &Sessions{store: store, cfg: cfg}
}

// Issue creates a session for a user and returns the cookie value.
//
// The returned token is the only copy that will ever exist; it is handed
// straight to SetCookie and is not logged, stored or returned in a body.
func (s *Sessions) Issue(ctx context.Context, q dbgen.Querier, userID uuid.UUID) (string, time.Time, error) {
	token, err := newSecret()
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt := time.Now().Add(s.cfg.TTL)

	if _, err := q.CreateSession(ctx, dbgen.CreateSessionParams{
		UserID:    userID,
		TokenHash: hashSecret(token),
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("create session: %w", db.Classify(err))
	}
	return token, expiresAt, nil
}

// Authenticate resolves a cookie value to the user behind it.
//
// Expiry and revocation are filtered in SQL (GetLiveSessionByTokenHash), so
// there is no path through this function that accepts a dead session.
func (s *Sessions) Authenticate(ctx context.Context, token string) (Caller, error) {
	hash, err := HashBearerToken(token)
	if err != nil {
		return Caller{}, ErrInvalidCredentials
	}

	row, err := s.store.GetLiveSessionByTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return Caller{}, ErrInvalidCredentials
		}
		return Caller{}, fmt.Errorf("look up session: %w", db.Classify(err))
	}

	s.touch(ctx, row.Session)

	return Caller{
		UserID:    row.User.ID,
		SessionID: row.Session.ID,
		Email:     row.User.Email,
		Name:      row.User.Name,
	}, nil
}

// touch refreshes last_used_at, but only once per TouchInterval per session.
//
// The update is advisory: it powers "last active" in the UI and the session
// clean-up, and a failure must never turn a valid request into a 500.
func (s *Sessions) touch(ctx context.Context, session dbgen.Session) {
	if session.LastUsedAt.Valid && time.Since(session.LastUsedAt.Time) < s.cfg.TouchInterval {
		return
	}
	if _, err := s.store.TouchSession(ctx, session.ID); err != nil {
		httpx.LoggerFrom(ctx).WarnContext(ctx, "could not record session activity", "err", db.Classify(err))
	}
}

// Revoke ends the session behind a cookie value. Revoking an already-dead or
// unknown session is not an error: logout is idempotent.
func (s *Sessions) Revoke(ctx context.Context, token string) error {
	hash, err := HashBearerToken(token)
	if err != nil {
		return nil
	}
	if _, err := s.store.RevokeSession(ctx, hash); err != nil {
		return fmt.Errorf("revoke session: %w", db.Classify(err))
	}
	return nil
}

// TokenFromRequest reads the session cookie. A missing cookie is
// ErrInvalidCredentials, the same as a bad one.
func (s *Sessions) TokenFromRequest(r *http.Request) (string, error) {
	c, err := r.Cookie(s.cfg.CookieName)
	if err != nil || c.Value == "" {
		return "", ErrInvalidCredentials
	}
	return c.Value, nil
}

// SetCookie writes the session cookie.
func (s *Sessions) SetCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	//nolint:gosec // Secure is configuration, not a constant: local development
	// is served over plain http, where a browser drops a Secure cookie. It
	// defaults to true and .env.example documents the one place it may be off.
	http.SetCookie(w, &http.Cookie{
		Name:  s.cfg.CookieName,
		Value: token,
		Path:  "/",
		// Host-only unless a domain is configured, so a session issued for
		// api.example.com is not sent to every other subdomain.
		Domain:  s.cfg.Domain,
		Expires: expiresAt,
		MaxAge:  int(time.Until(expiresAt).Seconds()),
		// Not readable from JavaScript: an XSS in the dashboard must not be
		// able to exfiltrate a long-lived session.
		HttpOnly: true,
		Secure:   s.cfg.Secure,
		SameSite: SessionCookieSameSite,
	})
}

// ClearCookie expires the session cookie. The attributes must match SetCookie
// or the browser keeps the old cookie alongside the tombstone.
func (s *Sessions) ClearCookie(w http.ResponseWriter) {
	//nolint:gosec // see SetCookie: the attributes must match the cookie being
	// cleared, including a Secure flag that is configuration.
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    "",
		Path:     "/",
		Domain:   s.cfg.Domain,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.Secure,
		SameSite: SessionCookieSameSite,
	})
}

// CookieName exposes the configured cookie name for tests and for the web
// app's logout path.
func (s *Sessions) CookieName() string { return s.cfg.CookieName }
