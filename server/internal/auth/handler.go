package auth

import (
	"errors"
	"net/http"

	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// Handler serves the authentication endpoints.
//
// Handlers here do three things and nothing else: decode, call the service,
// encode. There is no SQL and no business rule below this line - the role
// gate, the tenancy check and the transaction boundaries all live in the
// middleware and the service.
type Handler struct {
	svc      *Service
	sessions *Sessions
}

// NewHandler returns the auth HTTP handler.
func NewHandler(svc *Service, sessions *Sessions) *Handler {
	return &Handler{svc: svc, sessions: sessions}
}

// Mount registers the auth routes on mux, each with the middleware it needs.
//
// The middleware is attached per route rather than to a prefix: signup and
// login must be reachable without a session, and a prefix rule that has to
// carve out exceptions is how an endpoint ends up unauthenticated by accident.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/auth/signup", httpx.Handler(h.signup))
	mux.Handle("POST /api/v1/auth/login", httpx.Handler(h.login))
	mux.Handle("POST /api/v1/auth/logout", httpx.Handler(h.logout))
	mux.Handle("GET /api/v1/me", httpx.Chain(httpx.Handler(h.me), RequireUser(h.sessions)))
}

type signupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	OrgName  string `json:"orgName"`
}

type signupResponse struct {
	User UserView `json:"user"`
	Org  OrgView  `json:"org"`
	Role Role     `json:"role"`
}

// signup: POST /api/v1/auth/signup -> 201 {user, org, role} + session cookie.
func (h *Handler) signup(w http.ResponseWriter, r *http.Request) error {
	var req signupRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		return err
	}

	// A conversion rather than a field-by-field copy: it stops compiling the
	// moment the wire shape and the service input diverge, which is exactly
	// when somebody needs to look at both.
	result, err := h.svc.Signup(r.Context(), SignupInput(req))
	if err != nil {
		return err
	}

	h.sessions.SetCookie(w, result.Credentials.Token, result.Credentials.ExpiresAt)
	httpx.WriteJSON(w, r, http.StatusCreated, signupResponse{
		User: NewUserView(result.User),
		Org:  NewOrgView(result.Org),
		Role: result.Role,
	})
	return nil
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type meResponse struct {
	User UserView         `json:"user"`
	Orgs []MembershipView `json:"orgs"`
}

// login: POST /api/v1/auth/login -> 200 {user, orgs} + session cookie.
//
// The body mirrors GET /me so the web app can populate its store from either.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) error {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		return err
	}

	result, err := h.svc.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			// One message for "no such account" and "wrong password".
			return httpx.Unauthorized("that e-mail and password do not match")
		}
		return err
	}

	h.sessions.SetCookie(w, result.Credentials.Token, result.Credentials.ExpiresAt)
	httpx.WriteJSON(w, r, http.StatusOK, meResponse{
		User: NewUserView(result.User),
		Orgs: NewMembershipViews(result.Orgs),
	})
	return nil
}

// logout: POST /api/v1/auth/logout -> 204.
//
// Deliberately not behind RequireUser: logging out with an already-dead cookie
// must clear the cookie rather than answer 401, or a user with an expired
// session has no way to get back to a clean state.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) error {
	token, err := h.sessions.TokenFromRequest(r)
	if err == nil {
		if err := h.svc.Logout(r.Context(), token); err != nil {
			return err
		}
	}

	h.sessions.ClearCookie(w)
	httpx.WriteNoContent(w)
	return nil
}

// me: GET /api/v1/me -> 200 {user, orgs:[{id,name,slug,role}]}.
func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	caller, ok := CallerFrom(r.Context())
	if !ok {
		return httpx.Unauthorized("sign in to continue")
	}

	result, err := h.svc.Me(r.Context(), caller)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			h.sessions.ClearCookie(w)
			return httpx.Unauthorized("your session is no longer valid")
		}
		return err
	}

	httpx.WriteJSON(w, r, http.StatusOK, meResponse{
		User: NewUserView(result.User),
		Orgs: NewMembershipViews(result.Orgs),
	})
	return nil
}
