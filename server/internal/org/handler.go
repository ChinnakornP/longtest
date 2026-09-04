package org

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// Handler serves the organization, membership, invite and pairing endpoints.
//
// As in internal/auth, a handler only decodes, calls the service and encodes.
// Note in particular that no handler here reads an organization id from a body
// or a path: it calls auth.MustOrgScope, which can only return the org the
// middleware verified against X-Org-ID.
type Handler struct {
	svc      *Service
	store    auth.Store
	sessions *auth.Sessions
}

// NewHandler returns the org HTTP handler.
func NewHandler(svc *Service, store auth.Store, sessions *auth.Sessions) *Handler {
	return &Handler{svc: svc, store: store, sessions: sessions}
}

// Mount registers the org routes with the middleware each one needs.
//
// The role gate is visible on the route, which is the point: reviewing "who
// can do this?" for the whole API is reading this one function, not auditing
// every handler body.
func (h *Handler) Mount(mux *http.ServeMux) {
	var (
		user      = auth.RequireUser(h.sessions)
		org       = auth.RequireOrg(h.store)
		pathOrg   = auth.RequireOrgMatchesPath("orgID")
		anyMember = auth.RequireRole(auth.RoleViewer)
		admin     = auth.RequireRole(auth.RoleAdmin)
	)

	// Creating an organization needs a session but no organization: this is
	// how a user who belongs to none gets their first.
	mux.Handle("POST /api/v1/orgs",
		httpx.Chain(httpx.Handler(h.create), user))

	mux.Handle("GET /api/v1/orgs/{orgID}/members",
		httpx.Chain(httpx.Handler(h.listMembers), user, org, pathOrg, anyMember))

	mux.Handle("POST /api/v1/orgs/{orgID}/invites",
		httpx.Chain(httpx.Handler(h.createInvite), user, org, pathOrg, admin))
	mux.Handle("GET /api/v1/orgs/{orgID}/invites",
		httpx.Chain(httpx.Handler(h.listInvites), user, org, pathOrg, admin))
	mux.Handle("DELETE /api/v1/orgs/{orgID}/invites/{inviteID}",
		httpx.Chain(httpx.Handler(h.revokeInvite), user, org, pathOrg, admin))

	// Accepting is deliberately NOT org-scoped: the caller is not a member yet,
	// so they cannot send an X-Org-ID the middleware would accept. The token
	// names the organization.
	mux.Handle("POST /api/v1/invites/accept",
		httpx.Chain(httpx.Handler(h.acceptInvite), user))

	mux.Handle("POST /api/v1/orgs/{orgID}/runtimes/pair",
		httpx.Chain(httpx.Handler(h.createPairingCode), user, org, pathOrg, admin))

	// Unauthenticated by necessity: a fresh daemon holds nothing but the code.
	mux.Handle("POST /api/v1/runtimes/redeem", httpx.Handler(h.redeem))
}

type createOrgRequest struct {
	Name string `json:"name"`
}

// create: POST /api/v1/orgs -> 201 Org.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) error {
	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		return httpx.Unauthorized("sign in to continue")
	}

	var req createOrgRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		return err
	}

	created, err := h.svc.Create(r.Context(), caller, req.Name)
	if err != nil {
		return err
	}

	httpx.WriteJSON(w, r, http.StatusCreated, auth.NewOrgView(created))
	return nil
}

// MemberView is a member of an organization as the API returns it.
type MemberView struct {
	UserID   uuid.UUID `json:"userId"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	Role     auth.Role `json:"role"`
	JoinedAt time.Time `json:"joinedAt"`
}

type membersResponse struct {
	Members []MemberView `json:"members"`
}

// listMembers: GET /api/v1/orgs/{orgID}/members -> 200 {members}.
func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}

	members, err := h.svc.ListMembers(r.Context(), scope)
	if err != nil {
		return err
	}

	views := make([]MemberView, 0, len(members))
	for _, m := range members {
		// A conversion, so the view stops compiling if Member changes shape.
		views = append(views, MemberView(m))
	}
	httpx.WriteJSON(w, r, http.StatusOK, membersResponse{Members: views})
	return nil
}

type createInviteRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// InviteView is an outstanding invite. It never carries the token: that value
// exists only in the response to the request that created it.
type InviteView struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	Role      auth.Role `json:"role"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}

func newInviteView(i dbgen.Invite) InviteView {
	return InviteView{
		ID:        i.ID,
		Email:     i.Email,
		Role:      auth.RoleFromDB(i.Role),
		ExpiresAt: i.ExpiresAt.Time.UTC(),
		CreatedAt: i.CreatedAt.Time.UTC(),
	}
}

// createInviteResponse is the ONLY place an invite token is ever rendered.
type createInviteResponse struct {
	InviteView
	// Token is shown once. There is no endpoint that can return it again;
	// re-inviting the same address issues a new one and revokes this.
	Token string `json:"token"`
}

// createInvite: POST /api/v1/orgs/{orgID}/invites -> 201 Invite + token.
func (h *Handler) createInvite(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}

	var req createInviteRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	role, err := auth.ParseRole(req.Role)
	if err != nil {
		return httpx.InvalidField("role", "must be one of viewer, member, admin, owner")
	}

	invite, token, err := h.svc.CreateInvite(r.Context(), scope, req.Email, role)
	if err != nil {
		return err
	}

	httpx.WriteJSON(w, r, http.StatusCreated, createInviteResponse{
		InviteView: newInviteView(invite),
		Token:      token,
	})
	return nil
}

type invitesResponse struct {
	Invites []InviteView `json:"invites"`
}

// listInvites: GET /api/v1/orgs/{orgID}/invites -> 200 {invites}.
func (h *Handler) listInvites(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}

	invites, err := h.svc.ListInvites(r.Context(), scope)
	if err != nil {
		return err
	}

	views := make([]InviteView, 0, len(invites))
	for _, i := range invites {
		views = append(views, newInviteView(i))
	}
	httpx.WriteJSON(w, r, http.StatusOK, invitesResponse{Invites: views})
	return nil
}

// revokeInvite: DELETE /api/v1/orgs/{orgID}/invites/{inviteID} -> 204.
func (h *Handler) revokeInvite(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}
	inviteID, err := httpx.PathUUID(r, "inviteID")
	if err != nil {
		return err
	}

	if err := h.svc.RevokeInvite(r.Context(), scope, inviteID); err != nil {
		return err
	}
	httpx.WriteNoContent(w)
	return nil
}

type acceptInviteRequest struct {
	Token string `json:"token"`
}

type acceptInviteResponse struct {
	Org  auth.OrgView `json:"org"`
	Role auth.Role    `json:"role"`
}

// acceptInvite: POST /api/v1/invites/accept -> 200 {org, role}.
func (h *Handler) acceptInvite(w http.ResponseWriter, r *http.Request) error {
	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		return httpx.Unauthorized("sign in to continue")
	}

	var req acceptInviteRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		return err
	}

	accepted, err := h.svc.AcceptInvite(r.Context(), caller, req.Token)
	if err != nil {
		return err
	}

	httpx.WriteJSON(w, r, http.StatusOK, acceptInviteResponse{
		Org:  auth.NewOrgView(accepted.Org),
		Role: accepted.Role,
	})
	return nil
}

type pairingCodeResponse struct {
	PairingCode string    `json:"pairingCode"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// createPairingCode: POST /api/v1/orgs/{orgID}/runtimes/pair
// -> 201 {pairingCode, expiresAt}.
func (h *Handler) createPairingCode(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}

	code, err := h.svc.CreatePairingCode(r.Context(), scope)
	if err != nil {
		return err
	}

	httpx.WriteJSON(w, r, http.StatusCreated, pairingCodeResponse{
		PairingCode: code.Code,
		ExpiresAt:   code.ExpiresAt.UTC(),
	})
	return nil
}

type redeemRequest struct {
	PairingCode string   `json:"pairingCode"`
	RuntimeName string   `json:"runtimeName"`
	HostInfo    HostInfo `json:"hostInfo"`
}

type redeemResponse struct {
	RuntimeID uuid.UUID `json:"runtimeId"`
	// RuntimeToken is shown exactly once. The daemon must persist it; there is
	// no endpoint that returns it again.
	RuntimeToken string    `json:"runtimeToken"`
	RuntimeName  string    `json:"runtimeName"`
	OrgID        uuid.UUID `json:"orgId"`
}

// redeem: POST /api/v1/runtimes/redeem -> 201 {runtimeId, runtimeToken}.
//
// Unauthenticated. Everything it trusts comes from the pairing code; the
// organization in the response is reported back to the daemon, not taken from
// it.
func (h *Handler) redeem(w http.ResponseWriter, r *http.Request) error {
	var req redeemRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		return err
	}

	redeemed, err := h.svc.RedeemPairingCode(r.Context(), RedeemInput(req))
	if err != nil {
		return err
	}

	httpx.WriteJSON(w, r, http.StatusCreated, redeemResponse{
		RuntimeID:    redeemed.Runtime.ID,
		RuntimeToken: redeemed.Token,
		RuntimeName:  redeemed.Runtime.Name,
		OrgID:        redeemed.Runtime.OrgID,
	})
	return nil
}
