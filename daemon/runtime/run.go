package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ChinnakornP/longtest/daemon/artifacts"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/workspace"
)

// resultPayload is what the daemon sends as run.result.
//
// It mirrors qaschema.RunResultPayload except that findings are carried as the
// exact bytes the analyst produced and the daemon validated. Two reasons, and
// the second is the one that bites: a document the daemon only forwards should
// not be re-encoded at all, and the generated Finding type tags the required
// `stepIndex` as omitempty — so a finding that legitimately blames no single
// step comes back out of Go without the property and no longer matches
// finding@1. Round-tripping a validated document through a lossy struct is how
// a valid document becomes invalid in transit.
type resultPayload struct {
	Status     qaschema.RunResultPayloadStatus `json:"status"`
	Error      *qaschema.RunError              `json:"error,omitempty"`
	AppMap     *qaschema.ApplicationMap        `json:"appMap,omitempty"`
	TestPlan   *qaschema.TestPlan              `json:"testPlan,omitempty"`
	Executions []qaschema.ExecutionResult      `json:"executions,omitempty"`
	Findings   []json.RawMessage               `json:"findings,omitempty"`
	Artifacts  []qaschema.Artifact             `json:"artifacts,omitempty"`
}

// runFailure is a failure with a contract error code attached, so the reason a
// run failed survives all the way to the UI instead of becoming free text.
type runFailure struct {
	Code    qaschema.RunErrorCode
	Message string
	cause   error
}

func (e *runFailure) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *runFailure) Unwrap() error { return e.cause }

func failure(code qaschema.RunErrorCode, cause error, format string, args ...any) *runFailure {
	return &runFailure{Code: code, Message: fmt.Sprintf(format, args...), cause: cause}
}

// cancellation is the context cause set when a run is cancelled, so the run
// can report the reason the operator gave rather than "context canceled".
type cancellation struct {
	Reason  qaschema.RunCancelPayloadReason
	Message string
}

func (c *cancellation) Error() string {
	if c.Message != "" {
		return fmt.Sprintf("run cancelled (%s): %s", c.Reason, c.Message)
	}
	return fmt.Sprintf("run cancelled (%s)", c.Reason)
}

// runController drives one run from assignment to result.
type runController struct {
	daemon  *Daemon
	payload qaschema.RunAssignPayload
	logger  *slog.Logger

	cancel context.CancelCauseFunc

	mu    sync.Mutex
	phase qaschema.RunEventPayloadPhase
	ws    *workspace.Workspace
}

// handleAssign accepts a run, or explains why it will not run it twice.
func (d *Daemon) handleAssign(runCtx context.Context, env qaschema.Envelope) error {
	var payload qaschema.RunAssignPayload
	if err := decodePayload(env.Payload, &payload); err != nil {
		return err
	}
	if env.RunID == nil || *env.RunID != payload.RunID {
		return fmt.Errorf("runtime: run.assign envelope runId does not match its payload")
	}
	if !payload.Mode.IsValid() {
		return fmt.Errorf("runtime: run.assign has unknown mode %q", payload.Mode)
	}

	status, previous := d.runs.Claim(payload.RunID)
	switch status {
	case claimActive:
		// The backend re-sent an assignment we are already working on, which
		// is what at-least-once delivery looks like from here.
		d.logger.Info("ignoring duplicate run.assign", "runId", payload.RunID)
		return nil
	case claimFinished:
		d.logger.Info("re-reporting a finished run", "runId", payload.RunID, "status", previous.Status)
		d.sendResult(payload.RunID, previous)
		return nil
	}

	rc := &runController{
		daemon:  d,
		payload: payload,
		logger:  d.logger.With("runId", payload.RunID, "mode", string(payload.Mode)),
		phase:   qaschema.RunEventPayloadPhaseDiscover,
	}
	ctx, cancel := context.WithCancelCause(runCtx)
	rc.cancel = cancel
	d.runs.Attach(payload.RunID, rc)

	d.publish(func(s *State) {
		s.ActiveRuns = append(s.ActiveRuns, RunState{
			RunID:     payload.RunID,
			ProjectID: payload.ProjectID,
			Mode:      string(payload.Mode),
			StartedAt: d.now().UTC(),
		})
	})

	go rc.run(ctx)
	return nil
}

// handleCancel stops a run and acknowledges what it found.
func (d *Daemon) handleCancel(env qaschema.Envelope) error {
	var payload qaschema.RunCancelPayload
	if err := decodePayload(env.Payload, &payload); err != nil {
		return err
	}
	if env.RunID == nil {
		return errors.New("runtime: run.cancel has no runId")
	}
	message := ""
	if payload.Message != nil {
		message = *payload.Message
	}

	if d.runs.Cancel(*env.RunID, payload.Reason, message) {
		d.logger.Info("cancelling run", "runId", *env.RunID, "reason", string(payload.Reason))
		return nil
	}

	// Cancelling something already finished is normal (the user clicked stop
	// as the run ended). Report it rather than staying silent, so the backend
	// is not left waiting for a result that will never come.
	d.logger.Info("run.cancel for a run this daemon is not running", "runId", *env.RunID)
	d.emitEvent(*env.RunID, qaschema.RunEventPayload{
		Phase:   qaschema.RunEventPayloadPhaseReport,
		Level:   qaschema.RunEventPayloadLevelInfo,
		Code:    "cancel_ignored",
		Message: "this runtime is not executing that run",
	})
	return nil
}

// Cancel asks the run to stop.
func (rc *runController) Cancel(reason qaschema.RunCancelPayloadReason, message string) {
	rc.cancel(&cancellation{Reason: reason, Message: message})
}

// run executes the run and reports exactly one result.
func (rc *runController) run(ctx context.Context) {
	defer rc.cancel(nil)

	d := rc.daemon
	started := d.now()
	rc.emit(qaschema.RunEventPayloadLevelInfo, "run_started", fmt.Sprintf("run started in %s mode", rc.payload.Mode), nil)

	result := rc.execute(ctx)

	outcome := workspace.OutcomeFailed
	switch result.Status {
	case qaschema.RunResultPayloadStatusCompleted:
		outcome = workspace.OutcomeCompleted
	case qaschema.RunResultPayloadStatusCancelled:
		outcome = workspace.OutcomeCancelled
	}
	rc.finishWorkspace(outcome)

	rc.logger.Info("run finished",
		"status", string(result.Status),
		"executions", len(result.Executions),
		"artifacts", len(result.Artifacts),
		"took", d.now().Sub(started).Round(time.Millisecond))

	d.sendResult(rc.payload.RunID, result)

	for _, evicted := range d.runs.Release(rc.payload.RunID, result) {
		d.seq.Forget(evicted)
	}
	d.mu.Lock()
	d.completed++
	completed := d.completed
	d.mu.Unlock()
	d.publish(func(s *State) {
		s.CompletedRun = completed
		s.ActiveRuns = withoutRun(s.ActiveRuns, rc.payload.RunID)
	})

	// The sweep runs after a run rather than on a timer: it is the moment the
	// set of finished workspaces changed, and a daemon that never runs
	// anything has nothing to sweep.
	if swept, err := d.deps.Workspaces.Sweep(); err != nil {
		rc.logger.Warn("workspace retention sweep had errors", "error", err)
	} else if len(swept) > 0 {
		rc.logger.Info("swept workspaces", "count", len(swept), "first", swept[0].Reason)
	}
}

// execute walks the phases this run's mode calls for.
func (rc *runController) execute(ctx context.Context) resultPayload {
	d := rc.daemon

	ws, err := d.deps.Workspaces.Create(rc.payload.ProjectID, rc.payload.RunID)
	if err != nil {
		return rc.failed(ctx, failure(qaschema.RunErrorCodeInternal, err, "could not create the run workspace"))
	}
	rc.setWorkspace(ws)

	uploader, err := artifacts.NewUploader(rc.payload.ArtifactUpload,
		artifacts.WithHTTPClient(d.deps.HTTPClient),
		artifacts.WithLogger(rc.logger))
	if err != nil {
		return rc.failed(ctx, failure(qaschema.RunErrorCodeArtifactUploadFailed, err, "the presigned upload credentials are unusable"))
	}

	result := resultPayload{Status: qaschema.RunResultPayloadStatusCompleted}
	appMap := rc.payload.AppMap
	testCases := rc.payload.TestCases

	for _, phase := range phasesFor(rc.payload.Mode) {
		if err := ctx.Err(); err != nil {
			return rc.cancelled(ctx, result)
		}
		rc.setPhase(phase.event)
		rc.emit(qaschema.RunEventPayloadLevelInfo, "phase_started", fmt.Sprintf("%s phase started", phase.event), nil)

		var phaseErr error
		switch phase.workspace {
		case workspace.PhaseDiscovery:
			var discovered qaschema.ApplicationMap
			phaseErr = rc.agentPhase(ctx, ws, phase, "application-map@1", nil, nil, &discovered)
			if phaseErr == nil {
				appMap = &discovered
				result.AppMap = &discovered
			}
		case workspace.PhasePlanning:
			if appMap == nil {
				phaseErr = failure(qaschema.RunErrorCodeAgentOutputInvalid, nil,
					"planning needs an application map, and neither the assignment nor discovery produced one")
				break
			}
			var plan qaschema.TestPlan
			inputs, encodeErr := jsonInputs(map[string]any{"application-map.json": appMap})
			if encodeErr != nil {
				phaseErr = failure(qaschema.RunErrorCodeInternal, encodeErr, "could not write the planning inputs")
				break
			}
			// The gate travels with the task rather than running after it, so
			// a rejection becomes the next attempt's feedback instead of the
			// run's cause of death. See daemon/runtime/plan.go.
			review := planReview(appMap, rc.payload.BaseURL, rc.knownFixtures(), nil)
			phaseErr = rc.agentPhase(ctx, ws, phase, "test-plan@1", inputs, review, &plan)
			if phaseErr == nil {
				result.TestPlan = &plan
				testCases = plan.TestCases
				rc.narratePlan(&plan)
			}
		case workspace.PhaseExecution:
			if appMap == nil {
				phaseErr = failure(qaschema.RunErrorCodeAgentOutputInvalid, nil,
					"execution needs an application map, and none was assigned or discovered")
				break
			}
			if len(testCases) == 0 {
				phaseErr = failure(qaschema.RunErrorCodeAgentOutputInvalid, nil,
					"execution needs test cases, and none were assigned or planned")
				break
			}
			var executions []qaschema.ExecutionResult
			var uploaded []qaschema.Artifact
			executions, uploaded, phaseErr = rc.runTestCases(ctx, ws, *appMap, testCases, uploader)
			result.Executions = append(result.Executions, executions...)
			result.Artifacts = append(result.Artifacts, uploaded...)
		case workspace.PhaseAnalysis:
			// Everything this phase does is in analyze.go: collect the
			// evidence, decide what a rule can decide, ask the model about the
			// rest behind a gate, and guarantee that every failed execution
			// ends up with a finding.
			var findings []json.RawMessage
			var evidence []qaschema.Artifact
			findings, evidence, phaseErr = rc.analyse(ctx, ws, phase, result.Executions, testCases, appMap, uploader)
			result.Findings = append(result.Findings, findings...)
			result.Artifacts = append(result.Artifacts, evidence...)
		}

		if phaseErr != nil {
			if ctx.Err() != nil {
				return rc.cancelled(ctx, result)
			}
			rc.emit(qaschema.RunEventPayloadLevelError, "phase_failed", fmt.Sprintf("%s phase failed", phase.event),
				map[string]any{"error": phaseErr.Error()})
			failed := rc.failed(ctx, phaseErr)
			failed.Executions = result.Executions
			failed.Artifacts = result.Artifacts
			failed.AppMap = result.AppMap
			failed.TestPlan = result.TestPlan
			failed.Findings = result.Findings
			return failed
		}
		rc.emit(qaschema.RunEventPayloadLevelInfo, "phase_finished", fmt.Sprintf("%s phase finished", phase.event), nil)
	}

	if ctx.Err() != nil {
		return rc.cancelled(ctx, result)
	}
	return result
}

// phase pairs a workspace directory with the phase name the event contract
// uses. They differ on purpose: the workspace is named after the artefacts it
// holds, the event after the verb the UI shows.
type phase struct {
	workspace workspace.Phase
	event     qaschema.RunEventPayloadPhase
}

var (
	discoverPhase = phase{workspace.PhaseDiscovery, qaschema.RunEventPayloadPhaseDiscover}
	planPhase     = phase{workspace.PhasePlanning, qaschema.RunEventPayloadPhasePlan}
	executePhase  = phase{workspace.PhaseExecution, qaschema.RunEventPayloadPhaseExecute}
	analyzePhase  = phase{workspace.PhaseAnalysis, qaschema.RunEventPayloadPhaseAnalyze}
)

// phasesFor is the whole mode contract in one place.
func phasesFor(mode qaschema.RunAssignPayloadMode) []phase {
	switch mode {
	case qaschema.RunAssignPayloadModeDiscover:
		return []phase{discoverPhase}
	case qaschema.RunAssignPayloadModePlan:
		return []phase{planPhase}
	case qaschema.RunAssignPayloadModeExecute:
		return []phase{executePhase}
	case qaschema.RunAssignPayloadModeFull:
		return []phase{discoverPhase, planPhase, executePhase, analyzePhase}
	default:
		return nil
	}
}

// agentPhase runs one AI CLI phase whose out.json is a single document.
func (rc *runController) agentPhase(ctx context.Context, ws *workspace.Workspace, ph phase, schemaID string, inputs map[string][]byte, review func([]byte) []string, out any) error {
	raw, err := rc.runAgent(ctx, ws, ph, schemaID, inputs, review)
	if err != nil {
		return err
	}
	if err := rc.validateAgainst(schemaID, raw, ph); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return failure(qaschema.RunErrorCodeAgentOutputInvalid, err, "could not decode the %s output", ph.event)
	}
	return nil
}

// validateAgainst is the last gate before a model-authored document is
// forwarded to the backend as fact. The provider validates too (ADR-003);
// this is the check that does not depend on which provider ran.
func (rc *runController) validateAgainst(schemaID string, raw []byte, ph phase) error {
	result, err := qaschema.ValidateJSON(schemaID, raw)
	if err != nil {
		return failure(qaschema.RunErrorCodeInternal, err, "could not validate the %s output", ph.event)
	}
	if !result.Valid {
		return failure(qaschema.RunErrorCodeAgentOutputInvalid, nil,
			"the %s agent produced output that does not match %s: %s", ph.event, schemaID, result.Errors[0].String())
	}
	return nil
}

// runAgent performs the file exchange itself.
func (rc *runController) runAgent(ctx context.Context, ws *workspace.Workspace, ph phase, schemaID string, inputs map[string][]byte, review func([]byte) []string) ([]byte, error) {
	runner := rc.daemon.deps.Agent
	if runner == nil {
		return nil, failure(qaschema.RunErrorCodeAgentNotAvailable, nil,
			"this runtime has no AI CLI provider configured, so the %s phase cannot run", ph.event)
	}

	dir, err := ws.PhaseDir(ph.workspace)
	if err != nil {
		return nil, failure(qaschema.RunErrorCodeInternal, err, "could not resolve the %s workspace", ph.workspace)
	}

	task := AgentTask{
		Phase:        ph.workspace,
		Dir:          dir,
		SchemaID:     schemaID,
		Inputs:       inputs,
		RunID:        rc.payload.RunID,
		BaseURL:      rc.payload.BaseURL,
		FixtureNames: rc.fixtureNames(),
		Review:       review,
	}
	if rc.payload.Agent != nil {
		task.Agent = qaschema.AgentCapabilityName(*rc.payload.Agent)
	}

	raw, err := runner.Run(ctx, task)
	if err != nil {
		var typed *runFailure
		if errors.As(err, &typed) {
			return nil, typed
		}
		return nil, failure(qaschema.RunErrorCodeAgentOutputInvalid, err, "the %s agent failed", ph.event)
	}
	return raw, nil
}

func (rc *runController) failed(ctx context.Context, err error) resultPayload {
	if ctx.Err() != nil {
		return rc.cancelled(ctx, resultPayload{})
	}

	var typed *runFailure
	if !errors.As(err, &typed) {
		typed = failure(qaschema.RunErrorCodeInternal, err, "the run failed")
	}
	rc.logger.Error("run failed", "code", string(typed.Code), "error", err)
	return resultPayload{
		Status: qaschema.RunResultPayloadStatusFailed,
		Error: &qaschema.RunError{
			Code: typed.Code,
			// The cause is logged locally and summarised here: a driver or
			// sidecar message can carry a path or a URL from this network.
			Message: typed.Message,
		},
	}
}

func (rc *runController) cancelled(ctx context.Context, partial resultPayload) resultPayload {
	reason := qaschema.RunCancelPayloadReasonUserRequested
	message := "the run was cancelled"
	var cause *cancellation
	if errors.As(context.Cause(ctx), &cause) {
		reason = cause.Reason
		if cause.Message != "" {
			message = cause.Message
		}
	}
	rc.emit(qaschema.RunEventPayloadLevelWarn, "run_cancelled", message, map[string]any{"reason": string(reason)})

	partial.Status = qaschema.RunResultPayloadStatusCancelled
	partial.Error = nil
	return partial
}

func jsonInputs(files map[string]any) (map[string][]byte, error) {
	out := make(map[string][]byte, len(files))
	for name, value := range files {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("runtime: encode %s: %w", name, err)
		}
		out[name] = data
	}
	return out, nil
}

// setWorkspace / finishWorkspace keep the workspace handle off the hot path
// while still letting run() mark the outcome when it is known.
func (rc *runController) setWorkspace(ws *workspace.Workspace) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.ws = ws
}

func (rc *runController) finishWorkspace(outcome workspace.Outcome) {
	rc.mu.Lock()
	ws := rc.ws
	rc.mu.Unlock()
	if ws == nil {
		return
	}
	if err := rc.daemon.deps.Workspaces.Finish(ws, outcome); err != nil {
		rc.logger.Warn("could not record the workspace outcome", "error", err)
	}
}

func (rc *runController) setPhase(p qaschema.RunEventPayloadPhase) {
	rc.mu.Lock()
	rc.phase = p
	rc.mu.Unlock()
	rc.daemon.publish(func(s *State) {
		for i := range s.ActiveRuns {
			if s.ActiveRuns[i].RunID == rc.payload.RunID {
				s.ActiveRuns[i].Phase = string(p)
			}
		}
	})
}

func (rc *runController) currentPhase() qaschema.RunEventPayloadPhase {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.phase
}

// emit queues a run event. Events are the run's narration, not its log: the
// daemon keeps full logs locally and sends only what the UI switches on.
func (rc *runController) emit(level qaschema.RunEventPayloadLevel, code, message string, data map[string]any) {
	rc.daemon.emitEvent(rc.payload.RunID, qaschema.RunEventPayload{
		Phase:   rc.currentPhase(),
		Level:   level,
		Code:    code,
		Message: message,
		Data:    data,
	})
}

func (rc *runController) emitForCase(level qaschema.RunEventPayloadLevel, code, message, testCaseID string, data map[string]any) {
	rc.daemon.emitEvent(rc.payload.RunID, qaschema.RunEventPayload{
		Phase:      rc.currentPhase(),
		Level:      level,
		Code:       code,
		Message:    message,
		TestCaseID: &testCaseID,
		Data:       data,
	})
}

func (d *Daemon) emitEvent(runID string, payload qaschema.RunEventPayload) {
	env, err := newEnvelope(qaschema.EnvelopeTypeRunEvent, &runID, d.seq.NextRun(runID), payload, d.now())
	if err != nil {
		d.logger.Error("could not build a run event", "runId", runID, "error", err)
		return
	}
	d.outbox.Push(env)
}

func (d *Daemon) sendResult(runID string, result resultPayload) {
	env, err := newEnvelope(qaschema.EnvelopeTypeRunResult, &runID, d.seq.NextRun(runID), result, d.now())
	if err != nil {
		d.logger.Error("could not build the run result", "runId", runID, "error", err)
		return
	}
	d.outbox.Push(env)
}

func withoutRun(runs []RunState, runID string) []RunState {
	out := runs[:0]
	for _, run := range runs {
		if run.RunID != runID {
			out = append(out, run)
		}
	}
	return out
}
