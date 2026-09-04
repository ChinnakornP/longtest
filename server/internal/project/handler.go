package project

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/internal/testcase"
)

// Handler serves the project endpoints, including the two reads that hang off
// a project: its application map and its test cases.
type Handler struct {
	svc       *Service
	testCases *testcase.Service
	store     auth.Store
	sessions  *auth.Sessions
}

// NewHandler returns the project HTTP handler.
func NewHandler(svc *Service, testCases *testcase.Service, store auth.Store, sessions *auth.Sessions) *Handler {
	return &Handler{svc: svc, testCases: testCases, store: store, sessions: sessions}
}

// Mount registers the project routes.
func (h *Handler) Mount(mux *http.ServeMux) {
	var (
		user      = auth.RequireUser(h.sessions)
		org       = auth.RequireOrg(h.store)
		anyMember = auth.RequireRole(auth.RoleViewer)
		member    = auth.RequireRole(auth.RoleMember)
	)

	mux.Handle("POST /api/v1/projects", httpx.Chain(httpx.Handler(h.create), user, org, member))
	mux.Handle("GET /api/v1/projects", httpx.Chain(httpx.Handler(h.list), user, org, anyMember))
	mux.Handle("GET /api/v1/projects/{projectID}", httpx.Chain(httpx.Handler(h.get), user, org, anyMember))
	mux.Handle("GET /api/v1/projects/{projectID}/appmap", httpx.Chain(httpx.Handler(h.appMap), user, org, anyMember))
	mux.Handle("GET /api/v1/projects/{projectID}/test-cases", httpx.Chain(httpx.Handler(h.listTestCases), user, org, anyMember))
}

// View is a project as the API renders it.
type View struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	BaseURL    string     `json:"baseUrl"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// NewView renders a project row.
func NewView(p dbgen.Project) View {
	view := View{
		ID:        p.ID,
		Name:      p.Name,
		BaseURL:   p.BaseURL,
		CreatedAt: p.CreatedAt.Time.UTC(),
		UpdatedAt: p.UpdatedAt.Time.UTC(),
	}
	if p.ArchivedAt.Valid {
		archived := p.ArchivedAt.Time.UTC()
		view.ArchivedAt = &archived
	}
	return view
}

type createRequest struct {
	Name    string `json:"name"`
	BaseURL string `json:"baseUrl"`
}

// create: POST /api/v1/projects -> 201 Project.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}

	var req createRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		return err
	}

	created, err := h.svc.Create(r.Context(), scope, req.Name, req.BaseURL)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusCreated, NewView(created))
	return nil
}

type listResponse struct {
	Projects []View `json:"projects"`
	Total    int64  `json:"total"`
}

// list: GET /api/v1/projects?includeArchived=&limit=&offset= -> 200.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}
	page, err := httpx.PageFrom(r)
	if err != nil {
		return err
	}
	includeArchived := strings.EqualFold(r.URL.Query().Get("includeArchived"), "true")

	listed, err := h.svc.List(r.Context(), scope, includeArchived, page)
	if err != nil {
		return err
	}

	views := make([]View, 0, len(listed.Projects))
	for _, p := range listed.Projects {
		views = append(views, NewView(p))
	}
	httpx.WriteJSON(w, r, http.StatusOK, listResponse{Projects: views, Total: listed.Total})
	return nil
}

// get: GET /api/v1/projects/{projectID} -> 200 Project.
func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	scope, projectID, err := scopeAndProject(r)
	if err != nil {
		return err
	}

	found, err := h.svc.Get(r.Context(), scope, projectID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusOK, NewView(found))
	return nil
}

// appMap: GET /api/v1/projects/{projectID}/appmap -> 200 application-map@1.
//
// The body is the contract document itself rather than a wrapper, so the web
// app and the daemon validate the same bytes against the same schema.
func (h *Handler) appMap(w http.ResponseWriter, r *http.Request) error {
	scope, projectID, err := scopeAndProject(r)
	if err != nil {
		return err
	}

	document, err := h.svc.ApplicationMap(r.Context(), scope, projectID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusOK, document)
	return nil
}

// listTestCases: GET /api/v1/projects/{projectID}/test-cases?status= -> 200.
func (h *Handler) listTestCases(w http.ResponseWriter, r *http.Request) error {
	scope, projectID, err := scopeAndProject(r)
	if err != nil {
		return err
	}
	page, err := httpx.PageFrom(r)
	if err != nil {
		return err
	}

	// Establishes that the project is this tenant's before any case is read,
	// so an unknown project is a 404 rather than an empty list.
	if _, err := h.svc.Get(r.Context(), scope, projectID); err != nil {
		return err
	}

	listed, err := h.testCases.ListForProject(r.Context(), scope, projectID, r.URL.Query().Get("status"), page)
	if err != nil {
		return err
	}

	views := make([]testcase.View, 0, len(listed.TestCases))
	for _, tc := range listed.TestCases {
		views = append(views, testcase.NewView(tc))
	}
	httpx.WriteJSON(w, r, http.StatusOK, testcase.ListResponse{TestCases: views, Total: listed.Total})
	return nil
}

func scopeAndProject(r *http.Request) (auth.OrgScope, uuid.UUID, error) {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return auth.OrgScope{}, uuid.Nil, err
	}
	projectID, err := httpx.PathUUID(r, "projectID")
	if err != nil {
		return auth.OrgScope{}, uuid.Nil, err
	}
	return scope, projectID, nil
}
