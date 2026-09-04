package report

import (
	"net/http"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// Handler serves GET /api/v1/runs/{runID}/report.
type Handler struct {
	svc      *Service
	store    auth.Store
	sessions *auth.Sessions
}

// NewHandler returns the report HTTP handler.
func NewHandler(svc *Service, store auth.Store, sessions *auth.Sessions) *Handler {
	return &Handler{svc: svc, store: store, sessions: sessions}
}

// Mount registers the report route.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/runs/{runID}/report", httpx.Chain(httpx.Handler(h.get),
		auth.RequireUser(h.sessions), auth.RequireOrg(h.store), auth.RequireRole(auth.RoleViewer)))
}

// get: GET /api/v1/runs/{runID}/report -> 200 Report.
func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}
	runID, err := httpx.PathUUID(r, "runID")
	if err != nil {
		return err
	}

	report, err := h.svc.Get(r.Context(), scope, runID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusOK, report)
	return nil
}
