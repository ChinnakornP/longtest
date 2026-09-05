package auth_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
)

// signupBody is the POST /api/v1/auth/signup payload.
type signupBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	OrgName  string `json:"orgName"`
}

type meBody struct {
	User struct {
		ID    uuid.UUID `json:"id"`
		Email string    `json:"email"`
		Name  string    `json:"name"`
	} `json:"user"`
	Orgs []struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
		Slug string    `json:"slug"`
		Role auth.Role `json:"role"`
	} `json:"orgs"`
}

// Acceptance criterion 1: signup -> login -> GET /me returns the right
// organization with the right role.
func TestSignupLoginMe(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	client := env.Anonymous(t)

	const password = "a-long-enough-passphrase"
	email := "founder-" + uuid.NewString() + "@example.test"

	var created struct {
		User struct {
			ID    uuid.UUID `json:"id"`
			Email string    `json:"email"`
		} `json:"user"`
		Org struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
			Slug string    `json:"slug"`
		} `json:"org"`
		Role auth.Role `json:"role"`
	}

	resp := client.Post(t, "/api/v1/auth/signup", signupBody{
		Email: email, Password: password, Name: "A Founder", OrgName: "Acme QA",
	}).ExpectStatus(t, http.StatusCreated)
	resp.JSON(t, &created)

	if created.Role != auth.RoleOwner {
		t.Fatalf("the creator of an organization must be its owner, got %q", created.Role)
	}
	if created.Org.Slug == "" {
		t.Fatal("the organization was created without a slug")
	}
	if strings.Contains(resp.Text(), "password") {
		t.Fatalf("the signup response mentions the password: %s", resp.Text())
	}

	// The session cookie must be set, httpOnly and SameSite=Lax.
	cookie := resp.Cookie(authtest.SessionCookieName())
	if cookie == nil {
		t.Fatal("signup did not set a session cookie")
	}
	if !cookie.HttpOnly {
		t.Fatal("the session cookie is readable from JavaScript")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie SameSite: got %v, want Lax", cookie.SameSite)
	}
	if cookie.Value == "" {
		t.Fatal("the session cookie is empty")
	}

	// Signing up leaves the caller signed in.
	var me meBody
	client.Get(t, "/api/v1/me").ExpectStatus(t, http.StatusOK).JSON(t, &me)
	assertSoleMembership(t, me, created.Org.ID, auth.RoleOwner)

	// ... and logging in from a clean client gets the same answer.
	fresh := env.Anonymous(t)
	var loggedIn meBody
	fresh.Post(t, "/api/v1/auth/login", map[string]string{
		"email": email, "password": password,
	}).ExpectStatus(t, http.StatusOK).JSON(t, &loggedIn)
	assertSoleMembership(t, loggedIn, created.Org.ID, auth.RoleOwner)

	fresh.Get(t, "/api/v1/me").ExpectStatus(t, http.StatusOK).JSON(t, &me)
	assertSoleMembership(t, me, created.Org.ID, auth.RoleOwner)

	// The e-mail is a case-insensitive identity (citext), so the same account
	// must log in whichever way it is typed.
	upper := env.Anonymous(t)
	upper.Post(t, "/api/v1/auth/login", map[string]string{
		"email": strings.ToUpper(email), "password": password,
	}).ExpectStatus(t, http.StatusOK)
}

func assertSoleMembership(t *testing.T, me meBody, orgID uuid.UUID, role auth.Role) {
	t.Helper()
	if len(me.Orgs) != 1 {
		t.Fatalf("got %d organizations, want exactly 1", len(me.Orgs))
	}
	if me.Orgs[0].ID != orgID {
		t.Fatalf("organization: got %s, want %s", me.Orgs[0].ID, orgID)
	}
	if me.Orgs[0].Role != role {
		t.Fatalf("role: got %q, want %q", me.Orgs[0].Role, role)
	}
}

func TestSignupValidation(t *testing.T) {
	env := authtest.New(t, newAPI(t))

	valid := signupBody{
		Email: "valid@example.test", Password: "a-long-enough-passphrase",
		Name: "A Person", OrgName: "An Org",
	}

	tests := []struct {
		name       string
		mutate     func(*signupBody)
		wantStatus int
		wantCode   string
	}{
		{"missing email", func(b *signupBody) { b.Email = "" }, http.StatusUnprocessableEntity, "validation_failed"},
		{"not an email", func(b *signupBody) { b.Email = "not-an-email" }, http.StatusUnprocessableEntity, "validation_failed"},
		{"email with a display name", func(b *signupBody) { b.Email = "A Person <a@b.test>" }, http.StatusUnprocessableEntity, "validation_failed"},
		{"email too long", func(b *signupBody) { b.Email = strings.Repeat("a", 250) + "@example.test" }, http.StatusUnprocessableEntity, "validation_failed"},
		{"password too short", func(b *signupBody) { b.Password = "short" }, http.StatusUnprocessableEntity, "validation_failed"},
		{"missing name", func(b *signupBody) { b.Name = "   " }, http.StatusUnprocessableEntity, "validation_failed"},
		{"missing org name", func(b *signupBody) { b.OrgName = "" }, http.StatusUnprocessableEntity, "validation_failed"},
		{"name too long", func(b *signupBody) { b.Name = strings.Repeat("n", 201) }, http.StatusUnprocessableEntity, "validation_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := valid
			body.Email = uuid.NewString() + "@example.test"
			tt.mutate(&body)

			env.Anonymous(t).Post(t, "/api/v1/auth/signup", body).
				ExpectError(t, tt.wantStatus, tt.wantCode)
		})
	}
}

func TestSignupRejectsADuplicateEmail(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	email := "taken-" + uuid.NewString() + "@example.test"

	body := signupBody{Email: email, Password: "a-long-enough-passphrase", Name: "First", OrgName: "First Org"}
	env.Anonymous(t).Post(t, "/api/v1/auth/signup", body).ExpectStatus(t, http.StatusCreated)

	body.Name, body.OrgName = "Second", "Second Org"
	env.Anonymous(t).Post(t, "/api/v1/auth/signup", body).
		ExpectError(t, http.StatusConflict, "conflict")

	// The rollback has to be complete: the user insert failed, so the
	// organization created in the same transaction must be gone too.
	found, err := env.Store.GetOrganizationBySlug(t.Context(), "second-org")
	if err == nil {
		t.Fatalf("the failed signup left organization %s behind", found.ID)
	}
}

// Two organizations with the same name must both be creatable; the slug is
// what has to be made unique, not the name.
func TestSignupDerivesAUniqueSlug(t *testing.T) {
	env := authtest.New(t, newAPI(t))

	slugs := map[string]bool{}
	for range 3 {
		var created struct {
			Org struct {
				Slug string `json:"slug"`
			} `json:"org"`
		}
		env.Anonymous(t).Post(t, "/api/v1/auth/signup", signupBody{
			Email:    uuid.NewString() + "@example.test",
			Password: "a-long-enough-passphrase",
			Name:     "A Person",
			OrgName:  "Duplicate Name Ltd",
		}).ExpectStatus(t, http.StatusCreated).JSON(t, &created)

		if created.Org.Slug == "" {
			t.Fatal("empty slug")
		}
		if slugs[created.Org.Slug] {
			t.Fatalf("slug %q was issued twice", created.Org.Slug)
		}
		slugs[created.Org.Slug] = true
	}
}

// An unknown e-mail and a wrong password must be indistinguishable, or login
// becomes a way to enumerate accounts.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	tests := []struct {
		name     string
		email    string
		password string
	}{
		{"unknown e-mail", "nobody-" + uuid.NewString() + "@example.test", authtest.Password},
		{"wrong password", owner.Email, "definitely-the-wrong-password"},
		{"empty password", owner.Email, ""},
		{"malformed e-mail", "not-an-email", authtest.Password},
	}

	var messages []string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envelope := env.Anonymous(t).Post(t, "/api/v1/auth/login", map[string]string{
				"email": tt.email, "password": tt.password,
			}).ExpectError(t, http.StatusUnauthorized, "unauthorized")
			messages = append(messages, envelope.Error.Message)
		})
	}

	for _, msg := range messages {
		if msg != messages[0] {
			t.Fatalf("login failures produce different messages (%q vs %q), "+
				"which tells a caller whether an account exists", messages[0], msg)
		}
	}
}

func TestLoginRejectsABadRequestBody(t *testing.T) {
	env := authtest.New(t, newAPI(t))

	tests := []struct {
		name       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{"malformed json", `{"email":`, http.StatusBadRequest, "bad_request"},
		{"unknown field", map[string]string{"e-mail": "a@b.test", "password": "x"}, http.StatusUnprocessableEntity, "validation_failed"},
		{"wrong type", `{"email":42}`, http.StatusUnprocessableEntity, "validation_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env.Anonymous(t).Post(t, "/api/v1/auth/login", tt.body).
				ExpectError(t, tt.wantStatus, tt.wantCode)
		})
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	owner.Get(t, "/api/v1/me").ExpectStatus(t, http.StatusOK)

	resp := owner.Post(t, "/api/v1/auth/logout", nil).ExpectStatus(t, http.StatusNoContent)
	if cookie := resp.Cookie(authtest.SessionCookieName()); cookie == nil || cookie.MaxAge >= 0 {
		t.Fatal("logout must expire the session cookie")
	}

	// The cookie is gone from the jar, so this is now an anonymous request...
	owner.Get(t, "/api/v1/me").ExpectError(t, http.StatusUnauthorized, "unauthorized")

	// ...and, more importantly, the token itself is dead, even in the hands of
	// somebody who kept a copy of it.
	stolen := env.WithSession(t, owner.SessionToken)
	stolen.Get(t, "/api/v1/me").ExpectError(t, http.StatusUnauthorized, "unauthorized")

	// Logging out twice is not an error: a user with a dead cookie must be
	// able to get back to a clean state.
	owner.Post(t, "/api/v1/auth/logout", nil).ExpectStatus(t, http.StatusNoContent)
	env.Anonymous(t).Post(t, "/api/v1/auth/logout", nil).ExpectStatus(t, http.StatusNoContent)
}

func TestMeRequiresASession(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	env.Anonymous(t).Get(t, "/api/v1/me").ExpectError(t, http.StatusUnauthorized, "unauthorized")
}

// A user who belongs to no organization is a real state: they signed up
// through an invite that has not been accepted yet.
func TestMeWithNoOrganizations(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	user := env.NewUser(t)
	client := env.SignIn(t, user, uuid.Nil, "")

	var me meBody
	client.Get(t, "/api/v1/me").ExpectStatus(t, http.StatusOK).JSON(t, &me)

	if me.User.ID != user.ID {
		t.Fatalf("user: got %s, want %s", me.User.ID, user.ID)
	}
	// An empty list, not null: the web app iterates it without a guard.
	if me.Orgs == nil {
		t.Fatal("orgs must serialise as [], not null")
	}
	if len(me.Orgs) != 0 {
		t.Fatalf("got %d organizations, want 0", len(me.Orgs))
	}
}

// The API must never return a password hash, however the response is built.
func TestResponsesNeverCarryAPasswordHash(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	for _, body := range []string{
		owner.Get(t, "/api/v1/me").ExpectStatus(t, http.StatusOK).Text(),
		env.Anonymous(t).Post(t, "/api/v1/auth/login", map[string]string{
			"email": owner.Email, "password": authtest.Password,
		}).ExpectStatus(t, http.StatusOK).Text(),
	} {
		for _, leak := range []string{"argon2", "passwordHash", "password_hash", "$v=19$"} {
			if strings.Contains(body, leak) {
				t.Fatalf("a response leaked %q:\n%s", leak, body)
			}
		}
	}
}
