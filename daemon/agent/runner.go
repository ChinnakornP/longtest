package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ChinnakornP/longtest/daemon/agent/prompts"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// DefaultMaxAttempts is one try plus two retries.
//
// Two is where the evidence stops arguing for more: a model that has been told
// twice which fields are wrong and still writes the wrong shape is not going to
// find it on the third correction, and each attempt costs a full context of
// tokens. Past this the honest answer is agent_output_invalid with three
// prompts and three answers in the workspace for a human to read.
const DefaultMaxAttempts = 3

// RunnerOptions configure the phase loop.
type RunnerOptions struct {
	// Registry holds the providers. Required.
	Registry *Registry
	// Default is the CLI used when a run names none. Empty means the first
	// usable provider in registration order.
	Default qaschema.AgentCapabilityName
	// MaxAttempts is the total number of invocations per phase, including the
	// first. Zero means DefaultMaxAttempts; one disables retries.
	MaxAttempts int
	// Timeout bounds one invocation when a task does not set its own.
	Timeout time.Duration
	// Sandbox is the confinement every provider starts from. The runner sets
	// WorkspaceDir per task; everything else — the limits, the network policy,
	// whether an unsandboxed host is tolerated — comes from here.
	Sandbox security.Spec

	// Secrets are the platform's own credentials — the runtime pairing token,
	// the artifact-store key — registered with every task's scrubber.
	//
	// A run's fixture passwords are scrubbed because the run owns them and
	// hands them to a browser. These are the opposite case: the run never
	// touches them, so the only way one reaches a prompt is a page that echoes
	// it back, which means it has already leaked somewhere upstream. Removing
	// it here does not fix that leak; it stops the leak being copied into a
	// third party's context window on top.
	Secrets []string
	Logger  *slog.Logger
	Now     func() time.Time
}

// Runner turns a phase into a validated document.
//
// It owns everything that is the same for every CLI: rendering the prompt
// through [prompts.Build], placing the input files, invoking the provider,
// validating the answer against its contract, retrying with the validator's
// report attached, and leaving all of it in the workspace. A provider that
// wanted to do any of this itself would be reimplementing the part that must
// not vary between vendors.
type Runner struct {
	registry    *Registry
	preferred   qaschema.AgentCapabilityName
	maxAttempts int
	timeout     time.Duration
	sandbox     security.Spec
	secrets     []string
	logger      *slog.Logger
	now         func() time.Time
}

// NewRunner validates the options and builds the loop.
func NewRunner(opts RunnerOptions) (*Runner, error) {
	if opts.Registry == nil {
		return nil, errors.New("agent: a runner needs a registry")
	}
	if len(opts.Registry.Names()) == 0 {
		return nil, errors.New("agent: a runner needs at least one provider")
	}
	r := &Runner{
		registry:    opts.Registry,
		preferred:   opts.Default,
		maxAttempts: opts.MaxAttempts,
		timeout:     opts.Timeout,
		sandbox:     opts.Sandbox,
		secrets:     opts.Secrets,
		logger:      opts.Logger,
		now:         opts.Now,
	}
	if r.maxAttempts <= 0 {
		r.maxAttempts = DefaultMaxAttempts
	}
	if r.logger == nil {
		r.logger = slog.New(slog.DiscardHandler)
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r, nil
}

// Detect reports what this machine can do, for the hello frame.
func (r *Runner) Detect(ctx context.Context) []Capability { return r.registry.Detect(ctx) }

// Run executes one phase and returns the validated document.
//
// A non-nil error always accompanies a non-ok Result and is always an [*Error],
// so a caller can map the outcome onto a contract error code without matching
// on message text.
func (r *Runner) Run(ctx context.Context, t Task) (Result, error) {
	started := r.now()

	if t.WorkspaceDir == "" {
		return Result{Status: StatusError}, errorf(StatusError, nil, "the task names no workspace directory")
	}
	if !qaschema.IsSchemaID(t.OutputSchema) {
		return Result{Status: StatusError}, errorf(StatusError, nil,
			"%q is not a contract in this build; known: %s", t.OutputSchema, strings.Join(qaschema.SchemaIDs, ", "))
	}

	provider, capability, err := r.registry.Select(ctx, t.Agent, r.preferred)
	if err != nil {
		return Result{Status: StatusUnavailable, Detail: err.Error()}, err
	}
	name := capability.Name

	ws, err := security.OpenWorkspace(t.WorkspaceDir)
	if err != nil {
		return Result{Status: StatusError, Provider: name}, errorf(StatusError, err, "the %s workspace is not usable", t.Phase)
	}
	defer func() { _ = ws.Close() }()

	rec := recorder{ws: ws, scrubber: t.Scrubber}

	for fileName, data := range t.Inputs {
		if err := ws.WriteFile(fileName, data); err != nil {
			return Result{Status: StatusError, Provider: name}, errorf(StatusError, err, "could not place %s in the workspace", fileName)
		}
	}

	contract, err := writeContracts(ws, t.OutputSchema)
	if err != nil {
		return Result{Status: StatusError, Provider: name}, errorf(StatusError, err, "could not place the %s contract", t.OutputSchema)
	}

	nonce := t.Nonce
	if nonce == "" {
		nonce = security.NewNonce()
	}

	// The platform's own credentials are registered before a single byte is
	// rendered. A scrubber that learns a secret after the first prompt is not
	// a control.
	if len(r.secrets) > 0 {
		if t.Scrubber == nil {
			t.Scrubber = security.NewScrubber()
		}
		if err := t.Scrubber.AddAll(r.secrets...); err != nil {
			return Result{Status: StatusError, Provider: name}, errorf(StatusError, err, "could not register the platform secrets")
		}
	}

	// feedback is the previous attempt's validator report, handed back as an
	// untrusted block rather than as instructions. On the first attempt there
	// is none.
	var feedback []string
	result := Result{Provider: name, Status: StatusOutputInvalid}

	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		result.Attempts = attempt

		prompt, err := r.buildPrompt(t, nonce, contract, attempt, feedback)
		if err != nil {
			result.Status, result.Detail = StatusError, err.Error()
			return result, errorf(StatusError, err, "could not render the %s prompt", t.Phase)
		}
		if err := ws.WriteFile(t.promptName(), []byte(prompt)); err != nil {
			result.Status, result.Detail = StatusError, err.Error()
			return result, errorf(StatusError, err, "could not write %s", t.promptName())
		}
		// A leftover answer from the previous attempt would otherwise be read
		// back as this attempt's, turning a CLI that crashed on startup into a
		// phase that "succeeded" with stale content.
		if err := ws.WriteFile(t.outputName(), nil); err != nil {
			result.Status, result.Detail = StatusError, err.Error()
			return result, errorf(StatusError, err, "could not clear %s", t.outputName())
		}

		emit(t.Events, Event{
			Kind: EventAttemptStarted, Phase: t.Phase, Provider: name,
			Attempt: attempt, At: r.now(),
		})

		dir, stdout, stderr, err := rec.open(attempt)
		if err != nil {
			result.Status, result.Detail = StatusError, err.Error()
			return result, errorf(StatusError, err, "could not record attempt %d", attempt)
		}

		attemptTask := t
		attemptTask.Nonce = nonce
		attemptTask.Stdout, attemptTask.Stderr = stdout, stderr
		attemptTask.Sandbox = r.specFor(t)
		attemptTask.Timeout = r.timeoutFor(t)

		attemptStarted := r.now()
		invocation, runErr := provider.Run(ctx, attemptTask)
		_ = stdout.Close()
		_ = stderr.Close()

		record := attemptRecord{
			Attempt: attempt, Provider: name, Phase: string(t.Phase), RunID: t.RunID,
			Schema: t.OutputSchema, Status: invocation.Status,
			ExitCode: invocation.ExitCode, TimedOut: invocation.Status == StatusTimeout,
			StartedAt: attemptStarted.UTC(), Duration: r.now().Sub(attemptStarted).String(),
			Command: invocation.Command, PromptBytes: len(prompt),
			OutputBytes: len(invocation.Output), Detail: invocation.Detail,
		}

		output := invocation.Output
		if t.Scrubber != nil && len(output) > 0 {
			scrubbed, err := t.Scrubber.JSON(output)
			if err != nil {
				// Not valid JSON, so it is about to be rejected anyway; scrub
				// it as text so the recorded copy is still safe to keep.
				scrubbed = t.Scrubber.Bytes(output)
			}
			if !bytes.Equal(scrubbed, output) {
				// The model copied a credential into its answer, which is
				// what a page that asked it to looks like from here. The file
				// on disk is rewritten as well as the copy in memory: the
				// workspace outlives the run by a week, and the attempt
				// record is meant to be safe to attach to a bug report.
				if err := ws.WriteFile(t.outputName(), scrubbed); err != nil {
					result.Status, result.Detail = StatusError, err.Error()
					return result, errorf(StatusError, err, "could not redact %s", t.outputName())
				}
			}
			output = scrubbed
		}

		// A provider failure that is not about the answer's shape is terminal:
		// a CLI that is not installed will not be installed by trying again,
		// and a timeout retried is the same wall-clock spent twice.
		if invocation.Status != StatusOK && invocation.Status != StatusOutputInvalid {
			_ = rec.copyInto(dir, t.promptName(), []byte(prompt), t.outputName(), output)
			_ = rec.meta(dir, record)
			result.Status, result.Detail = invocation.Status, detailOf(invocation, runErr)
			result.Duration = r.now().Sub(started)
			emit(t.Events, Event{
				Kind: EventAttemptFinished, Phase: t.Phase, Provider: name, Attempt: attempt,
				Status: invocation.Status, Message: result.Detail, At: r.now(),
			})
			return result, r.asError(invocation.Status, runErr, result.Detail)
		}

		problems := r.validate(t, output)
		record.ValidationErrors = problems
		if len(problems) == 0 && invocation.Status == StatusOK {
			record.Status = StatusOK
			_ = rec.copyInto(dir, t.promptName(), []byte(prompt), t.outputName(), output)
			if err := rec.meta(dir, record); err != nil {
				r.logger.Warn("could not write the agent attempt record", "error", err, "phase", t.Phase)
			}
			result.Status, result.Output, result.Detail = StatusOK, output, ""
			result.Duration = r.now().Sub(started)
			emit(t.Events, Event{
				Kind: EventAttemptFinished, Phase: t.Phase, Provider: name,
				Attempt: attempt, Status: StatusOK, At: r.now(),
			})
			emit(t.Events, Event{
				Kind: EventFinished, Phase: t.Phase, Provider: name,
				Attempt: attempt, Status: StatusOK, At: r.now(),
			})
			return result, nil
		}

		if len(problems) == 0 {
			// The provider said the answer was unusable without saying which
			// field; keep its own reason rather than inventing one.
			problems = []string{invocation.Detail}
		}
		record.Status = StatusOutputInvalid
		_ = rec.copyInto(dir, t.promptName(), []byte(prompt), t.outputName(), output)
		if err := rec.meta(dir, record); err != nil {
			r.logger.Warn("could not write the agent attempt record", "error", err, "phase", t.Phase)
		}

		feedback = problems
		result.Status = StatusOutputInvalid
		result.Detail = fmt.Sprintf("%s does not match %s: %s", t.outputName(), t.OutputSchema, problems[0])

		r.logger.Info("agent output rejected",
			"phase", t.Phase, "provider", name, "attempt", attempt,
			"schema", t.OutputSchema, "problems", len(problems))
		emit(t.Events, Event{
			Kind: EventOutputInvalid, Phase: t.Phase, Provider: name,
			Attempt: attempt, Status: StatusOutputInvalid, Message: problems[0], At: r.now(),
		})
	}

	result.Duration = r.now().Sub(started)
	detail := fmt.Sprintf("%s did not produce a valid %s in %d attempts: %s",
		name, t.OutputSchema, result.Attempts, result.Detail)
	emit(t.Events, Event{
		Kind: EventFinished, Phase: t.Phase, Provider: name,
		Attempt: result.Attempts, Status: StatusOutputInvalid, Message: detail, At: r.now(),
	})
	return result, errorf(StatusOutputInvalid, nil, "%s", detail)
}

// buildPrompt renders the phase prompt. On a retry the validator's report goes
// in as an untrusted block: it quotes the document the model wrote, and a model
// hijacked on its first attempt would otherwise get its own injected text
// handed back to it as system feedback.
func (r *Runner) buildPrompt(t Task, nonce, contract string, attempt int, feedback []string) (string, error) {
	in := prompts.Input{
		Phase:            t.Phase,
		Nonce:            nonce,
		OutputSchema:     t.OutputSchema,
		OutputSchemaFile: contract,
		AllowedOrigins:   t.AllowedOrigins,
		FixtureNames:     t.FixtureNames,
		Untrusted:        t.Untrusted,
		Scrubber:         t.Scrubber,
	}
	if attempt > 1 && len(feedback) > 0 {
		in.Retry = &prompts.Retry{Attempt: attempt, OutputFile: t.outputName()}
		in.Untrusted = append(append([]security.Block(nil), in.Untrusted...), security.Block{
			Kind:    security.KindAgentOutput,
			Source:  fmt.Sprintf("validator report on attempt %d", attempt-1),
			Content: strings.Join(feedback, "\n"),
		})
	}
	return prompts.Build(in)
}

// validate checks the answer against its contract and returns one line per
// failing field. A document that is an array of contract documents — the
// analysis phase's findings — is checked element by element, because the
// contract describes one finding and an array of them is not one.
//
// [Task.Review] runs after, and only after, the schema passes. Running it on a
// malformed document would produce a page of confusing complaints about fields
// that are not there, on top of the one true complaint that the document is not
// a contract document at all.
func (r *Runner) validate(t Task, output []byte) []string {
	if len(output) == 0 {
		return []string{fmt.Sprintf("%s is empty: the CLI wrote no answer", t.outputName())}
	}

	problems := r.schemaProblems(t, output)
	if len(problems) > 0 || t.Review == nil {
		return problems
	}
	return t.Review(output)
}

func (r *Runner) schemaProblems(t Task, output []byte) []string {
	if !t.OutputAsList {
		return validationProblems(t.OutputSchema, output)
	}

	var elements []json.RawMessage
	if err := json.Unmarshal(output, &elements); err != nil {
		return []string{fmt.Sprintf("/: expected a JSON array of %s documents: %v", t.OutputSchema, err)}
	}
	var problems []string
	for i, element := range elements {
		for _, problem := range validationProblems(t.OutputSchema, element) {
			problems = append(problems, fmt.Sprintf("/%d%s", i, problem))
		}
	}
	return problems
}

func validationProblems(schemaID string, data []byte) []string {
	result, err := qaschema.ValidateJSON(schemaID, data)
	if err != nil {
		return []string{err.Error()}
	}
	if result.Valid {
		return nil
	}
	problems := make([]string, len(result.Errors))
	for i, e := range result.Errors {
		problems[i] = e.String()
	}
	return problems
}

// specFor is the sandbox one task's child gets: the runner's policy, pinned to
// this run's directory. A provider may add its own credential path and
// environment on top; it cannot widen what is set here.
func (r *Runner) specFor(t Task) security.Spec {
	spec := r.sandbox
	spec.WorkspaceDir = t.WorkspaceDir
	if len(spec.EnvAllow) == 0 {
		spec.EnvAllow = security.BaseEnvAllow()
	}
	if spec.Limits == (security.Limits{}) {
		spec.Limits = security.DefaultAgentLimits()
	}
	if spec.Network == "" {
		spec.Network = security.NetworkHost
	}
	return spec
}

func (r *Runner) timeoutFor(t Task) time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	if r.timeout > 0 {
		return r.timeout
	}
	return DefaultTimeout
}

func (r *Runner) asError(status Status, cause error, detail string) error {
	var typed *Error
	if errors.As(cause, &typed) {
		return typed
	}
	return errorf(status, cause, "%s", detail)
}

func detailOf(invocation Result, err error) string {
	if invocation.Detail != "" {
		return invocation.Detail
	}
	if err != nil {
		return err.Error()
	}
	return string(invocation.Status)
}
