package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ChinnakornP/longtest/server/internal/artifact"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/internal/realtime"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// Frame types, aliased so the call sites below read as frame names rather than
// as generated constants.
const (
	qaschemaRunAssign = qaschema.EnvelopeTypeRunAssign
	qaschemaRunCancel = qaschema.EnvelopeTypeRunCancel
)

// Sequence numbers for the two server-to-daemon frames.
//
// The envelope requires seq to be monotonic per run, and this direction has a
// fixed, short script: a run is assigned once and cancelled at most once, so
// the numbers are constants rather than a counter that would have to survive a
// reconnect. The daemon-to-server direction is where seq does real work — it
// is the dedup key for at-least-once event delivery.
const (
	assignSeq int64 = 0
	cancelSeq int64 = 1
)

// assignPayload is run.assign on the wire.
//
// It mirrors qaschema.RunAssignPayload except that testCases stay as raw
// documents: a test case is a test-case@1 body this backend stores and hands
// on but does not own, and decoding then re-encoding it would reorder its keys
// and drop anything a newer minor version of the contract added. The assembled
// frame is validated against the schema before it is sent, so keeping the
// documents opaque costs no safety.
type assignPayload struct {
	RunID     string                   `json:"runId"`
	Mode      string                   `json:"mode"`
	ProjectID string                   `json:"projectId"`
	BaseURL   string                   `json:"baseUrl"`
	AppMap    *qaschema.ApplicationMap `json:"appMap,omitempty"`
	TestCases []json.RawMessage        `json:"testCases,omitempty"`
	// Fixtures are the project's registered fixture NAMES. They travel with
	// the assignment so the daemon can tell the planner which logins it may
	// reference and reject a plan that invented one — while the model is still
	// there to be asked again, rather than after the run has finished and this
	// backend rejects the plan on ingest.
	//
	// Names only. There is no field here that could carry a value, and this
	// backend has none to put in one.
	Fixtures       []string                `json:"fixtures,omitempty"`
	ArtifactUpload qaschema.ArtifactUpload `json:"artifactUpload"`
}

// cancelPayload is run.cancel for a user-initiated cancel, which is the only
// kind this backend originates: a timeout or a shutdown cancel would be the
// daemon's own decision.
func cancelPayload() qaschema.RunCancelPayload {
	return qaschema.RunCancelPayload{
		Reason:  qaschema.RunCancelPayloadReasonUserRequested,
		Message: strPtr("cancelled from the dashboard"),
	}
}

// buildAssignFrame assembles the run.assign frame for a claimed run.
//
// It reads the project, the application map and the pinned test-case documents
// — everything the daemon needs to work without calling back into the API.
// That is deliberate: the daemon is on the far side of a NAT with one outbound
// socket, and a work item it has to fetch in pieces is a work item that fails
// halfway.
//
// There is no auth.OrgScope here and there cannot be one: this runs on the
// scheduler, not on a request, so there is no caller whose membership auth
// could have verified. The org id is the one on the run row the scheduler
// claimed, and the project reads below take it as a plain argument through
// the project service's System* methods (ADR-007).
func (s *Service) buildAssignFrame(ctx context.Context, claimed dbgen.Run) ([]byte, error) {
	project, err := s.projects.SystemGet(ctx, claimed.OrgID, claimed.ProjectID)
	if err != nil {
		return nil, err
	}

	payload := assignPayload{
		RunID:          claimed.ID.String(),
		Mode:           string(claimed.Mode),
		ProjectID:      claimed.ProjectID.String(),
		BaseURL:        project.BaseURL,
		ArtifactUpload: s.uploadGrant(claimed),
	}

	// A discover run builds the map; every other mode resolves element refs
	// against it, so it travels with the assignment. A project nobody has
	// discovered yet simply has none — that is the ordinary first run, not a
	// failure, so the not-found the project service returns is swallowed here.
	if claimed.Mode != dbgen.RunModeDiscover {
		appMap, err := s.projects.SystemApplicationMap(ctx, claimed.OrgID, claimed.ProjectID)
		switch {
		case err == nil:
			payload.AppMap = &appMap
		case isNotFound(err):
		default:
			return nil, err
		}
	}

	// A planning run is the one that needs the fixture vocabulary: it is what
	// the planner writes preconditions from. An execute run's cases already
	// name theirs, and a discover run writes no test case at all.
	if claimed.Mode == dbgen.RunModePlan || claimed.Mode == dbgen.RunModeFull {
		fixtures, err := s.store.ListProjectFixtureNames(ctx, dbgen.ListProjectFixtureNamesParams{
			OrgID: claimed.OrgID, ProjectID: claimed.ProjectID,
		})
		if err != nil {
			return nil, fmt.Errorf("list project fixtures: %w", db.Classify(err))
		}
		payload.Fixtures = fixtures
	}

	if claimed.Mode == dbgen.RunModeExecute || claimed.Mode == dbgen.RunModeFull {
		documents, err := s.store.ListTestCaseDocumentsForRun(ctx, dbgen.ListTestCaseDocumentsForRunParams{
			OrgID: claimed.OrgID, RunID: claimed.ID,
		})
		if err != nil {
			return nil, fmt.Errorf("list test case documents: %w", db.Classify(err))
		}
		payload.TestCases = make([]json.RawMessage, 0, len(documents))
		for _, doc := range documents {
			payload.TestCases = append(payload.TestCases, doc.Payload)
		}
	}

	return realtime.NewFrame(qaschemaRunAssign, &claimed.ID, assignSeq, payload)
}

// uploadGrant is the artifact upload capability handed to a daemon.
//
// keyPrefix is the only prefix this run's evidence may be written under, and it
// is the same bound the minting endpoint re-checks on every request and the
// artifacts_storage_key_layout CHECK re-checks on every insert. See the package
// doc of internal/artifact for why the grant is a minting endpoint rather than
// one signed URL.
func (s *Service) uploadGrant(claimed dbgen.Run) qaschema.ArtifactUpload {
	window := s.artifacts.UploadWindow()
	if window <= 0 {
		window = artifact.MaxUploadWindow
	}
	return qaschema.ArtifactUpload{
		PresignedPutBase: strings.TrimSuffix(s.cfg.PresignBaseURL, "/") +
			"/api/v1/runs/" + claimed.ID.String() + "/artifacts/presign",
		KeyPrefix: artifact.KeyPrefix(claimed.OrgID, claimed.ID, runDay(claimed)),
		ExpiresAt: time.Now().Add(window).UTC().Format(time.RFC3339),
	}
}

// runDay is the UTC day a run's evidence is filed under. It is the run's own
// creation day, not "today", so a run that crosses midnight keeps every
// artifact in one prefix and the key a daemon builds at 00:01 still validates.
func runDay(r dbgen.Run) time.Time {
	if r.CreatedAt.Valid {
		return r.CreatedAt.Time.UTC()
	}
	return time.Now().UTC()
}

func strPtr(s string) *string { return &s }

// isNotFound reports whether err is this API's 404. It lets the assignment
// path tell "this project has no map yet" from a real read failure without
// reaching for the string in the message.
func isNotFound(err error) bool {
	var apiErr *httpx.Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}
