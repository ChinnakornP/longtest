package runtime

import (
	"net/http"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// Handler serves GET /api/v1/runtimes.
type Handler struct {
	svc      *Service
	store    auth.Store
	sessions *auth.Sessions
}

// NewHandler returns the runtime HTTP handler.
func NewHandler(svc *Service, store auth.Store, sessions *auth.Sessions) *Handler {
	return &Handler{svc: svc, store: store, sessions: sessions}
}

// Mount registers the runtime routes.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/runtimes", httpx.Chain(httpx.Handler(h.list),
		auth.RequireUser(h.sessions), auth.RequireOrg(h.store), auth.RequireRole(auth.RoleViewer)))
}

type listResponse struct {
	Runtimes []View `json:"runtimes"`
}

// list: GET /api/v1/runtimes -> 200 {runtimes}.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}

	runtimes, err := h.svc.List(r.Context(), scope)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusOK, listResponse{Runtimes: runtimes})
	return nil
}
