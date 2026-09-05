package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/executor"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/workspace"
)

// fakeStorage is MinIO's PUT surface: the daemon uploads evidence to it
// directly with the presigned credentials from run.assign (ADR-002).
type fakeStorage struct {
	srv *httptest.Server

	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeStorage(t *testing.T) *fakeStorage {
	t.Helper()
	fs := &fakeStorage{objects: map[string][]byte{}}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "only PUT", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("sig") == "" {
			// A real presigned URL carries its signature in the query string;
			// losing it while building the object URL is a silent 403 in
			// production, so the fake refuses it here instead.
			http.Error(w, "unsigned request", http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fs.mu.Lock()
		fs.objects[strings.TrimPrefix(r.URL.Path, "/bucket/")] = body
		fs.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeStorage) PutBase() string { return fs.srv.URL + "/bucket?sig=abc" }

func (fs *fakeStorage) Object(key string) ([]byte, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	body, ok := fs.objects[key]
	return body, ok
}

func (fs *fakeStorage) Keys() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	keys := make([]string, 0, len(fs.objects))
	for key := range fs.objects {
		keys = append(keys, key)
	}
	return keys
}

// writeEvidence is what the executor does: it writes files into the artifact
// directory the daemon gave it and reports keys under the run's prefix.
func writeEvidence(t *testing.T, params executor.TestcaseRunParams, files map[string]string) qaschema.ExecutionResult {
	t.Helper()

	artifacts := make([]qaschema.Artifact, 0, len(files))
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(params.ArtifactDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write evidence: %v", err)
		}
		kind := qaschema.ArtifactKindScreenshot
		if strings.HasSuffix(name, ".zip") {
			kind = qaschema.ArtifactKindTrace
		}
		artifacts = append(artifacts, qaschema.Artifact{
			ID:   "art-" + strings.ReplaceAll(name, ".", "-"),
			Kind: kind,
			Key:  params.StorageKeyPrefix + params.TestCase.ID + "/" + name,
		})
	}

	now := time.Now().UTC().Format(time.RFC3339)
	return qaschema.ExecutionResult{
		Version:    1,
		TestCaseID: params.TestCase.ID,
		Result:     qaschema.OutcomePass,
		Steps:      []qaschema.StepResult{},
		Artifacts:  artifacts,
		StartedAt:  now,
		EndedAt:    now,
	}
}

// failWith is writeEvidence's failing counterpart: a case that failed on its
// first step, with a message the executor itself wrote.
//
// The analysis phase only looks at failures, so a test that wants a finding
// has to produce one of these. A passing run has nothing to explain.
func failWith(t *testing.T, params executor.TestcaseRunParams, message string, files map[string]string) qaschema.ExecutionResult {
	t.Helper()

	result := writeEvidence(t, params, files)
	result.Result = qaschema.OutcomeFail
	result.Message = ptr(message)
	result.Steps = []qaschema.StepResult{{
		Index:   0,
		Action:  qaschema.StepActionClick,
		Status:  qaschema.OutcomeFail,
		Message: ptr(message),
	}}
	return result
}

// The acceptance criterion: evidence the executor produced reaches storage,
// and run.result names artifacts that can actually be fetched.
func TestRunUploadsEvidenceAndReportsIt(t *testing.T) {
	storage := newFakeStorage(t)
	h := newHarness(t, harnessOptions{})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return writeEvidence(t, params, map[string]string{
			"screenshot-1.png": "png-bytes",
			"trace.zip":        "trace-bytes",
		})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	assign := assignFrame(t, assignOptions{
		withMap:   true,
		putBase:   storage.PutBase(),
		testCases: []any{testCase("TC-001", "Create employee")},
	})
	h.backend.Send(assign)
	prefix := decodeAs[qaschema.RunAssignPayload](t, assign.Payload).ArtifactUpload.KeyPrefix

	result := h.backend.ExpectType(15*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusCompleted {
		t.Fatalf("status = %q, error = %+v", payload.Status, payload.Error)
	}
	if len(payload.Artifacts) != 2 {
		t.Fatalf("run.result reported %d artifacts, want 2: %+v", len(payload.Artifacts), payload.Artifacts)
	}

	for _, artifact := range payload.Artifacts {
		if !strings.HasPrefix(artifact.Key, prefix) {
			t.Fatalf("artifact key %q is outside the run prefix %q", artifact.Key, prefix)
		}
		body, ok := storage.Object(artifact.Key)
		if !ok {
			t.Fatalf("run.result names %s but storage has %v", artifact.Key, storage.Keys())
		}
		if artifact.Sha256 == nil {
			t.Fatalf("artifact %s has no digest", artifact.Key)
		}
		want := sha256.Sum256(body)
		if *artifact.Sha256 != hex.EncodeToString(want[:]) {
			t.Fatalf("artifact %s digest does not match the stored object", artifact.Key)
		}
		if artifact.SizeBytes == nil || *artifact.SizeBytes != len(body) {
			t.Fatalf("artifact %s size = %v, stored %d bytes", artifact.Key, artifact.SizeBytes, len(body))
		}
		if artifact.ID == "" {
			t.Fatalf("artifact %s has no run-local id for findings to point at", artifact.Key)
		}
	}
}

// An executor that reports a key belonging to another run must not be able to
// make this daemon write there: the prefix is the tenant boundary.
func TestRunRefusesEvidenceOutsideItsPrefix(t *testing.T) {
	storage := newFakeStorage(t)
	h := newHarness(t, harnessOptions{})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		result := writeEvidence(t, params, map[string]string{"screenshot-1.png": "png"})
		result.Artifacts[0].Key = "orgs/someone-else/runs/2026-09-04/otherrun/TC-001/screenshot-1.png"
		return result
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		withMap:   true,
		putBase:   storage.PutBase(),
		testCases: []any{testCase("TC-001", "Create employee")},
	}))

	result := h.backend.ExpectType(15*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusFailed {
		t.Fatalf("status = %q, want failed", payload.Status)
	}
	if payload.Error == nil || payload.Error.Code != qaschema.RunErrorCodeArtifactUploadFailed {
		t.Fatalf("error = %+v", payload.Error)
	}
	if keys := storage.Keys(); len(keys) != 0 {
		t.Fatalf("something was written outside this run's prefix: %v", keys)
	}
}

// Two runs on the same machine must not see each other's evidence, and a
// finished run's workspace must be swept once retention says so.
func TestWorkspacesAreIsolatedAndSwept(t *testing.T) {
	root := t.TempDir()
	storage := newFakeStorage(t)
	h := newHarness(t, harnessOptions{
		workspaceRoot: root,
		// Long enough that the first run's workspace is still there when the
		// second finishes, short enough that it is gone afterwards.
		retention: workspace.Retention{KeepCompleted: time.Nanosecond, KeepFailed: time.Hour},
	})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return writeEvidence(t, params, map[string]string{"screenshot-1.png": params.TestCase.ID})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)

	first := assignFrame(t, assignOptions{
		withMap: true, putBase: storage.PutBase(),
		testCases: []any{testCase("TC-001", "First run case")},
	})
	h.backend.Send(first)
	h.backend.ExpectType(15*time.Second, qaschema.EnvelopeTypeRunResult)

	firstPayload := decodeAs[qaschema.RunAssignPayload](t, first.Payload)
	firstDir := filepath.Join(root, firstPayload.ProjectID, firstPayload.RunID)

	// The completed workspace is swept by the next run's sweep, per retention.
	second := assignFrame(t, assignOptions{
		withMap: true, putBase: storage.PutBase(),
		testCases: []any{testCase("TC-002", "Second run case")},
	})
	h.backend.Send(second)
	h.backend.ExpectType(15*time.Second, qaschema.EnvelopeTypeRunResult)

	secondPayload := decodeAs[qaschema.RunAssignPayload](t, second.Payload)
	secondDir := filepath.Join(root, secondPayload.ProjectID, secondPayload.RunID)
	if firstDir == secondDir {
		t.Fatal("two runs shared a workspace")
	}

	waitFor(t, 10*time.Second, func() bool {
		_, err := os.Stat(firstDir)
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(firstDir); !os.IsNotExist(err) {
		t.Fatalf("the completed workspace was not swept: %v", err)
	}
}

// A daemon that restarted mid-run and was re-assigned the same run must not
// replay a case that already executed — the ledger, not memory, is what
// carries that across a restart.
func TestLedgerPreventsReplayAfterARestart(t *testing.T) {
	root := t.TempDir()
	storage := newFakeStorage(t)
	assign := assignFrame(t, assignOptions{
		withMap: true, putBase: storage.PutBase(),
		testCases: []any{testCase("TC-001", "Create employee"), testCase("TC-002", "Edit employee")},
	})
	payload := decodeAs[qaschema.RunAssignPayload](t, assign.Payload)

	// First daemon: runs TC-001, then dies before TC-002 (its executor fails
	// the way a killed sidecar would).
	first := newHarness(t, harnessOptions{workspaceRoot: root})
	first.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return writeEvidence(t, params, map[string]string{"screenshot-1.png": params.TestCase.ID})
	}
	first.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	first.backend.Send(assign)
	first.backend.ExpectType(15*time.Second, qaschema.EnvelopeTypeRunResult)
	first.Stop()

	if ran := first.executor.RanCases(); len(ran) != 2 {
		t.Fatalf("first daemon ran %v", ran)
	}

	// Second daemon, same machine, same workspace root, same run.
	second := newHarness(t, harnessOptions{workspaceRoot: root})
	second.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return writeEvidence(t, params, map[string]string{"screenshot-1.png": params.TestCase.ID})
	}
	second.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	second.backend.Send(assign)

	result := second.backend.ExpectType(15*time.Second, qaschema.EnvelopeTypeRunResult)
	got := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if got.Status != qaschema.RunResultPayloadStatusCompleted {
		t.Fatalf("status = %q, error = %+v", got.Status, got.Error)
	}
	if len(got.Executions) != 2 {
		t.Fatalf("executions = %d, want both cases reported from the ledger", len(got.Executions))
	}
	if ran := second.executor.RanCases(); len(ran) != 0 {
		t.Fatalf("the restarted daemon re-ran %v; the ledger should have covered them", ran)
	}

	ledger := filepath.Join(root, payload.ProjectID, payload.RunID, ledgerFile)
	if _, err := os.Stat(ledger); err != nil {
		t.Fatalf("no ledger at %s: %v", ledger, err)
	}
}

func TestLedgerSurvivesATruncatedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ledgerFile)

	if err := appendLedger(path, qaschema.ExecutionResult{
		Version: 1, TestCaseID: "TC-001", Result: qaschema.OutcomePass,
		Steps: []qaschema.StepResult{}, Artifacts: []qaschema.Artifact{},
		StartedAt: "2026-09-04T12:00:00Z", EndedAt: "2026-09-04T12:00:01Z",
	}); err != nil {
		t.Fatalf("appendLedger: %v", err)
	}
	// The tail of a crash: a partially written line.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := file.WriteString(`{"version":1,"testCaseId":"TC-0`); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = file.Close()

	done, err := readLedger(path)
	if err != nil {
		t.Fatalf("readLedger: %v", err)
	}
	if len(done) != 1 || done["TC-001"].Result != qaschema.OutcomePass {
		t.Fatalf("ledger = %+v, want the complete entry to survive", done)
	}
}

func TestReadLedgerOnAFreshRun(t *testing.T) {
	done, err := readLedger(filepath.Join(t.TempDir(), ledgerFile))
	if err != nil {
		t.Fatalf("readLedger: %v", err)
	}
	if len(done) != 0 {
		t.Fatalf("ledger = %+v", done)
	}
}

// A case the executor could not run at all still appears in the report, so a
// 40-case run never comes back with 39 results and no explanation.
func TestTransportFailureBecomesAnErrorResult(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.executor.runErr = &executor.Error{Code: "TIMEOUT", Message: "step timed out"}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		withMap:   true,
		testCases: []any{testCase("TC-001", "Create employee")},
	}))

	result := h.backend.ExpectType(15*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if len(payload.Executions) != 1 {
		t.Fatalf("executions = %d", len(payload.Executions))
	}
	execution := payload.Executions[0]
	if execution.Result != qaschema.OutcomeError {
		t.Fatalf("result = %q", execution.Result)
	}
	if execution.FailureClass == nil || *execution.FailureClass != qaschema.FailureClassTIMEOUT {
		t.Fatalf("failureClass = %v, want TIMEOUT", execution.FailureClass)
	}
}

func TestFailureClassMapping(t *testing.T) {
	tests := map[string]qaschema.FailureClass{
		"BROWSER_LAUNCH_FAILED":  qaschema.FailureClassENVIRONMENTERROR,
		"FIXTURE_UNAVAILABLE":    qaschema.FailureClassENVIRONMENTERROR,
		"NETWORK_ERROR":          qaschema.FailureClassNETWORKERROR,
		"TIMEOUT":                qaschema.FailureClassTIMEOUT,
		"TARGET_NOT_FOUND":       qaschema.FailureClassTESTBUG,
		"UNKNOWN_ASSERTION_TYPE": qaschema.FailureClassTESTBUG,
		"INTERNAL":               qaschema.FailureClassUNKNOWN,
		"something-new":          qaschema.FailureClassUNKNOWN,
	}
	for code, want := range tests {
		if got := failureClassFor(code); got != want {
			t.Fatalf("failureClassFor(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestPhasesForEveryMode(t *testing.T) {
	tests := map[qaschema.RunAssignPayloadMode]int{
		qaschema.RunAssignPayloadModeDiscover: 1,
		qaschema.RunAssignPayloadModePlan:     1,
		qaschema.RunAssignPayloadModeExecute:  1,
		qaschema.RunAssignPayloadModeFull:     4,
	}
	for mode, want := range tests {
		if got := phasesFor(mode); len(got) != want {
			t.Fatalf("phasesFor(%q) has %d phases, want %d", mode, len(got), want)
		}
	}
	if phasesFor("nonsense") != nil {
		t.Fatal("an unknown mode must not resolve to a phase list")
	}
}
