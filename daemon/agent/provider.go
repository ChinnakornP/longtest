package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/ChinnakornP/longtest/daemon/agent/prompts"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// Status is how one agent invocation ended.
//
// The vocabulary is closed on purpose. "The CLI is not installed", "the CLI
// answered with something that is not a test plan" and "the CLI never answered"
// are three different operational problems with three different fixes, and a
// UI that shows them all as "agent failed" sends the operator looking in the
// wrong place.
type Status string

// The five outcomes. Anything a provider cannot classify is StatusError.
const (
	// StatusOK means out.json was written and validated against the contract.
	StatusOK Status = "ok"
	// StatusOutputInvalid means the CLI ran and produced something, but it
	// still did not match the schema after every retry. The document is never
	// repaired by guesswork: a plan we edited is not the plan the model wrote,
	// and silently fixing one is how a broken prompt survives for months.
	StatusOutputInvalid Status = "agent_output_invalid"
	// StatusTimeout means the process was killed at the deadline.
	StatusTimeout Status = "agent_timeout"
	// StatusUnavailable means the CLI is not usable on this machine: not
	// installed, or installed and not authenticated.
	StatusUnavailable Status = "agent_unavailable"
	// StatusError is any other failure to run the CLI — it exited non-zero,
	// or the exchange could not be written to the workspace.
	StatusError Status = "agent_error"
)

// RunErrorCode maps an outcome onto the contract's error vocabulary, which is
// what the backend and the UI switch on.
func (s Status) RunErrorCode() qaschema.RunErrorCode {
	switch s {
	case StatusUnavailable:
		return qaschema.RunErrorCodeAgentNotAvailable
	case StatusOutputInvalid, StatusTimeout, StatusError:
		return qaschema.RunErrorCodeAgentOutputInvalid
	default:
		return qaschema.RunErrorCodeInternal
	}
}

// Error is the typed failure a provider or the runner returns. It carries the
// status so a caller can map it to a contract error code without matching on
// message text.
type Error struct {
	Status  Status
	Op      string
	Message string
	cause   error
}

func (e *Error) Error() string {
	msg := e.Message
	if e.Op != "" {
		msg = e.Op + ": " + msg
	}
	if e.cause != nil {
		return fmt.Sprintf("%s (%s): %v", msg, e.Status, e.cause)
	}
	return fmt.Sprintf("%s (%s)", msg, e.Status)
}

func (e *Error) Unwrap() error { return e.cause }

func errorf(status Status, cause error, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...), cause: cause}
}

// Readiness is the three-way answer Detect gives about one CLI.
//
// The middle state is the one that matters: "installed but not logged in" is
// the single most common reason a fresh runtime cannot do AI work, and an
// operator told only "claude: not available" will reinstall a CLI that was
// never missing.
type Readiness string

// The three states, from least to most usable.
const (
	ReadinessMissing         Readiness = "missing"
	ReadinessUnauthenticated Readiness = "unauthenticated"
	ReadinessReady           Readiness = "ready"
)

// Capability is what this machine can do with one CLI.
type Capability struct {
	Name      qaschema.AgentCapabilityName
	Readiness Readiness
	// Version is the CLI's own version string, first line only.
	Version string
	// Path is the resolved executable, empty when missing.
	Path string
	// Detail explains a non-ready state and names the command that fixes it.
	Detail string
}

// Usable reports whether a run may be handed to this CLI.
func (c Capability) Usable() bool { return c.Readiness == ReadinessReady }

// Schema converts the capability to the wire form carried in the hello frame.
// A CLI that is installed but unauthenticated is reported ok:false with the
// reason, never omitted: the operator picking an agent in the UI needs to see
// that it exists as an option and why this runtime cannot offer it.
func (c Capability) Schema() qaschema.AgentCapability {
	out := qaschema.AgentCapability{Name: c.Name, Ok: c.Usable()}
	if c.Version != "" {
		out.Version = ptr(c.Version)
	}
	if !c.Usable() && c.Detail != "" {
		out.Error = ptr(c.Detail)
	}
	return out
}

// Task is one file exchange with an AI CLI (ADR-003).
//
// Everything the model reads is a file in WorkspaceDir and everything it
// writes is OutputFile in the same directory. Nothing is passed as an
// argument, and stdout is a debug log rather than a channel: CLIs change their
// output format between minor versions, and a parser built on one is a
// silent breakage waiting for the next release.
type Task struct {
	// Agent names the CLI to use. Empty means the runner's default.
	Agent qaschema.AgentCapabilityName

	// Phase selects the prompt template.
	Phase prompts.Phase

	// WorkspaceDir is the run's phase directory,
	// /workspaces/{projectId}/{runId}/{phase}. It is the agent's blast
	// radius: its working directory, its $HOME, and the only path it may
	// write to.
	WorkspaceDir string

	// PromptFile is the rendered prompt's name inside WorkspaceDir. Empty
	// means prompt.md.
	PromptFile string
	// OutputFile is the document the CLI must write. Empty means out.json.
	OutputFile string

	// OutputSchema names the contract OutputFile is validated against,
	// e.g. "application-map@1".
	OutputSchema string
	// OutputAsList says OutputFile is a JSON array whose elements are each
	// OutputSchema documents, which is how the analysis phase returns
	// findings.
	OutputAsList bool

	// Inputs are files placed in WorkspaceDir before the CLI runs, keyed by
	// name. Large context travels this way, never inlined into the prompt.
	Inputs map[string][]byte

	// Untrusted are page-derived observations. They reach the model only
	// inside a security.Wrap frame; there is no path in this package that
	// puts them anywhere else.
	Untrusted []security.Block
	// Nonce is the frame id. Empty means the runner mints one.
	Nonce string

	// AllowedOrigins is the egress allowlist restated to the model, and
	// FixtureNames the logins it may reference. Neither is the enforcement
	// point — security.PlanGate is — but a model told the rule breaks it
	// less often, which saves a retry.
	AllowedOrigins []string
	FixtureNames   []string

	// Scrubber removes the run's target-app credentials from the prompt, the
	// output and the recorded logs.
	Scrubber *security.Scrubber

	// Sandbox is the base confinement for the child process. The runner
	// fills WorkspaceDir and the limits; a provider adds only what its own
	// CLI needs — its credential directory read-only, its own environment
	// variables — and never a wildcard.
	Sandbox security.Spec

	// Timeout bounds one invocation. Empty means DefaultTimeout.
	Timeout time.Duration

	// Stdout and Stderr are the attempt's debug log, wired by the runner to
	// files under the phase directory. A provider passes them straight to
	// [Launch]; it must never read them back and treat what the CLI printed
	// as the answer. The answer is OutputFile (ADR-003).
	Stdout io.Writer
	Stderr io.Writer

	// Events receives progress as the phase runs. Sends are non-blocking: a
	// slow consumer drops events rather than stalling a run.
	Events chan<- Event

	// RunID and BaseURL are context for logs and for the prompt header.
	RunID   string
	BaseURL string
}

// DefaultPromptFile and DefaultOutputFile are the names the prompt templates
// refer to. A provider that renames them has broken the templates.
const (
	DefaultPromptFile = "prompt.md"
	DefaultOutputFile = "out.json"
)

// DefaultTimeout bounds one invocation. Discovery of a large application is
// the slow case; anything past this is a hung CLI, not a thoughtful one.
const DefaultTimeout = 10 * time.Minute

// promptName and outputName apply the defaults.
func (t Task) promptName() string {
	if t.PromptFile != "" {
		return t.PromptFile
	}
	return DefaultPromptFile
}

func (t Task) outputName() string {
	if t.OutputFile != "" {
		return t.OutputFile
	}
	return DefaultOutputFile
}

// Result is how one phase came out.
type Result struct {
	Status Status
	// Attempts is how many times the CLI was invoked, including the one that
	// succeeded. It is what makes a retry loop visible in the run log rather
	// than only in the workspace.
	Attempts int
	// Output is the validated document, or nil when Status is not ok.
	Output json.RawMessage
	// Provider names the CLI that ran.
	Provider qaschema.AgentCapabilityName
	// Duration is wall time across every attempt.
	Duration time.Duration
	// Detail explains a non-ok status in one line, safe to show an operator.
	Detail string

	// ExitCode and Command describe the last invocation, for the attempt
	// record. Command is argv without the prompt: the prompt goes in on
	// stdin so it is not on the process table for every account on the
	// machine to read.
	ExitCode int
	Command  string
}

// Provider is one AI CLI's launch recipe — contract E's AgentProvider, named
// without the stutter Go would otherwise give agent.AgentProvider.
//
// A provider owns how its CLI is invoked and nothing else. It does not render
// prompts (prompts.Build does, and it is the only thing that may), it does not
// validate output against a contract, and it does not decide whether to retry:
// those are identical for every CLI and live in Runner. What a provider
// implements is the part that genuinely differs — the flags, the credential
// location, and how the CLI is told to write a file.
type Provider interface {
	// Name is the CLI this provider drives.
	Name() qaschema.AgentCapabilityName

	// Detect answers whether this machine can use it: not installed,
	// installed but not authenticated, or ready. A detection that cannot be
	// completed returns an error; a CLI that is simply absent does not.
	Detect(ctx context.Context) (Capability, error)

	// Run performs exactly one invocation: it launches the CLI against the
	// prompt already written in t.WorkspaceDir and returns the bytes of
	// t.OutputFile. The returned Result has Attempts 1; the retry loop is
	// Runner's.
	//
	// It blocks until the CLI exits, the timeout fires or ctx is cancelled,
	// and it leaves no process behind in any of those cases.
	Run(ctx context.Context, t Task) (Result, error)
}

// EventKind is what happened.
type EventKind string

// The progress an operator watching a live run sees.
const (
	EventAttemptStarted  EventKind = "agent_attempt_started"
	EventAttemptFinished EventKind = "agent_attempt_finished"
	EventOutputInvalid   EventKind = "agent_output_invalid"
	EventFinished        EventKind = "agent_finished"
)

// Event is one line of progress from a phase — contract E's AgentEvent.
//
// It carries no model output and no page content: everything here is either
// generated by this package or a schema path, so it can be forwarded to the
// backend without passing through the untrusted-content boundary again.
type Event struct {
	Kind     EventKind                    `json:"kind"`
	Phase    prompts.Phase                `json:"phase"`
	Provider qaschema.AgentCapabilityName `json:"provider"`
	Attempt  int                          `json:"attempt"`
	Status   Status                       `json:"status,omitempty"`
	Message  string                       `json:"message,omitempty"`
	At       time.Time                    `json:"at"`
}

// emit sends without blocking. A dropped progress line is better than a run
// that stalls because nobody is draining the channel.
func emit(ch chan<- Event, ev Event) {
	if ch == nil {
		return
	}
	select {
	case ch <- ev:
	default:
	}
}
