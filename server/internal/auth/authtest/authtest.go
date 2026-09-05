// Package authtest builds authenticated, tenant-scoped HTTP clients for tests.
//
// It exists so that every task after LONG-7 can write a cross-tenant test
// without re-deriving how a session or an organization is made:
//
//	func TestMain(m *testing.M) { authtest.Main(m) }
//
//	func TestListProjects(t *testing.T) {
//	    env := authtest.New(t, newRouter(...))   // scratch database + server
//	    owner := env.NewOrg(t)                   // org + owner + session
//	    other := env.NewOrg(t)                   // a second tenant
//
//	    owner.Get(t, "/api/v1/projects").ExpectStatus(t, 200)
//	    owner.AsOrg(other.OrgID).Get(t, "/api/v1/projects").ExpectError(t, 403, "forbidden")
//	}
//
// Fixtures are written straight to the database rather than through the signup
// endpoint on purpose: a package testing the run API must not have to mount the
// auth routes to get a logged-in caller.
//
// This package is only ever imported from _test.go files. It lives outside
// package auth so that auth's own tests can use it without an import cycle.
package authtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	pgdb "github.com/ChinnakornP/longtest/server/pkg/db"
)

// Password is the password every fixture account is created with, so a test
// that wants to exercise the login endpoint has something to send.
//
// It is a test fixture, not a credential: it only ever exists in a throwaway
// database that is dropped when the suite ends.
const Password = "authtest-fixture-passphrase"

const (
	adminDSNEnv    = "TEST_DATABASE_URL"
	fallbackDSNEnv = "DATABASE_URL"
)

var (
	sharedStore *db.Store
	// Fixtures hash a password on every account they create, so they use the
	// cheap parameters; the hash format is identical either way.
	fixtureHasher = auth.NewHasher(auth.FastPasswordParams())
)

// Main runs a package's tests against a scratch database.
//
// It provisions and drops a database of its own, exactly like
// internal/db's harness, so two packages running in parallel never share rows.
// With neither TEST_DATABASE_URL nor DATABASE_URL set it runs the suite
// anyway, and New skips the tests that need a database.
func Main(m *testing.M) {
	dsn := os.Getenv(adminDSNEnv)
	if dsn == "" {
		dsn = os.Getenv(fallbackDSNEnv)
	}
	if dsn == "" {
		os.Exit(m.Run())
	}

	code, err := runWithDatabase(dsn, m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "authtest:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runWithDatabase(adminDSN string, m *testing.M) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	name, err := scratchDatabaseName()
	if err != nil {
		return 0, err
	}

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return 0, fmt.Errorf("connect to %s: %w", pgdb.RedactDSN(adminDSN), err)
	}
	// The identifier is generated from hex here, never from input, and is
	// quoted anyway; CREATE DATABASE cannot take a bind parameter.
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		_ = admin.Close(ctx)
		return 0, fmt.Errorf("create scratch database: %w", err)
	}
	_ = admin.Close(ctx)

	defer dropDatabase(adminDSN, name)

	testDSN, err := withDatabase(adminDSN, name)
	if err != nil {
		return 0, err
	}

	migrator, err := pgdb.NewMigrator(ctx, testDSN, nil)
	if err != nil {
		return 0, err
	}
	if err := migrator.Up(ctx); err != nil {
		_ = migrator.Close()
		return 0, err
	}
	if err := migrator.Close(); err != nil {
		return 0, err
	}

	pool, err := pgdb.NewPool(ctx, testDSN, pgdb.DefaultPoolConfig())
	if err != nil {
		return 0, err
	}
	sharedStore = db.NewStore(pool)
	code := m.Run()
	pool.Close()
	return code, nil
}

func dropDatabase(adminDSN, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "authtest: cannot drop scratch database:", err)
		return
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
		fmt.Fprintln(os.Stderr, "authtest: cannot drop scratch database:", err)
	}
}

func scratchDatabaseName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate scratch database name: %w", err)
	}
	return "qa_test_" + hex.EncodeToString(b[:]), nil
}

func withDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", pgdb.RedactDSN(dsn), err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// Store returns the shared scratch store, skipping the test when Main found no
// database to work with.
func Store(t *testing.T) *db.Store {
	t.Helper()
	if sharedStore == nil {
		t.Skipf("set %s (or %s) to run the database-backed tests", adminDSNEnv, fallbackDSNEnv)
	}
	return sharedStore
}

// SessionConfig is the cookie configuration the fixtures issue against. Secure
// is off because httptest serves plain http; everything else matches
// production, so a test still exercises an httpOnly, SameSite=Lax cookie.
func SessionConfig() auth.SessionConfig {
	cfg := auth.DefaultSessionConfig()
	cfg.Secure = false
	return cfg
}

// SessionCookieName is the cookie name the fixtures issue under. It is derived
// rather than written out, because the name follows from Secure and Domain:
// hard-coding it here would let the derivation change without a test noticing.
func SessionCookieName() string {
	return SessionConfig().EffectiveCookieName()
}

// Env is one test's world: a scratch database and a server running the handler
// under test.
type Env struct {
	Store    *db.Store
	Server   *httptest.Server
	sessions *auth.Sessions
}

// New starts a test server for h and returns the environment around it. The
// server is shut down by t.Cleanup.
func New(t *testing.T, h http.Handler) *Env {
	t.Helper()

	store := Store(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return &Env{
		Store:    store,
		Server:   srv,
		sessions: auth.NewSessions(store, SessionConfig()),
	}
}

// URL resolves a path against the test server.
func (e *Env) URL(path string) string { return e.Server.URL + path }

// NewOrg creates an organization with a fresh owner and returns a client
// signed in as that owner, with X-Org-ID already set. This is the one-line
// fixture the task contract asks for.
func (e *Env) NewOrg(t *testing.T) *Client {
	t.Helper()

	suffix := uuid.NewString()[:8]
	org, err := e.Store.CreateOrganization(t.Context(), dbgen.CreateOrganizationParams{
		Name: "Test Org " + suffix,
		Slug: "test-org-" + suffix,
	})
	if err != nil {
		t.Fatalf("create organization: %v", err)
	}
	return e.NewMember(t, org.ID, auth.RoleOwner)
}

// NewMember adds a brand-new user to orgID with the given role and returns a
// client signed in as them.
func (e *Env) NewMember(t *testing.T, orgID uuid.UUID, role auth.Role) *Client {
	t.Helper()

	user := e.NewUser(t)
	if _, err := e.Store.UpsertMembership(t.Context(), dbgen.UpsertMembershipParams{
		OrgID:  orgID,
		UserID: user.ID,
		Role:   role.DB(),
	}); err != nil {
		t.Fatalf("grant %s membership: %v", role, err)
	}
	return e.SignIn(t, user, orgID, role)
}

// NewUser creates an account that belongs to no organization. Useful for
// testing the invite flow and "a user with no orgs" states.
func (e *Env) NewUser(t *testing.T) dbgen.User {
	t.Helper()

	hash, err := fixtureHasher.Hash(Password)
	if err != nil {
		t.Fatalf("hash fixture password: %v", err)
	}
	user, err := e.Store.CreateUser(t.Context(), dbgen.CreateUserParams{
		Email:        "user-" + uuid.NewString() + "@example.test",
		PasswordHash: hash,
		Name:         "Test User",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user
}

// SignIn issues a real session for user and returns a client that carries it.
//
// orgID may be uuid.Nil, which produces a signed-in client that sends no
// X-Org-ID - the fixture for "this endpoint must reject a request without an
// active organization".
func (e *Env) SignIn(t *testing.T, user dbgen.User, orgID uuid.UUID, role auth.Role) *Client {
	t.Helper()

	token, _, err := e.sessions.Issue(t.Context(), e.Store, user.ID)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}

	client := e.WithSession(t, token)
	client.UserID = user.ID
	client.Email = user.Email
	client.OrgID = orgID
	client.Role = role
	return client
}

// WithSession returns a client carrying an arbitrary session token.
//
// It is how a test asks "does this exact token still work?" - for example
// after a logout, where the point is that the token is dead rather than that
// the browser forgot it.
func (e *Env) WithSession(t *testing.T, token string) *Client {
	t.Helper()

	client := e.Anonymous(t)
	base, err := url.Parse(e.Server.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	//nolint:gosec // a jar entry, not a Set-Cookie: the attributes the server
	// sends are what the tests assert on, and httptest serves plain http.
	client.http.Jar.SetCookies(base, []*http.Cookie{{
		Name:  e.sessions.CookieName(),
		Value: token,
		Path:  "/",
	}})
	client.SessionToken = token
	return client
}

// Anonymous returns a client with no session and no organization.
//
// It still carries a cookie jar, so a test can sign up or log in through the
// real endpoints and keep the session the response sets.
func (e *Env) Anonymous(t *testing.T) *Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	return &Client{Env: e, http: &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		// A test asserts on a redirect itself, it never follows one.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Client is an HTTP client bound to one caller and one active organization.
type Client struct {
	Env    *Env
	UserID uuid.UUID
	Email  string
	OrgID  uuid.UUID
	Role   auth.Role
	// SessionToken is the raw cookie value, for tests that need to replay it
	// from somewhere other than this client's jar.
	SessionToken string

	http   *http.Client
	bearer string
}

// AsOrg returns a copy of the client that sends a different X-Org-ID. This is
// the cross-tenant fixture: the session is unchanged, only the organization
// the caller claims to be acting in.
func (c *Client) AsOrg(orgID uuid.UUID) *Client {
	clone := *c
	clone.OrgID = orgID
	return &clone
}

// WithoutOrg returns a copy that sends no X-Org-ID at all.
func (c *Client) WithoutOrg() *Client { return c.AsOrg(uuid.Nil) }

// WithBearer returns a copy that sends an Authorization header, for the
// runtime-token routes.
func (c *Client) WithBearer(token string) *Client {
	clone := *c
	clone.bearer = token
	return &clone
}

// Do sends a request. A non-nil body is marshalled as JSON.
func (c *Client) Do(t *testing.T, method, path string, body any) *Response {
	t.Helper()
	return c.DoWithHeaders(t, method, path, body, nil)
}

// DoWithHeaders is Do with extra request headers, for the cases where the
// header itself is what is under test - a malformed X-Org-ID, say, which
// AsOrg cannot express because it takes a uuid.
func (c *Client) DoWithHeaders(t *testing.T, method, path string, body any, headers map[string]string) *Response {
	t.Helper()

	var reader io.Reader
	if body != nil {
		switch typed := body.(type) {
		case string:
			// A raw string is sent verbatim, so a test can send malformed JSON.
			reader = bytes.NewBufferString(typed)
		default:
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal request body: %v", err)
			}
			reader = bytes.NewReader(encoded)
		}
	}

	req, err := http.NewRequestWithContext(t.Context(), method, c.Env.URL(path), reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.OrgID != uuid.Nil {
		req.Header.Set(auth.OrgHeader, c.OrgID.String())
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return &Response{
		Status:  resp.StatusCode,
		Header:  resp.Header,
		Cookies: resp.Cookies(),
		Body:    payload,
		method:  method,
		path:    path,
	}
}

// Get sends a GET request. It, Post and Delete are the shorthands tests
// actually use; Do and DoWithHeaders are there for the unusual cases.
func (c *Client) Get(t *testing.T, path string) *Response {
	t.Helper()
	return c.Do(t, http.MethodGet, path, nil)
}

// Post sends a POST request with a JSON body.
func (c *Client) Post(t *testing.T, path string, body any) *Response {
	t.Helper()
	return c.Do(t, http.MethodPost, path, body)
}

// Delete sends a DELETE request.
func (c *Client) Delete(t *testing.T, path string) *Response {
	t.Helper()
	return c.Do(t, http.MethodDelete, path, nil)
}

// Response is a captured HTTP response with assertions attached.
type Response struct {
	Status  int
	Header  http.Header
	Cookies []*http.Cookie
	Body    []byte

	method string
	path   string
}

// ExpectStatus fails the test unless the status matches, quoting the body -
// which, on a failure, is the error envelope that explains why.
func (r *Response) ExpectStatus(t *testing.T, want int) *Response {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s %s: got status %d, want %d\nbody: %s", r.method, r.path, r.Status, want, r.Body)
	}
	return r
}

// ErrorEnvelope is the decoded {"error":{...}} body.
type ErrorEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// ExpectError asserts both the status and the stable error code, which is what
// a client actually switches on.
func (r *Response) ExpectError(t *testing.T, status int, code string) ErrorEnvelope {
	t.Helper()
	r.ExpectStatus(t, status)

	var envelope ErrorEnvelope
	if err := json.Unmarshal(r.Body, &envelope); err != nil {
		t.Fatalf("%s %s: response is not an error envelope: %v\nbody: %s", r.method, r.path, err, r.Body)
	}
	if envelope.Error.Code != code {
		t.Fatalf("%s %s: got error code %q, want %q\nbody: %s",
			r.method, r.path, envelope.Error.Code, code, r.Body)
	}
	if envelope.Error.Message == "" {
		t.Fatalf("%s %s: error envelope has no message\nbody: %s", r.method, r.path, r.Body)
	}
	return envelope
}

// Text returns the body as a string, for assertions about what a response
// must NOT contain.
func (r *Response) Text() string { return string(r.Body) }

// JSON decodes the body into dst.
func (r *Response) JSON(t *testing.T, dst any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, dst); err != nil {
		t.Fatalf("%s %s: decode response: %v\nbody: %s", r.method, r.path, err, r.Body)
	}
}

// Cookie returns a named response cookie, or nil.
func (r *Response) Cookie(name string) *http.Cookie {
	for _, c := range r.Cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}
