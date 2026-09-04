package run

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/artifact"
	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// IdempotencyHeader carries a client-chosen retry token on POST /runs. It is a
// header rather than a body field because it is about the request, not about
// the run, and because a proxy or a client library can then set it generically.
const IdempotencyHeader = "Idempotency-Key"

// Handler serves the run endpoints. It decodes, calls the service and encodes;
// there is no SQL and no business rule below this line.
type Handler struct {
	svc      *Service
	store    auth.Store
	sessions *auth.Sessions
}

// NewHandler returns the run HTTP handler.
func NewHandler(svc *Service, store auth.Store, sessions *auth.Sessions) *Handler {
	return &Handler{svc: svc, store: store, sessions: sessions}
}

// Mount registers the run routes with the middleware each needs. The role gate
// is on the route so "who can start a run?" is answered by reading this
// function rather than by auditing handler bodies.
func (h *Handler) Mount(mux *http.ServeMux) {
	var (
		user      = auth.RequireUser(h.sessions)
		org       = auth.RequireOrg(h.store)
		anyMember = auth.RequireRole(auth.RoleViewer)
		member    = auth.RequireRole(auth.RoleMember)
		runtime   = auth.RequireRuntime(h.store)
	)

	mux.Handle("POST /api/v1/runs", httpx.Chain(httpx.Handler(h.create), user, org, member))
	mux.Handle("GET /api/v1/runs", httpx.Chain(httpx.Handler(h.list), user, org, anyMember))
	mux.Handle("GET /api/v1/runs/{runID}", httpx.Chain(httpx.Handler(h.get), user, org, anyMember))
	mux.Handle("POST /api/v1/runs/{runID}/cancel", httpx.Chain(httpx.Handler(h.cancel), user, org, member))
	mux.Handle("GET /api/v1/runs/{runID}/events", httpx.Chain(httpx.Handler(h.events), user, org, anyMember))

	// Daemon-facing: authenticated by the runtime token, not by a session. It
	// is mounted here rather than in internal/artifact because the run is what
	// bounds the key prefix a signature may be issued for.
	mux.Handle("POST /api/v1/runs/{runID}/artifacts/presign",
		httpx.Chain(httpx.Handler(h.presignArtifact), runtime))
}

type createRequest struct {
	ProjectID   uuid.UUID   `json:"projectId"`
	RuntimeID   *uuid.UUID  `json:"runtimeId"`
	Mode        string      `json:"mode"`
	TestCaseIDs []uuid.UUID `json:"testCaseIds"`
}

// create: POST /api/v1/runs -> 201 Run, or 200 when an Idempotency-Key
// replayed an earlier request.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}

	var req createRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		return err
	}
	if req.ProjectID == uuid.Nil {
		return httpx.InvalidField("projectId", "is required")
	}

	created, err := h.svc.Create(r.Context(), scope, CreateInput{
		ProjectID:      req.ProjectID,
		RuntimeID:      req.RuntimeID,
		Mode:           req.Mode,
		TestCaseIDs:    req.TestCaseIDs,
		IdempotencyKey: r.Header.Get(IdempotencyHeader),
	})
	if err != nil {
		return err
	}

	status := http.StatusCreated
	if created.Existing {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, r, status, NewView(created.Run))
	return nil
}

type listResponse struct {
	Runs  []View `json:"runs"`
	Total int64  `json:"total"`
}

// list: GET /api/v1/runs?projectId=&limit=&offset= -> 200 {runs, total}.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return err
	}
	projectID, err := httpx.QueryUUIDPtr(r, "projectId")
	if err != nil {
		return err
	}
	page, err := httpx.PageFrom(r)
	if err != nil {
		return err
	}

	listed, err := h.svc.List(r.Context(), scope, projectID, page)
	if err != nil {
		return err
	}

	views := make([]View, 0, len(listed.Runs))
	for _, run := range listed.Runs {
		views = append(views, NewView(run))
	}
	httpx.WriteJSON(w, r, http.StatusOK, listResponse{Runs: views, Total: listed.Total})
	return nil
}

// get: GET /api/v1/runs/{runID} -> 200 Run.
func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	scope, runID, err := scopeAndRun(r)
	if err != nil {
		return err
	}

	found, err := h.svc.Get(r.Context(), scope, runID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusOK, NewView(found))
	return nil
}

// cancel: POST /api/v1/runs/{runID}/cancel -> 200 Run.
//
// Idempotent: cancelling an already-cancelled run succeeds. Cancelling a run
// that finished on its own is a 409, because that outcome is no longer
// reachable.
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) error {
	scope, runID, err := scopeAndRun(r)
	if err != nil {
		return err
	}

	cancelled, err := h.svc.Cancel(r.Context(), scope, runID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusOK, NewView(cancelled))
	return nil
}

type eventsResponse struct {
	Events []EventView `json:"events"`
	// NextSince is what to pass as ?since on the next poll. It is the highest
	// sequence in this page, so a client never has to work it out itself and a
	// page that ends mid-run resumes exactly where it stopped.
	NextSince int64 `json:"nextSince"`
}

// events: GET /api/v1/runs/{runID}/events?since=&limit= -> 200 {events}.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) error {
	scope, runID, err := scopeAndRun(r)
	if err != nil {
		return err
	}
	since, err := sinceFrom(r)
	if err != nil {
		return err
	}
	page, err := httpx.PageFrom(r)
	if err != nil {
		return err
	}

	events, err := h.svc.Events(r.Context(), scope, runID, since, page.Limit)
	if err != nil {
		return err
	}

	views := make([]EventView, 0, len(events))
	next := since
	for _, event := range events {
		views = append(views, NewEventView(event))
		next = event.Seq
	}
	httpx.WriteJSON(w, r, http.StatusOK, eventsResponse{Events: views, NextSince: next})
	return nil
}

type presignRequest struct {
	// Key is the full object key the daemon intends to write. It must be under
	// the run's own prefix, which is what the service re-checks.
	Key         string `json:"key"`
	ContentType string `json:"contentType"`
}

type presignResponse struct {
	URL       string    `json:"url"`
	Key       string    `json:"key"`
	Method    string    `json:"method"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// presignArtifact: POST /api/v1/runs/{runID}/artifacts/presign -> 201 {url}.
//
// The daemon calls this once per object it is about to upload. The URL it gets
// back authorises exactly that key and nothing else; see the internal/artifact
// package doc for why a prefix-wide presigned PUT is not a thing S3 can issue.
func (h *Handler) presignArtifact(w http.ResponseWriter, r *http.Request) error {
	rc, err := auth.MustRuntimeCaller(r.Context())
	if err != nil {
		return err
	}
	runID, err := httpx.PathUUID(r, "runID")
	if err != nil {
		return err
	}

	var req presignRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		return err
	}

	signed, err := h.svc.PresignArtifactUpload(r.Context(), rc, runID, req.Key)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, r, http.StatusCreated, presignResponse{
		URL: signed.URL, Key: signed.Key, Method: http.MethodPut, ExpiresAt: signed.ExpiresAt,
	})
	return nil
}

// PresignArtifactUpload mints an upload capability for one object of one run.
//
// It is on the service rather than the handler because it is an authorization
// decision: the run must belong to the caller's runtime, must still be live,
// and the key must be inside that run's prefix. A handler that assembled this
// itself could get any one of the three wrong.
func (s *Service) PresignArtifactUpload(ctx context.Context, rc auth.RuntimeCaller, runID uuid.UUID, key string) (artifact.SignedURL, error) {
	current, err := s.runForRuntime(ctx, rc, runID)
	if err != nil {
		return artifact.SignedURL{}, httpx.NotFound("run not found")
	}
	if isTerminal(current.Status) {
		// The upload window closes with the run. Evidence for a finished run
		// has nowhere to be referenced from, and an open capability outliving
		// its run is exactly what the window exists to prevent.
		return artifact.SignedURL{}, httpx.Conflict("that run has already finished")
	}
	return s.artifacts.PutURL(rc.OrgID, runID, runDay(current), key)
}

func scopeAndRun(r *http.Request) (auth.OrgScope, uuid.UUID, error) {
	scope, err := auth.MustOrgScope(r.Context())
	if err != nil {
		return auth.OrgScope{}, uuid.Nil, err
	}
	runID, err := httpx.PathUUID(r, "runID")
	if err != nil {
		return auth.OrgScope{}, uuid.Nil, err
	}
	return scope, runID, nil
}

// sinceFrom reads the resume cursor. Events are numbered from 0, so "nothing
// seen yet" is -1 rather than 0.
func sinceFrom(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("since")
	if raw == "" {
		return -1, nil
	}
	since, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || since < -1 {
		return 0, httpx.BadRequest("since must be a sequence number")
	}
	return since, nil
}
