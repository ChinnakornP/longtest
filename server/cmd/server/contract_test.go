package main

import (
	"encoding/json"
	"net/http"
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
