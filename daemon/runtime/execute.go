package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/ChinnakornP/longtest/daemon/artifacts"
	"github.com/ChinnakornP/longtest/daemon/executor"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/workspace"
)

// ledgerFile records which test cases of this run already executed.
//
// The registry keeps a reconnect from re-running a case, but it lives in
// memory: a daemon that was restarted mid-run and re-assigned the same run
// would otherwise replay every case, and "create employee" is not a step you
// want replayed. The ledger is per-run and lives beside the evidence.
const ledgerFile = "execution-ledger.jsonl"

// runTestCases executes every test case and uploads its evidence.
//
// Evidence is uploaded per case rather than at the end: a run that fails on
// case 40 must still leave the first 39 screenshots in storage, because those
// are what an operator looks at to work out what changed.
func (rc *runController) runTestCases(
	ctx context.Context,
	ws *workspace.Workspace,
	appMap qaschema.ApplicationMap,
	testCases []qaschema.TestCase,
	uploader *artifacts.Uploader,
) ([]qaschema.ExecutionResult, []qaschema.Artifact, error) {
	d := rc.daemon

	execDir, err := ws.PhaseDir(workspace.PhaseExecution)
	if err != nil {
		return nil, nil, failure(qaschema.RunErrorCodeInternal, err, "could not resolve the execution workspace")
	}

	ledgerPath := filepath.Join(ws.Dir(), ledgerFile)
	done, err := readLedger(ledgerPath)
	if err != nil {
		rc.logger.Warn("could not read the execution ledger; treating the run as fresh", "error", err)
		done = map[string]qaschema.ExecutionResult{}
	}

	client, err := d.deps.NewExecutor(executor.Options{
		Command: d.cfg.Executor(),
		Dir:     execDir,
		Logger:  rc.logger,
		OnEvent: rc.onExecutorEvent,
	})
	if err != nil {
		return nil, nil, failure(qaschema.RunErrorCodeBrowserLaunchFailed, err,
			"could not start the executor sidecar (%v)", d.cfg.Executor())
	}
	// WithoutCancel so teardown still runs when the run was cancelled: that is
	// exactly the case where the process tree must die.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.deps.CancelGrace)
		defer cancel()
		if err := client.Close(closeCtx, d.deps.CancelGrace); err != nil {
			rc.logger.Warn("executor teardown reported an error", "error", err)
		}
	}()

	if _, err := client.SessionOpen(ctx, executor.SessionOpenParams{BaseURL: rc.payload.BaseURL}); err != nil {
		return nil, nil, sessionOpenFailure(err)
	}

	executions := make([]qaschema.ExecutionResult, 0, len(testCases))
	uploaded := make([]qaschema.Artifact, 0, len(testCases))

	for _, testCase := range testCases {
		if err := ctx.Err(); err != nil {
			return executions, uploaded, err
		}

		if previous, ok := done[testCase.ID]; ok {
			rc.emitForCase(qaschema.RunEventPayloadLevelInfo, "test_case_skipped",
				"already executed in an earlier attempt of this run", testCase.ID, nil)
			executions = append(executions, previous)
			continue
		}

		rc.emitForCase(qaschema.RunEventPayloadLevelInfo, "test_case_started", testCase.Name, testCase.ID, nil)

		artifactDir, err := ws.MkdirAll(workspace.PhaseExecution, testCase.ID)
		if err != nil {
			return executions, uploaded, failure(qaschema.RunErrorCodeInternal, err,
				"could not create an evidence directory for %s", testCase.ID)
		}

		result, err := client.RunTestCase(ctx, executor.TestcaseRunParams{
			TestCase:         testCase,
			AppMap:           appMap,
			ArtifactDir:      artifactDir,
			StorageKeyPrefix: rc.payload.ArtifactUpload.KeyPrefix,
			RunID:            rc.payload.RunID,
		})
		if err != nil {
			if ctx.Err() != nil {
				return executions, uploaded, ctx.Err()
			}
			// A transport-level failure is still a result for this case: the
			// alternative is a run that reports 39 of 40 cases with no record
			// of what happened to the fortieth.
			result = rc.synthesiseFailure(testCase, err)
		}

		executions = append(executions, result)
		if err := appendLedger(ledgerPath, result); err != nil {
			rc.logger.Warn("could not update the execution ledger", "error", err)
		}

		caseArtifacts, err := rc.uploadEvidence(ctx, uploader, artifactDir, result)
		if err != nil {
			if ctx.Err() != nil {
				return executions, uploaded, ctx.Err()
			}
			return executions, uploaded, err
		}
		uploaded = append(uploaded, caseArtifacts...)

		rc.emitForCase(levelFor(result.Result), "test_case_finished", string(result.Result), testCase.ID,
			map[string]any{"outcome": string(result.Result), "artifacts": len(caseArtifacts)})
	}

	if err := client.SessionClose(ctx); err != nil {
		rc.logger.Warn("session.close reported an error", "error", err)
	}
	return executions, uploaded, nil
}

// uploadEvidence puts the files the executor wrote into object storage and
// returns the artifact records for run.result.
func (rc *runController) uploadEvidence(
	ctx context.Context,
	uploader *artifacts.Uploader,
	artifactDir string,
	result qaschema.ExecutionResult,
) ([]qaschema.Artifact, error) {
	if len(result.Artifacts) == 0 {
		return nil, nil
	}

	uploads := make([]artifacts.Upload, 0, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		if err := artifacts.KeyWithinPrefix(uploader.Prefix(), artifact.Key); err != nil {
			// The executor built this key from the prefix we handed it, so a
			// mismatch means the document did not come from this run.
			return nil, failure(qaschema.RunErrorCodeArtifactUploadFailed, err,
				"the executor reported an artifact key outside this run's storage prefix")
		}
		name := path.Base(artifact.Key)
		local := filepath.Join(artifactDir, name)
		if _, err := os.Stat(local); err != nil {
			rc.logger.Warn("skipping an artifact the executor did not write", "key", artifact.Key, "error", err)
			continue
		}
		upload := artifacts.Upload{Key: artifact.Key, Path: local, Kind: artifact.Kind, ID: artifact.ID}
		if artifact.ContentType != nil {
			upload.ContentType = *artifact.ContentType
		}
		uploads = append(uploads, upload)
	}

	stored, err := uploader.UploadAll(ctx, uploads)
	if err != nil {
		if errors.Is(err, artifacts.ErrPrefixExpired) {
			// ADR-002 classes an expired window as an environment error: the
			// fix is fresh credentials, which arrive with the next assignment.
			return stored, failure(qaschema.RunErrorCodeArtifactUploadFailed, err,
				"the presigned upload window expired before this run finished uploading its evidence")
		}
		return stored, failure(qaschema.RunErrorCodeArtifactUploadFailed, err, "could not upload run evidence")
	}
	for _, artifact := range stored {
		rc.emitForCase(qaschema.RunEventPayloadLevelDebug, "artifact_uploaded", artifact.Key, result.TestCaseID,
			map[string]any{"artifactId": artifact.ID, "kind": string(artifact.Kind)})
	}
	return stored, nil
}

// synthesiseFailure turns an executor transport failure into an execution
// result, so every assigned case appears in the report exactly once.
func (rc *runController) synthesiseFailure(testCase qaschema.TestCase, err error) qaschema.ExecutionResult {
	now := rc.daemon.now().UTC().Format(time.RFC3339)
	message := "the executor could not run this test case"

	failureClass := qaschema.FailureClassUNKNOWN
	var rpcErr *executor.Error
	if errors.As(err, &rpcErr) {
		failureClass = failureClassFor(rpcErr.Code)
		message = fmt.Sprintf("the executor reported %s", rpcErr.Code)
	}
	rc.logger.Error("test case could not be executed", "testCaseId", testCase.ID, "error", err)

	return qaschema.ExecutionResult{
		Version:      1,
		TestCaseID:   testCase.ID,
		RunID:        &rc.payload.RunID,
		Result:       qaschema.OutcomeError,
		FailureClass: &failureClass,
		Message:      &message,
		Steps:        []qaschema.StepResult{},
		Artifacts:    []qaschema.Artifact{},
		StartedAt:    now,
		EndedAt:      now,
	}
}

// failureClassFor maps the sidecar's error vocabulary onto the platform's
// failure classification. Getting this wrong is what turns "your environment
// is broken" into "your product has a bug".
func failureClassFor(code string) qaschema.FailureClass {
	switch code {
	case "BROWSER_LAUNCH_FAILED", "FIXTURE_UNAVAILABLE":
		return qaschema.FailureClassENVIRONMENTERROR
	case "NETWORK_ERROR":
		return qaschema.FailureClassNETWORKERROR
	case "TIMEOUT":
		return qaschema.FailureClassTIMEOUT
	case "TARGET_NOT_FOUND", "UNKNOWN_ACTION", "UNKNOWN_ASSERTION_TYPE", "INVALID_PARAMS":
		return qaschema.FailureClassTESTBUG
	default:
		return qaschema.FailureClassUNKNOWN
	}
}

func sessionOpenFailure(err error) error {
	var rpcErr *executor.Error
	if errors.As(err, &rpcErr) {
		switch rpcErr.Code {
		case "BROWSER_LAUNCH_FAILED":
			return failure(qaschema.RunErrorCodeBrowserLaunchFailed, err, "the executor could not launch a browser")
		case "NETWORK_ERROR", "TIMEOUT":
			return failure(qaschema.RunErrorCodeTargetUnreachable, err, "the application under test did not respond")
		}
	}
	return failure(qaschema.RunErrorCodeBrowserLaunchFailed, err, "the executor could not open a browser session")
}

func levelFor(outcome qaschema.Outcome) qaschema.RunEventPayloadLevel {
	switch outcome {
	case qaschema.OutcomePass:
		return qaschema.RunEventPayloadLevelInfo
	case qaschema.OutcomeSkipped:
		return qaschema.RunEventPayloadLevelWarn
	default:
		return qaschema.RunEventPayloadLevelError
	}
}

// onExecutorEvent forwards the sidecar's progress as run events. Only step and
// evidence milestones are forwarded; the rest stays in the local log, because
// ADR-002 makes every event a row in run_events.
func (rc *runController) onExecutorEvent(event executor.Event) {
	switch event.Event {
	case "step", "assertion":
		rc.emit(qaschema.RunEventPayloadLevelDebug, "executor_"+event.Event, event.Event, event.Data)
	case "progress":
		rc.emit(qaschema.RunEventPayloadLevelDebug, "executor_progress", "executor progress", event.Data)
	}
}

// readLedger loads the results already recorded for this run.
func readLedger(path string) (map[string]qaschema.ExecutionResult, error) {
	file, err := os.Open(path) //nolint:gosec // the path is inside the run's own workspace
	if errors.Is(err, os.ErrNotExist) {
		return map[string]qaschema.ExecutionResult{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runtime: open execution ledger: %w", err)
	}
	defer func() { _ = file.Close() }()

	out := map[string]qaschema.ExecutionResult{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var result qaschema.ExecutionResult
		if err := json.Unmarshal(line, &result); err != nil {
			// A half-written line is the tail of a crash, not a reason to
			// discard the entries before it.
			continue
		}
		out[result.TestCaseID] = result
	}
	return out, scanner.Err()
}

// appendLedger records one finished case. The write is flushed before the next
// case starts, because the crash it protects against can happen at any point.
func appendLedger(path string, result qaschema.ExecutionResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("runtime: encode ledger entry: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // inside the run workspace
	if err != nil {
		return fmt.Errorf("runtime: open execution ledger: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("runtime: append to execution ledger: %w", err)
	}
	return file.Sync()
}
