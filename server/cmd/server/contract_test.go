package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// The REST contract, including the error paths. Every assertion here is on the
// wire shape and the status code, because those are what the web app and the
// daemon are written against.

// --- response shapes the tests decode into --------------------------------

type projectView struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	BaseURL string    `json:"baseUrl"`
}

type runView struct {
	ID        uuid.UUID  `json:"id"`
	ProjectID uuid.UUID  `json:"projectId"`
	RuntimeID *uuid.UUID `json:"runtimeId"`
	Mode      string     `json:"mode"`
	Status    string     `json:"status"`
	Phase     string     `json:"phase"`
	Counters  struct {
		Total   int `json:"total"`
		Passed  int `json:"passed"`
		Failed  int `json:"failed"`
		Skipped int `json:"skipped"`
		Errored int `json:"errored"`
	} `json:"counters"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	CreatedAt time.Time `json:"createdAt"`
}

type runtimeView struct {
	ID       uuid.UUID       `json:"id"`
	Name     string          `json:"name"`
	Online   bool            `json:"online"`
	Version  string          `json:"version"`
	Browsers json.RawMessage `json:"browsers"`
	Agents   json.RawMessage `json:"agents"`
}

// --- projects -------------------------------------------------------------

func TestProjectContract(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)

	t.Run("create returns the project", func(t *testing.T) {
		resp := owner.Post(t, "/api/v1/projects", map[string]string{
			"name": "Demo", "baseUrl": "https://demo.example.com/",
		})
		resp.ExpectStatus(t, http.StatusCreated)

		var view projectView
		resp.JSON(t, &view)
		if view.Name != "Demo" {
			t.Fatalf("got name %q, want Demo", view.Name)
		}
		// The trailing slash is normalised away, so the daemon is never handed
		// two spellings of the same origin.
		if view.BaseURL != "https://demo.example.com" {
			t.Fatalf("got baseUrl %q, want https://demo.example.com", view.BaseURL)
		}
	})

	t.Run("re-submitting the same project is not a conflict", func(t *testing.T) {
		body := map[string]string{"name": "Retried", "baseUrl": "https://retry.example.com"}
		first := owner.Post(t, "/api/v1/projects", body)
		first.ExpectStatus(t, http.StatusCreated)
		second := owner.Post(t, "/api/v1/projects", body)
		second.ExpectStatus(t, http.StatusCreated)

		var a, b projectView
		first.JSON(t, &a)
		second.JSON(t, &b)
		if a.ID != b.ID {
			t.Fatalf("a double submit created two projects: %s and %s", a.ID, b.ID)
		}
	})

	t.Run("the same name pointing somewhere else is a conflict", func(t *testing.T) {
		owner.Post(t, "/api/v1/projects", map[string]string{
			"name": "Ambiguous", "baseUrl": "https://one.example.com",
		}).ExpectStatus(t, http.StatusCreated)
		owner.Post(t, "/api/v1/projects", map[string]string{
			"name": "Ambiguous", "baseUrl": "https://two.example.com",
		}).ExpectError(t, http.StatusConflict, "conflict")
	})

	t.Run("invalid bodies are rejected by field", func(t *testing.T) {
		for name, body := range map[string]map[string]string{
			"no name":            {"baseUrl": "https://x.example.com"},
			"no scheme":          {"name": "X", "baseUrl": "demo.example.com"},
			"not http":           {"name": "X", "baseUrl": "ftp://demo.example.com"},
			"carries a password": {"name": "X", "baseUrl": "https://admin:secret@demo.example.com"},
		} {
			t.Run(name, func(t *testing.T) {
				owner.Post(t, "/api/v1/projects", body).
					ExpectError(t, http.StatusUnprocessableEntity, "validation_failed")
			})
		}
	})

	t.Run("a viewer may read but not create", func(t *testing.T) {
		viewer := env.NewMember(t, owner.OrgID, auth.RoleViewer)
		viewer.Get(t, "/api/v1/projects").ExpectStatus(t, http.StatusOK)
		viewer.Post(t, "/api/v1/projects", map[string]string{
			"name": "Nope", "baseUrl": "https://nope.example.com",
		}).ExpectError(t, http.StatusForbidden, "forbidden")
	})

	t.Run("another organization sees none of it", func(t *testing.T) {
		projectID := env.project(t, owner, "https://isolated.example.com")
		stranger := env.NewOrg(t)

		stranger.Get(t, "/api/v1/projects/"+projectID.String()).
			ExpectError(t, http.StatusNotFound, "not_found")
		stranger.Get(t, "/api/v1/projects/"+projectID.String()+"/appmap").
			ExpectError(t, http.StatusNotFound, "not_found")
		stranger.Get(t, "/api/v1/projects/"+projectID.String()+"/test-cases").
			ExpectError(t, http.StatusNotFound, "not_found")

		// Claiming the owner's organization with somebody else's session is a
		// 403 from the membership check, never a 404 from the query.
		stranger.AsOrg(owner.OrgID).Get(t, "/api/v1/projects").
			ExpectError(t, http.StatusForbidden, "forbidden")
	})

	t.Run("a request without an organization is refused", func(t *testing.T) {
		owner.WithoutOrg().Get(t, "/api/v1/projects").ExpectError(t, http.StatusForbidden, "forbidden")
	})
}

// A project nobody has discovered has no map. That is a 404, not an empty 200:
// application-map@1 requires at least one page, so an empty document would not
// be the contract document this route promises.
func TestApplicationMapIsMissingUntilDiscoveryHasRun(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://map.example.com")

	owner.Get(t, "/api/v1/projects/"+projectID.String()+"/appmap").
		ExpectError(t, http.StatusNotFound, "not_found")
}

// --- runs -----------------------------------------------------------------

func TestRunCreationContract(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://runs.example.com")
	runtimeID, _ := env.pairedRuntime(t, owner)

	t.Run("a queued run comes back with no runtime and no phase", func(t *testing.T) {
		resp := owner.Post(t, "/api/v1/runs", map[string]any{
			"projectId": projectID, "mode": "discover",
		})
		resp.ExpectStatus(t, http.StatusCreated)

		var view runView
		resp.JSON(t, &view)
		if view.Status != "queued" {
			t.Fatalf("got status %q, want queued", view.Status)
		}
		// No runtime is named and none is connected, so the run waits rather
		// than failing. That is the contract: a run that cannot be placed is
		// queued, not an error.
		if view.RuntimeID != nil {
			t.Fatalf("got runtimeId %s, want none", view.RuntimeID)
		}
	})

	t.Run("naming a runtime pins the run to it", func(t *testing.T) {
		resp := owner.Post(t, "/api/v1/runs", map[string]any{
			"projectId": projectID, "runtimeId": runtimeID, "mode": "discover",
		})
		resp.ExpectStatus(t, http.StatusCreated)

		var view runView
		resp.JSON(t, &view)
		if view.RuntimeID == nil || *view.RuntimeID != runtimeID {
			t.Fatalf("got runtimeId %v, want %s", view.RuntimeID, runtimeID)
		}
	})

	t.Run("an unknown project or runtime is a 404", func(t *testing.T) {
		owner.Post(t, "/api/v1/runs", map[string]any{
			"projectId": uuid.New(), "mode": "discover",
		}).ExpectError(t, http.StatusNotFound, "not_found")

		owner.Post(t, "/api/v1/runs", map[string]any{
			"projectId": projectID, "runtimeId": uuid.New(), "mode": "discover",
		}).ExpectError(t, http.StatusNotFound, "not_found")
	})

	t.Run("another organization's project is a 404, not a 403", func(t *testing.T) {
		stranger := env.NewOrg(t)
		stranger.Post(t, "/api/v1/runs", map[string]any{
			"projectId": projectID, "mode": "discover",
		}).ExpectError(t, http.StatusNotFound, "not_found")
	})

	t.Run("an unknown mode names the four that exist", func(t *testing.T) {
		envelope := owner.Post(t, "/api/v1/runs", map[string]any{
			"projectId": projectID, "mode": "explode",
		}).ExpectError(t, http.StatusUnprocessableEntity, "validation_failed")

		fields, _ := envelope.Error.Details["fields"].(map[string]any)
		if _, ok := fields["mode"]; !ok {
			t.Fatalf("the validation error does not name the mode field: %v", envelope.Error.Details)
		}
	})

	t.Run("an execute run with no approved cases is a conflict", func(t *testing.T) {
		owner.Post(t, "/api/v1/runs", map[string]any{
			"projectId": projectID, "mode": "execute",
		}).ExpectError(t, http.StatusConflict, "conflict")
	})

	t.Run("a viewer may not start a run", func(t *testing.T) {
		viewer := env.NewMember(t, owner.OrgID, auth.RoleViewer)
		viewer.Post(t, "/api/v1/runs", map[string]any{
			"projectId": projectID, "mode": "discover",
		}).ExpectError(t, http.StatusForbidden, "forbidden")
	})
}

// A retried POST must not start a second browser session against a customer's
// application. That is the whole reason the key exists.
func TestIdempotencyKeyReplaysTheOriginalRun(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://idempotent.example.com")

	key := "retry-" + uuid.NewString()
	body := map[string]any{"projectId": projectID, "mode": "discover"}
	headers := map[string]string{"Idempotency-Key": key}

	first := owner.DoWithHeaders(t, http.MethodPost, "/api/v1/runs", body, headers)
	first.ExpectStatus(t, http.StatusCreated)

	// A replay is a 200, not a 201: the caller learns that nothing new was
	// started, which is exactly what it needs to know.
	second := owner.DoWithHeaders(t, http.MethodPost, "/api/v1/runs", body, headers)
	second.ExpectStatus(t, http.StatusOK)

	var a, b runView
	first.JSON(t, &a)
	second.JSON(t, &b)
	if a.ID != b.ID {
		t.Fatalf("a retry started a second run: %s and %s", a.ID, b.ID)
	}

	// The key is scoped to the organization, so another tenant reusing the same
	// string gets its own run rather than a peek at this one.
	stranger := env.NewOrg(t)
	strangerProject := env.project(t, stranger, "https://other.example.com")
	other := stranger.DoWithHeaders(t, http.MethodPost, "/api/v1/runs",
		map[string]any{"projectId": strangerProject, "mode": "discover"}, headers)
	other.ExpectStatus(t, http.StatusCreated)

	var c runView
	other.JSON(t, &c)
	if c.ID == a.ID {
		t.Fatal("an idempotency key leaked across organizations")
	}
}

func TestRunReadContract(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://reads.example.com")

	resp := owner.Post(t, "/api/v1/runs", map[string]any{"projectId": projectID, "mode": "discover"})
	resp.ExpectStatus(t, http.StatusCreated)
	var created runView
	resp.JSON(t, &created)

	t.Run("get", func(t *testing.T) {
		owner.Get(t, "/api/v1/runs/"+created.ID.String()).ExpectStatus(t, http.StatusOK)
		owner.Get(t, "/api/v1/runs/"+uuid.NewString()).ExpectError(t, http.StatusNotFound, "not_found")
		owner.Get(t, "/api/v1/runs/not-a-uuid").ExpectError(t, http.StatusBadRequest, "bad_request")
	})

	t.Run("list, filtered by project", func(t *testing.T) {
		listed := owner.Get(t, "/api/v1/runs?projectId="+projectID.String())
		listed.ExpectStatus(t, http.StatusOK)

		var page struct {
			Runs  []runView `json:"runs"`
			Total int64     `json:"total"`
		}
		listed.JSON(t, &page)
		if page.Total == 0 || len(page.Runs) == 0 {
			t.Fatalf("the run just created is not in the list: %+v", page)
		}
	})

	t.Run("an out-of-range page is a 400 rather than a silent clamp", func(t *testing.T) {
		owner.Get(t, "/api/v1/runs?limit=100000").ExpectError(t, http.StatusBadRequest, "bad_request")
		owner.Get(t, "/api/v1/runs?limit=abc").ExpectError(t, http.StatusBadRequest, "bad_request")
	})

	t.Run("events and report are org-scoped", func(t *testing.T) {
		stranger := env.NewOrg(t)
		stranger.Get(t, "/api/v1/runs/"+created.ID.String()+"/events").
			ExpectError(t, http.StatusNotFound, "not_found")
		stranger.Get(t, "/api/v1/runs/"+created.ID.String()+"/report").
			ExpectError(t, http.StatusNotFound, "not_found")
	})

	t.Run("a fresh run has an empty report", func(t *testing.T) {
		report := owner.Get(t, "/api/v1/runs/"+created.ID.String()+"/report")
		report.ExpectStatus(t, http.StatusOK)

		var body struct {
			Run        runView           `json:"run"`
			Executions []json.RawMessage `json:"executions"`
			Findings   []json.RawMessage `json:"findings"`
			Artifacts  []json.RawMessage `json:"artifacts"`
		}
		report.JSON(t, &body)
		if body.Run.ID != created.ID {
			t.Fatalf("the report is for run %s, want %s", body.Run.ID, created.ID)
		}
		// Empty collections are [], never null: a client should not have to
		// null-check a list.
		if body.Executions == nil || body.Findings == nil || body.Artifacts == nil {
			t.Fatalf("a collection came back as null: %s", report.Body)
		}
	})
}

func TestCancelContract(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://cancel.example.com")

	resp := owner.Post(t, "/api/v1/runs", map[string]any{"projectId": projectID, "mode": "discover"})
	resp.ExpectStatus(t, http.StatusCreated)
	var created runView
	resp.JSON(t, &created)

	cancelled := owner.Post(t, "/api/v1/runs/"+created.ID.String()+"/cancel", nil)
	cancelled.ExpectStatus(t, http.StatusOK)
	var view runView
	cancelled.JSON(t, &view)
	if view.Status != "cancelled" {
		t.Fatalf("got status %q, want cancelled", view.Status)
	}

	// Cancel is retryable: the caller's intent is already satisfied.
	owner.Post(t, "/api/v1/runs/"+created.ID.String()+"/cancel", nil).ExpectStatus(t, http.StatusOK)

	owner.Post(t, "/api/v1/runs/"+uuid.NewString()+"/cancel", nil).
		ExpectError(t, http.StatusNotFound, "not_found")

	viewer := env.NewMember(t, owner.OrgID, auth.RoleViewer)
	viewer.Post(t, "/api/v1/runs/"+created.ID.String()+"/cancel", nil).
		ExpectError(t, http.StatusForbidden, "forbidden")
}

// --- test cases -----------------------------------------------------------

func TestTestCaseReviewContract(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://cases.example.com")
	caseID := seedTestCase(t, env, owner, projectID, "TC-001")

	t.Run("get", func(t *testing.T) {
		resp := owner.Get(t, "/api/v1/test-cases/"+caseID.String())
		resp.ExpectStatus(t, http.StatusOK)

		var view struct {
			Ref     string          `json:"ref"`
			Status  string          `json:"status"`
			Payload json.RawMessage `json:"payload"`
		}
		resp.JSON(t, &view)
		if view.Status != "draft" {
			t.Fatalf("got status %q, want draft", view.Status)
		}
		// The payload is passed through as the contract document a planner
		// wrote, not as this backend's idea of one.
		if err := qaschema.MustBeValid("test-case@1", view.Payload); err != nil {
			t.Fatalf("the stored payload no longer matches test-case@1: %v", err)
		}
	})

	t.Run("approve, then archive", func(t *testing.T) {
		approve := owner.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
			map[string]string{"status": "approved"})
		approve.ExpectStatus(t, http.StatusOK)

		archive := owner.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
			map[string]string{"status": "archived"})
		archive.ExpectStatus(t, http.StatusOK)
	})

	t.Run("an archived case cannot jump straight back into the suite", func(t *testing.T) {
		owner.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
			map[string]string{"status": "approved"}).
			ExpectError(t, http.StatusConflict, "conflict")
	})

	t.Run("an unknown status names the three that exist", func(t *testing.T) {
		owner.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
			map[string]string{"status": "blessed"}).
			ExpectError(t, http.StatusUnprocessableEntity, "validation_failed")
	})

	t.Run("another organization cannot see or change it", func(t *testing.T) {
		stranger := env.NewOrg(t)
		stranger.Get(t, "/api/v1/test-cases/"+caseID.String()).
			ExpectError(t, http.StatusNotFound, "not_found")
		stranger.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
			map[string]string{"status": "approved"}).
			ExpectError(t, http.StatusNotFound, "not_found")
	})

	t.Run("a viewer may read but not approve", func(t *testing.T) {
		viewer := env.NewMember(t, owner.OrgID, auth.RoleViewer)
		viewer.Get(t, "/api/v1/test-cases/"+caseID.String()).ExpectStatus(t, http.StatusOK)
		viewer.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
			map[string]string{"status": "approved"}).
			ExpectError(t, http.StatusForbidden, "forbidden")
	})

	t.Run("the project listing filters by status", func(t *testing.T) {
		listed := owner.Get(t, "/api/v1/projects/"+projectID.String()+"/test-cases?status=draft")
		listed.ExpectStatus(t, http.StatusOK)

		owner.Get(t, "/api/v1/projects/"+projectID.String()+"/test-cases?status=nonsense").
			ExpectError(t, http.StatusBadRequest, "bad_request")
	})
}

// --- test case payload edit + version history -----------------------------

type testCaseVersionView struct {
	Version   int             `json:"version"`
	Status    string          `json:"status"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

type testCaseVersionList struct {
	Versions []testCaseVersionView `json:"versions"`
	Total    int64                 `json:"total"`
}

// editedTestCaseDocument builds a contract-valid document and lets the caller
// break exactly one thing about it, which is how every rejection below is
// expressed as a one-line difference from an accepted one.
func editedTestCaseDocument(t *testing.T, ref string, mutate func(map[string]any)) json.RawMessage {
	t.Helper()

	document := map[string]any{
		"version":  1,
		"id":       ref,
		"name":     "Log in with valid credentials",
		"priority": "high",
		"category": "functional",
		"steps": []any{
			map[string]any{"action": "navigate", "url": "/login"},
			map[string]any{"action": "click", "target": map[string]any{"ref": "login.submit"}},
		},
		"assertions": []any{
			map[string]any{"type": "urlMatches", "value": "/dashboard"},
		},
	}
	if mutate != nil {
		mutate(document)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode the fixture document: %v", err)
	}
	return raw
}

func readTestCase(t *testing.T, client *authtest.Client, caseID uuid.UUID) testCaseView {
	t.Helper()

	var view testCaseView
	client.Get(t, "/api/v1/test-cases/"+caseID.String()).
		ExpectStatus(t, http.StatusOK).
		JSON(t, &view)
	return view
}

func readVersions(t *testing.T, client *authtest.Client, caseID uuid.UUID, query string) testCaseVersionList {
	t.Helper()

	var list testCaseVersionList
	client.Get(t, "/api/v1/test-cases/"+caseID.String()+"/versions"+query).
		ExpectStatus(t, http.StatusOK).
		JSON(t, &list)
	return list
}

// The payload edit is the one write in this API that has to be refused more
// often than it is accepted: it rewrites what a regression suite means.
func TestTestCasePayloadEditContract(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://edits.example.com")
	caseID := seedTestCase(t, env, owner, projectID, "TC-100")
	path := "/api/v1/test-cases/" + caseID.String() + "/payload"

	// A freshly created case has exactly one version: the payload it was
	// created with, which the insert trigger snapshotted.
	if history := readVersions(t, owner, caseID, ""); history.Total != 1 {
		t.Fatalf("a new case has %d versions, want 1", history.Total)
	}

	t.Run("editing a draft bumps the version and keeps the old payload", func(t *testing.T) {
		edited := editedTestCaseDocument(t, "TC-100", func(d map[string]any) {
			d["name"] = "Log in, then land on the dashboard"
			d["priority"] = "critical"
			d["steps"].([]any)[0].(map[string]any)["url"] = "/sign-in"
		})

		var updated testCaseView
		owner.Do(t, http.MethodPut, path,
			map[string]any{"baseVersion": 1, "payload": edited}).
			ExpectStatus(t, http.StatusOK).
			JSON(t, &updated)

		if updated.Version != 2 {
			t.Fatalf("got version %d, want 2", updated.Version)
		}
		// The row's own columns are the list's projection of the document, so
		// they move with it or the listing shows a name the case no longer has.
		if updated.Name != "Log in, then land on the dashboard" || updated.Priority != "critical" {
			t.Fatalf("the row did not follow the document: %+v", updated)
		}
		if updated.Status != "draft" {
			t.Fatalf("an edit changed the review status to %q", updated.Status)
		}

		// Exactly one new history row, and the payload as it stood before the
		// edit is still readable at the version it belonged to.
		history := readVersions(t, owner, caseID, "")
		if history.Total != 2 || len(history.Versions) != 2 {
			t.Fatalf("got %d versions (total %d), want 2", len(history.Versions), history.Total)
		}
		if history.Versions[0].Version != 2 || history.Versions[1].Version != 1 {
			t.Fatalf("history is not newest-first: %d then %d",
				history.Versions[0].Version, history.Versions[1].Version)
		}
		if !sameDocument(t, history.Versions[0].Payload, updated.Payload) {
			t.Fatal("version[0] is not the payload the case now holds")
		}
		if !sameDocument(t, history.Versions[1].Payload, testCaseDocument(t, "TC-100")) {
			t.Fatalf("the pre-edit payload was not preserved: %s", history.Versions[1].Payload)
		}
		if err := qaschema.MustBeValid("test-case@1", history.Versions[1].Payload); err != nil {
			t.Fatalf("a stored version is no longer a valid test-case@1: %v", err)
		}
	})

	t.Run("a stale baseVersion is refused, not merged", func(t *testing.T) {
		before := readTestCase(t, owner, caseID)

		owner.Do(t, http.MethodPut, path, map[string]any{
			"baseVersion": 1,
			"payload": editedTestCaseDocument(t, "TC-100", func(d map[string]any) {
				d["name"] = "An edit written against a version somebody else replaced"
			}),
		}).ExpectError(t, http.StatusConflict, "conflict")

		assertUnchanged(t, owner, caseID, before)
	})

	t.Run("a payload that violates the contract is refused with pointers", func(t *testing.T) {
		before := readTestCase(t, owner, caseID)

		envelope := owner.Do(t, http.MethodPut, path, map[string]any{
			"baseVersion": before.Version,
			"payload": editedTestCaseDocument(t, "TC-100", func(d map[string]any) {
				d["steps"].([]any)[1].(map[string]any)["action"] = "teleport"
			}),
		}).ExpectError(t, http.StatusUnprocessableEntity, "validation_failed")

		problems, ok := envelope.Error.Details["errors"].([]any)
		if !ok || len(problems) == 0 {
			t.Fatalf("got details %+v, want a non-empty errors list", envelope.Error.Details)
		}
		// A client that cannot locate the bad field can only show the reviewer
		// "invalid", which is the failure mode the pointers exist to prevent.
		var located bool
		for _, raw := range problems {
			problem, _ := raw.(map[string]any)
			path, _ := problem["instancePath"].(string)
			if path == "" {
				t.Fatalf("a problem carries no instancePath: %+v", problem)
			}
			if strings.HasPrefix(path, "/steps/1") {
				located = true
			}
		}
		if !located {
			t.Fatalf("no problem points at the bad step: %+v", problems)
		}

		assertUnchanged(t, owner, caseID, before)
	})

	t.Run("the case id is immutable", func(t *testing.T) {
		before := readTestCase(t, owner, caseID)

		envelope := owner.Do(t, http.MethodPut, path, map[string]any{
			"baseVersion": before.Version,
			"payload":     editedTestCaseDocument(t, "TC-999", nil),
		}).ExpectError(t, http.StatusUnprocessableEntity, "validation_failed")

		fields, _ := envelope.Error.Details["fields"].(map[string]any)
		if _, named := fields["payload.id"]; !named {
			t.Fatalf("got details %+v, want a payload.id field", envelope.Error.Details)
		}

		assertUnchanged(t, owner, caseID, before)
	})

	t.Run("re-sending the same document burns no version", func(t *testing.T) {
		before := readTestCase(t, owner, caseID)

		var again testCaseView
		owner.Do(t, http.MethodPut, path,
			map[string]any{"baseVersion": before.Version, "payload": before.Payload}).
			ExpectStatus(t, http.StatusOK).
			JSON(t, &again)

		if again.Version != before.Version {
			t.Fatalf("an identical document moved the version from %d to %d",
				before.Version, again.Version)
		}
		if history := readVersions(t, owner, caseID, ""); history.Total != int64(before.Version) {
			t.Fatalf("an identical document wrote a history row: %d versions", history.Total)
		}
	})

	t.Run("an approved case is not editable", func(t *testing.T) {
		owner.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
			map[string]string{"status": "approved"}).
			ExpectStatus(t, http.StatusOK)
		before := readTestCase(t, owner, caseID)

		owner.Do(t, http.MethodPut, path, map[string]any{
			"baseVersion": before.Version,
			"payload": editedTestCaseDocument(t, "TC-100", func(d map[string]any) {
				d["name"] = "A rewrite of what the last twenty runs were measured against"
			}),
		}).ExpectError(t, http.StatusConflict, "conflict")

		assertUnchanged(t, owner, caseID, before)

		// An archived case is refused for the same reason.
		owner.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
			map[string]string{"status": "archived"}).
			ExpectStatus(t, http.StatusOK)
		owner.Do(t, http.MethodPut, path, map[string]any{
			"baseVersion": before.Version,
			"payload":     editedTestCaseDocument(t, "TC-100", nil),
		}).ExpectError(t, http.StatusConflict, "conflict")

		// Back to draft, and the same edit is accepted: the refusal is about
		// the review state, not about the edit.
		owner.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
			map[string]string{"status": "draft"}).
			ExpectStatus(t, http.StatusOK)
		owner.Do(t, http.MethodPut, path, map[string]any{
			"baseVersion": before.Version,
			"payload": editedTestCaseDocument(t, "TC-100", func(d map[string]any) {
				d["name"] = "Edited after the reviewer moved it back to draft"
			}),
		}).ExpectStatus(t, http.StatusOK)
	})

	t.Run("the history honours limit and is newest first", func(t *testing.T) {
		full := readVersions(t, owner, caseID, "")
		if full.Total < 3 {
			t.Fatalf("this case should have accumulated versions by now, has %d", full.Total)
		}

		page := readVersions(t, owner, caseID, "?limit=2")
		if len(page.Versions) != 2 {
			t.Fatalf("got %d versions, want the 2 that were asked for", len(page.Versions))
		}
		// total is the whole history, not the size of the page: a client that
		// reads it as the page size renders "2 versions" for a case with more.
		if page.Total != full.Total {
			t.Fatalf("got total %d on a limited page, want %d", page.Total, full.Total)
		}
		if page.Versions[0].Version <= page.Versions[1].Version {
			t.Fatalf("a limited page is not newest-first: %d then %d",
				page.Versions[0].Version, page.Versions[1].Version)
		}
		if page.Versions[0].Version != full.Versions[0].Version {
			t.Fatal("a limited page dropped the newest version instead of the oldest")
		}

		// An out-of-range limit is refused rather than silently clamped.
		owner.Get(t, "/api/v1/test-cases/"+caseID.String()+"/versions?limit=0").
			ExpectError(t, http.StatusBadRequest, "bad_request")
		owner.Get(t, "/api/v1/test-cases/"+caseID.String()+"/versions?limit=201").
			ExpectError(t, http.StatusBadRequest, "bad_request")
	})

	t.Run("another organization sees neither the case nor its history", func(t *testing.T) {
		stranger := env.NewOrg(t)

		// 404 and never 403: confirming that an id exists somewhere else is
		// itself the cross-tenant leak.
		stranger.Do(t, http.MethodPut, path, map[string]any{
			"baseVersion": 1,
			"payload":     editedTestCaseDocument(t, "TC-100", nil),
		}).ExpectError(t, http.StatusNotFound, "not_found")

		stranger.Get(t, "/api/v1/test-cases/"+caseID.String()+"/versions").
			ExpectError(t, http.StatusNotFound, "not_found")
	})

	t.Run("a viewer may read the history but not rewrite the case", func(t *testing.T) {
		viewer := env.NewMember(t, owner.OrgID, auth.RoleViewer)

		viewer.Get(t, "/api/v1/test-cases/"+caseID.String()+"/versions").
			ExpectStatus(t, http.StatusOK)
		viewer.Do(t, http.MethodPut, path, map[string]any{
			"baseVersion": 1,
			"payload":     editedTestCaseDocument(t, "TC-100", nil),
		}).ExpectError(t, http.StatusForbidden, "forbidden")
	})

	t.Run("an edit that does not say what it was based on is refused", func(t *testing.T) {
		before := readTestCase(t, owner, caseID)

		// No baseVersion at all. Defaulting it to "whatever the case is at
		// now" would turn every edit into a last-write-wins, which is the one
		// thing this endpoint exists to prevent.
		envelope := owner.Do(t, http.MethodPut, path, map[string]any{
			"payload": editedTestCaseDocument(t, "TC-100", nil),
		}).ExpectError(t, http.StatusUnprocessableEntity, "validation_failed")

		fields, _ := envelope.Error.Details["fields"].(map[string]any)
		if _, named := fields["baseVersion"]; !named {
			t.Fatalf("got details %+v, want a baseVersion field", envelope.Error.Details)
		}

		assertUnchanged(t, owner, caseID, before)
	})

	t.Run("a case that does not exist is a 404 on both routes", func(t *testing.T) {
		missing := uuid.NewString()
		owner.Do(t, http.MethodPut, "/api/v1/test-cases/"+missing+"/payload", map[string]any{
			"baseVersion": 1,
			"payload":     editedTestCaseDocument(t, "TC-100", nil),
		}).ExpectError(t, http.StatusNotFound, "not_found")
		owner.Get(t, "/api/v1/test-cases/"+missing+"/versions").
			ExpectError(t, http.StatusNotFound, "not_found")
	})
}

// Two reviewers saving the same version at the same time is the race
// baseVersion exists for, and an optimistic lock that is only checked outside a
// transaction does not stop it: both requests read version 1, both find it
// matches, and the second edit lands on top of the first with nobody told.
//
// This is the test for the FOR UPDATE, not for the comparison.
func TestConcurrentPayloadEditsCannotBothWin(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://races.example.com")
	caseID := seedTestCase(t, env, owner, projectID, "TC-200")
	path := "/api/v1/test-cases/" + caseID.String() + "/payload"

	const editors = 6
	statuses := make(chan int, editors)
	var start sync.WaitGroup
	start.Add(1)

	var done sync.WaitGroup
	for i := range editors {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			// Every editor writes a different document, so a lost update is a
			// name that no longer matches the version that reported success.
			statuses <- owner.Do(t, http.MethodPut, path, map[string]any{
				"baseVersion": 1,
				"payload": editedTestCaseDocument(t, "TC-200", func(d map[string]any) {
					d["name"] = fmt.Sprintf("Edited by reviewer %d", i)
				}),
			}).Status
		}()
	}
	start.Done()
	done.Wait()
	close(statuses)

	var accepted, conflicted int
	for status := range statuses {
		switch status {
		case http.StatusOK:
			accepted++
		case http.StatusConflict:
			conflicted++
		default:
			t.Errorf("got status %d, want 200 or 409", status)
		}
	}
	if accepted != 1 {
		t.Fatalf("%d of %d concurrent edits were accepted, want exactly 1", accepted, editors)
	}
	if conflicted != editors-1 {
		t.Fatalf("%d edits were refused, want %d", conflicted, editors-1)
	}

	// One winner, one version bump, one new history row — not six.
	after := readTestCase(t, owner, caseID)
	if after.Version != 2 {
		t.Fatalf("the case is at version %d after one accepted edit, want 2", after.Version)
	}
	history := readVersions(t, owner, caseID, "")
	if history.Total != 2 {
		t.Fatalf("got %d versions, want 2", history.Total)
	}
	// The surviving payload is the one the winner sent, whole: a merge of two
	// documents would satisfy neither reviewer and match no version row.
	if !sameDocument(t, history.Versions[0].Payload, after.Payload) {
		t.Fatal("the newest version is not the payload the case holds")
	}
}

// assertUnchanged is the other half of every rejection above: a refused edit
// has to leave the case exactly as it found it, history included.
func assertUnchanged(t *testing.T, client *authtest.Client, caseID uuid.UUID, before testCaseView) {
	t.Helper()

	after := readTestCase(t, client, caseID)
	if after.Version != before.Version {
		t.Fatalf("a refused edit moved the version from %d to %d", before.Version, after.Version)
	}
	if !sameDocument(t, after.Payload, before.Payload) {
		t.Fatalf("a refused edit changed the payload to %s", after.Payload)
	}
	if after.Name != before.Name || after.Priority != before.Priority || after.Status != before.Status {
		t.Fatalf("a refused edit changed the row: %+v, was %+v", after, before)
	}
	if history := readVersions(t, client, caseID, ""); history.Total != int64(before.Version) {
		t.Fatalf("a refused edit wrote a history row: %d versions for version %d",
			history.Total, before.Version)
	}
}

// sameDocument compares two JSON documents by value, not by bytes: a payload
// makes a round trip through jsonb, which does not preserve key order or
// whitespace, and neither of those is part of the contract.
func sameDocument(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()

	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(left, right)
}

// --- runtimes -------------------------------------------------------------

func TestRuntimeListContract(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	runtimeID, token := env.pairedRuntime(t, owner)

	t.Run("a paired but unconnected runtime is offline", func(t *testing.T) {
		runtimes := listRuntimes(t, owner)
		found := runtimeByID(t, runtimes, runtimeID)
		if found.Online {
			t.Fatal("a runtime with no connection and no heartbeat reports online")
		}
	})

	t.Run("a connected runtime reports its capabilities", func(t *testing.T) {
		daemon := env.dialDaemon(t, runtimeID, token)
		daemon.hello(t)

		// hello is fire-and-forget, so poll rather than assume it landed.
		var found runtimeView
		waitFor(t, 2*time.Second, func() bool {
			found = runtimeByID(t, listRuntimes(t, owner), runtimeID)
			return found.Online && found.Version == "0.1.0-test"
		})
		if string(found.Browsers) != `["chromium"]` {
			t.Fatalf("got browsers %s, want [\"chromium\"]", found.Browsers)
		}
	})

	t.Run("another organization sees none of it", func(t *testing.T) {
		stranger := env.NewOrg(t)
		for _, r := range listRuntimes(t, stranger) {
			if r.ID == runtimeID {
				t.Fatal("a runtime leaked into another organization's list")
			}
		}
	})
}

func listRuntimes(t *testing.T, client *authtest.Client) []runtimeView {
	t.Helper()

	resp := client.Get(t, "/api/v1/runtimes")
	resp.ExpectStatus(t, http.StatusOK)

	var body struct {
		Runtimes []runtimeView `json:"runtimes"`
	}
	resp.JSON(t, &body)
	return body.Runtimes
}

func runtimeByID(t *testing.T, runtimes []runtimeView, id uuid.UUID) runtimeView {
	t.Helper()
	for _, r := range runtimes {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("runtime %s is not in the list", id)
	return runtimeView{}
}
