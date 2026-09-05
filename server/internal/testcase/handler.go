package testcase

import (
	"encoding/json"
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
	// Rewriting a case is the same class of decision as approving one — the
	// suite that runs against a customer's application changes either way — so
	// it takes the same role.
	mux.Handle("PUT /api/v1/test-cases/{testCaseID}/payload", httpx.Chain(httpx.Handler(h.putPayload), user, org, member))
	mux.Handle("GET /api/v1/test-cases/{testCaseID}/versions", httpx.Chain(httpx.Handler(h.versions), user, org, anyMember))
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

// payloadRequest wraps the document so the client can declare what it edited.
//
// The wrapper is what makes baseVersion possible: a bare test-case@1 body has
// nowhere to say which version the editor loaded, and without that this
// endpoint could only be last-write-wins.
type payloadRequest struct {
	// A pointer so that an absent baseVersion is told apart from a sent zero.
	// Both are refused, but "you did not send one" and "1 is the first version"
	// are different mistakes, and only one of them is a client bug.
	BaseVersion *int32          `json:"baseVersion"`
	Payload     json.RawMessage `json:"payload"`
}

// putPayload: PUT /api/v1/test-cases/{testCaseID}/payload -> 200 TestCase.
//
// PUT and not PATCH: the body carries the whole test-case@1 document, and a
// partial edit of a contract document is not something this API can validate.
func (h *Handler) putPayload(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}
	id, err := httpx.PathUUID(r, "testCaseID")
	if err != nil {
		return err
	}

	var req payloadRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	if req.BaseVersion == nil {
		return httpx.InvalidField("baseVersion",
			"is required, and is the version of the test case you loaded")
	}

	updated, err := h.svc.UpdatePayload(r.Context(), scope, id, PayloadEdit{
		BaseVersion: *req.BaseVersion,
		Payload:     req.Payload,
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusOK, NewView(updated))
	return nil
}

// versions: GET /api/v1/test-cases/{testCaseID}/versions -> 200 the history.
//
// Newest first, so versions[0] is the payload the case holds now. The diff a
// reviewer reads is rendered client-side from any two of these payloads; there
// is deliberately no server-side diff, because a diff of a contract document is
// a presentation decision and this endpoint would have to pick one.
func (h *Handler) versions(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}
	id, err := httpx.PathUUID(r, "testCaseID")
	if err != nil {
		return err
	}
	limit, err := httpx.LimitFrom(r)
	if err != nil {
		return err
	}

	history, err := h.svc.ListVersions(r.Context(), scope, id, limit)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusOK, NewVersionListResponse(history))
	return nil
}
