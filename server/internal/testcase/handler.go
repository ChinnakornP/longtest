package testcase

import (
	"net/http"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// Handler serves the individual test-case endpoints. The per-project list
// lives on internal/project, next to the project it is scoped by.
type Handler struct {
	svc      *Service
	store    auth.Store
	sessions *auth.Sessions
}

// NewHandler returns the test case HTTP handler.
func NewHandler(svc *Service, store auth.Store, sessions *auth.Sessions) *Handler {
	return &Handler{svc: svc, store: store, sessions: sessions}
}

// Mount registers the test case routes.
func (h *Handler) Mount(mux *http.ServeMux) {
	var (
		user      = auth.RequireUser(h.sessions)
		org       = auth.RequireOrg(h.store)
		anyMember = auth.RequireRole(auth.RoleViewer)
		// Approving a case adds it to the regression suite that runs against a
		// customer's application, which is a member's decision, not a viewer's.
		member = auth.RequireRole(auth.RoleMember)
	)

	mux.Handle("GET /api/v1/test-cases/{testCaseID}", httpx.Chain(httpx.Handler(h.get), user, org, anyMember))
	mux.Handle("PATCH /api/v1/test-cases/{testCaseID}", httpx.Chain(httpx.Handler(h.patch), user, org, member))
}

// get: GET /api/v1/test-cases/{testCaseID} -> 200 TestCase.
func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}
	id, err := httpx.PathUUID(r, "testCaseID")
	if err != nil {
		return err
	}

	found, err := h.svc.Get(r.Context(), scope, id)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusOK, NewView(found))
	return nil
}

type patchRequest struct {
	Status string `json:"status"`
}

// patch: PATCH /api/v1/test-cases/{testCaseID} -> 200 TestCase.
//
// Status is the only field this endpoint accepts. Editing a payload is a
// different operation with different consequences — it bumps the version and
// snapshots the old one — and folding both into one PATCH would make "approve
// this case" and "rewrite this case" the same request.
func (h *Handler) patch(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}
	id, err := httpx.PathUUID(r, "testCaseID")
	if err != nil {
		return err
	}

	var req patchRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	if req.Status == "" {
		return httpx.InvalidField("status", "is required")
	}

	updated, err := h.svc.SetStatus(r.Context(), scope, id, req.Status)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusOK, NewView(updated))
	return nil
}
