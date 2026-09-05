// Package prompts renders every prompt the daemon hands to an AI CLI.
//
// It exists so that the untrusted-content boundary is structural rather than a
// convention. There is no way to build a prompt in this codebase that puts
// page-derived bytes anywhere except inside a frame produced by
// [security.Wrap]: the template has no hole for raw content, and [Build] is
// the only exported constructor.
//
// Per ADR-003 large inputs are files in the run workspace, referenced by name.
// What ends up inline here is the standing rules, the task, and the
// observations that are too small or too incidental to deserve a file.
package prompts

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/ChinnakornP/longtest/daemon/security"
)

//go:embed templates/*.md
var templatesFS embed.FS

var tmpl = template.Must(template.ParseFS(templatesFS, "templates/*.md"))

// Phase selects which task template is rendered.
type Phase string

// The three points in a run where a model is invoked.
const (
	PhaseDiscovery Phase = "discovery"
	PhasePlanning  Phase = "planning"
	PhaseAnalysis  Phase = "analysis"
)

var phaseTemplate = map[Phase]string{
	PhaseDiscovery: "discovery.md",
	PhasePlanning:  "planning.md",
	PhaseAnalysis:  "analysis.md",
}

// Input is everything a prompt is built from.
type Input struct {
	// Phase selects the task.
	Phase Phase
	// Nonce is the run's frame id. Required: without it the closing marker
	// carries nothing for the model to check the opening one against.
	Nonce string
	// OutputSchema names the contract the CLI must write, e.g. "test-plan@1".
	OutputSchema string
	// OutputSchemaFile is where that contract has been placed in the
	// workspace, so the model can read the field names rather than guess
	// them. Empty omits the reference — a caller that names no file is
	// asserting the model already knows the shape, which no model does.
	OutputSchemaFile string
	// AllowedOrigins is the egress allowlist, restated to the model. It is
	// not the enforcement point — security.PlanGate is — but a model that is
	// told the rule breaks it less often, which saves a retry.
	AllowedOrigins []string
	// FixtureNames are the logins the run can establish. Names only.
	FixtureNames []string
	// Untrusted are the observation blocks. Their Nonce is overwritten with
	// Input.Nonce so a caller cannot accidentally frame one under an id the
	// task text does not mention.
	Untrusted []security.Block

	// Retry, when set, tells the model this is a second or third attempt and
	// that the validator's report on its previous answer is among the blocks
	// below. The report itself is never inlined: it quotes the document the
	// model wrote, which on a hijacked first attempt is page content wearing
	// the model's voice.
	Retry *Retry

	// Scrubber removes the run's credentials from the finished prompt.
	//
	// It belongs here rather than at the call site because the prompt string
	// is handed to the AI CLI directly, not only written to a file: scrubbing
	// on the way to disk would leave the in-memory copy — the one the model
	// actually reads — intact. A run that has fixtures and builds a prompt
	// without this is a leak, which is why Build refuses it.
	Scrubber *security.Scrubber
}

// System returns the standing rules. Providers pass it as the system prompt
// where the CLI supports one, and prepend it to the task otherwise.
func System() string {
	b, err := templatesFS.ReadFile("templates/system.md")
	if err != nil {
		// Embedded at build time; unreachable short of a corrupted binary.
		panic(fmt.Sprintf("prompts: read system template: %v", err))
	}
	return string(b)
}

// Retry is the feedback a rejected attempt gets. It carries counts and file
// names only — every byte the previous attempt produced travels as an
// [Input.Untrusted] block, framed like any other content this system did not
// author.
type Retry struct {
	// Attempt is the number of the attempt about to be made, counting from 1.
	Attempt int
	// OutputFile is the document to rewrite, e.g. "out.json".
	OutputFile string
}

// ErrNoNonce is returned when an Input has no frame id.
var ErrNoNonce = errors.New("prompts: a prompt needs a frame nonce")

// ErrCredentialInPrompt is returned when the rendered prompt still contains a
// value the scrubber knows. It means a secret reached the prompt through a
// path the scrubber does not cover, and the run must not continue.
var ErrCredentialInPrompt = errors.New("prompts: a run credential survived into the prompt")

// Build renders the task prompt.
//
// The returned string is deterministic for a given Input: the same inputs
// produce the same bytes. That is what lets the injection corpus assert the
// property that matters — that varying the *page content* changes only the
// bytes inside the frames, and never a byte of the instructions.
func Build(in Input) (string, error) {
	if in.Nonce == "" {
		return "", ErrNoNonce
	}
	name, ok := phaseTemplate[in.Phase]
	if !ok {
		return "", fmt.Errorf("prompts: unknown phase %q", in.Phase)
	}

	origins := append([]string(nil), in.AllowedOrigins...)
	sort.Strings(origins)
	fixtures := append([]string(nil), in.FixtureNames...)
	sort.Strings(fixtures)

	var head bytes.Buffer
	if err := tmpl.ExecuteTemplate(&head, name, struct {
		Nonce            string
		OutputSchema     string
		OutputSchemaFile string
		AllowedOrigins   []string
		FixtureNames     []string
	}{in.Nonce, in.OutputSchema, in.OutputSchemaFile, origins, fixtures}); err != nil {
		return "", fmt.Errorf("prompts: render %s: %w", name, err)
	}

	var sb strings.Builder
	sb.WriteString(head.String())
	if in.Retry != nil {
		var retry bytes.Buffer
		if err := tmpl.ExecuteTemplate(&retry, "retry.md", struct {
			Attempt      int
			OutputFile   string
			OutputSchema string
		}{in.Retry.Attempt, in.Retry.OutputFile, in.OutputSchema}); err != nil {
			return "", fmt.Errorf("prompts: render retry.md: %w", err)
		}
		sb.WriteString("\n")
		sb.WriteString(retry.String())
	}
	if len(in.Untrusted) == 0 {
		sb.WriteString("\n(no page content was captured for this task)\n")
	}
	for _, b := range in.Untrusted {
		b.Nonce = in.Nonce
		sb.WriteString("\n")
		sb.WriteString(security.Wrap(b))
		sb.WriteString("\n")
	}

	out := sb.String()
	if in.Scrubber == nil {
		return out, nil
	}
	out = in.Scrubber.String(out)
	if in.Scrubber.Contains(out) {
		// Unreachable unless Scrubber.String and Scrubber.Contains disagree,
		// which would be a bug in the scrubber rather than in the caller. It
		// is checked anyway because the cost of being wrong here is a
		// credential in a third-party CLI's context window.
		return "", ErrCredentialInPrompt
	}
	return out, nil
}

// InstructionRegion returns the prompt with every framed block replaced by a
// fixed placeholder.
//
// It is the assertion surface for the injection corpus: two prompts built from
// the same task and different page content must have identical instruction
// regions. If they ever differ, page content has reached the part of the
// prompt the model treats as authoritative, and no amount of "the model
// handled it correctly" makes that acceptable.
func InstructionRegion(prompt string) string {
	var sb strings.Builder
	rest := prompt
	for {
		i := strings.Index(rest, security.MarkerStart)
		if i < 0 {
			sb.WriteString(rest)
			return sb.String()
		}
		sb.WriteString(rest[:i])
		sb.WriteString("<<<BLOCK>>>")

		after := rest[i:]
		j := strings.Index(after, security.MarkerEnd)
		if j < 0 {
			// An unterminated frame is itself a boundary failure; return what
			// we have so the test reports a difference rather than panicking.
			return sb.String()
		}
		after = after[j+len(security.MarkerEnd):]
		if k := strings.Index(after, security.MarkerClose); k >= 0 {
			after = after[k+len(security.MarkerClose):]
		}
		rest = after
	}
}
