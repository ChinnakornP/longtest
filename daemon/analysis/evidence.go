package analysis

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// The evidence collector: everything a failed execution is analysed from,
// assembled without a model.
//
// It runs first because it is the cheap, checkable part. A rule can classify
// from a bundle, a model can be handed one, and the gate that checks the
// model's answer is checking it against this same bundle — so all three agree
// on what the run actually observed, rather than each re-deriving it.

// NetworkEntry is one request, as the executor's network log records it.
// Mirrors CapturedNetworkEntry in daemon/executor/src/evidence.ts; a field
// added there and not here is silently dropped, which is why the analysis
// bundle carries the entries it kept rather than the file.
type NetworkEntry struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	// Status is absent when the request never produced a response at all —
	// connection refused, DNS failure, the browser giving up. That absence is
	// itself the strongest network-error signal there is.
	Status     *int   `json:"status,omitempty"`
	DurationMs *int   `json:"durationMs,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
}

// Failed reports whether this request is worth an analyst's attention: it came
// back an error, or it never came back.
func (e NetworkEntry) Failed() bool { return e.Status == nil || *e.Status >= 400 }

// ConsoleEntry is one console line. Mirrors CapturedConsoleEntry.
type ConsoleEntry struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// PriorOutcome is how the same test case went in an earlier run.
//
// It is the difference between "this has been broken since Tuesday" and "this
// broke with the change you just deployed", which is the first thing a person
// reading a failure wants to know and the last thing a single run can tell
// them.
type PriorOutcome struct {
	RunID        string                 `json:"runId,omitempty"`
	Result       qaschema.Outcome       `json:"result"`
	FailureClass *qaschema.FailureClass `json:"failureClass,omitempty"`
}

// Element is one application-map element the failing case targets, flattened
// with the page it lives on.
type Element struct {
	Ref      string `json:"ref"`
	PagePath string `json:"pagePath"`
	Type     string `json:"type"`
	Label    string `json:"label,omitempty"`
	// Locators is the fallback chain as the map records it. It is in the
	// bundle because a TEST_BUG usually looks like exactly this: a chain whose
	// every entry stopped matching the page.
	Locators []qaschema.Locator `json:"locators,omitempty"`
}

// Bundle is the deterministic evidence for one failed execution.
//
// It is the file handed to the model and the fact base every rule and the
// review gate read. Its JSON form is also uploaded as an artifact, so what the
// analyst reasoned from is something a person can open rather than something
// they have to take on trust.
type Bundle struct {
	// TestCaseRef is the case's own ref (TC-001), which is how findings,
	// executions and artifact keys all name it.
	TestCaseRef string                   `json:"testCaseRef"`
	Execution   qaschema.ExecutionResult `json:"execution"`
	TestCase    *qaschema.TestCase       `json:"testCase,omitempty"`

	// FailedStep is the first step that did not pass, and PrecedingStep the
	// one before it. The predecessor is in the bundle because the cause of a
	// failure is usually the step before the one that reported it: a click
	// that silently did nothing shows up as the assertion after it failing.
	FailedStep    *qaschema.StepResult `json:"failedStep,omitempty"`
	PrecedingStep *qaschema.StepResult `json:"precedingStep,omitempty"`

	FailedAssertions []qaschema.AssertionResult `json:"failedAssertions,omitempty"`

	// Console is the error and warning lines only. A passing run's info
	// chatter is noise that costs tokens and buys nothing.
	Console []ConsoleEntry `json:"consoleErrors,omitempty"`
	// Network is the requests that failed, not the whole log.
	Network []NetworkEntry `json:"failedRequests,omitempty"`

	// Elements is the slice of the application map this case targets.
	Elements []Element `json:"targetedElements,omitempty"`

	Previous *PriorOutcome `json:"previousRun,omitempty"`

	// Artifacts is every artifact this execution produced, which is exactly
	// the set a finding about it may cite.
	Artifacts []qaschema.Artifact `json:"artifacts"`
}

// ArtifactIDs is the citable set, sorted.
func (b Bundle) ArtifactIDs() []string {
	out := make([]string, 0, len(b.Artifacts))
	for _, a := range b.Artifacts {
		out = append(out, a.ID)
	}
	sort.Strings(out)
	return out
}

// StepCount is how many steps the test case declares, which is what bounds a
// finding's stepIndex. Zero when the case is not in this run's set — a finding
// that blames a step of a case we do not have is rejected on the case, not on
// the index.
func (b Bundle) StepCount() int {
	if b.TestCase == nil {
		return 0
	}
	return len(b.TestCase.Steps)
}

// maxEntries bounds the console and network lists that reach a prompt.
//
// A page that logs in a loop can produce tens of thousands of console lines,
// and the model is being asked to explain one failure, not to read a log. The
// entries kept are the ones nearest the failure, which is where the cause is.
const maxEntries = 50

// Collector assembles bundles from a finished execution phase.
type Collector struct {
	// ArtifactDir resolves where one test case's evidence files were written.
	// The executor wrote them; this reads them back. A case whose directory
	// cannot be resolved still gets a bundle — with no console and no network
	// log — because a failure we cannot collect evidence for is still a
	// failure that needs a finding.
	ArtifactDir func(testCaseRef string) (string, error)

	// AppMap is this run's application map, used to attach the elements the
	// failing case targets. Optional.
	AppMap *qaschema.ApplicationMap

	// Previous is the outcome of each case in an earlier run, keyed by ref.
	// Optional; empty on a project's first run.
	Previous map[string]PriorOutcome

	Logger *slog.Logger
}

// Collect returns one bundle per failed execution, in execution order.
//
// Passing executions get nothing: there is nothing to explain, and an analyst
// asked to explain a pass will find something to say.
func (c Collector) Collect(executions []qaschema.ExecutionResult, testCases []qaschema.TestCase) []Bundle {
	byRef := make(map[string]*qaschema.TestCase, len(testCases))
	for i := range testCases {
		byRef[testCases[i].ID] = &testCases[i]
	}

	out := make([]Bundle, 0, len(executions))
	for _, execution := range executions {
		if !IsFailure(execution.Result) {
			continue
		}
		out = append(out, c.bundle(execution, byRef[execution.TestCaseID]))
	}
	return out
}

// IsFailure is the one definition of "this execution needs a finding".
//
// fail and error both count. They are different things — fail is the
// application disagreeing with an assertion, error is the harness never
// getting far enough to have an opinion — but a user looking at a red row
// wants to know why in both cases. skipped does not count: a case that never
// ran has no failure to explain, and inventing one for it would put a verdict
// on the report that nothing observed.
func IsFailure(result qaschema.Outcome) bool {
	return result == qaschema.OutcomeFail || result == qaschema.OutcomeError
}

func (c Collector) bundle(execution qaschema.ExecutionResult, testCase *qaschema.TestCase) Bundle {
	b := Bundle{
		TestCaseRef: execution.TestCaseID,
		Execution:   execution,
		TestCase:    testCase,
		Artifacts:   execution.Artifacts,
	}
	if b.Artifacts == nil {
		b.Artifacts = []qaschema.Artifact{}
	}

	for i := range execution.Steps {
		if execution.Steps[i].Status == qaschema.OutcomePass {
			continue
		}
		b.FailedStep = &execution.Steps[i]
		if i > 0 {
			b.PrecedingStep = &execution.Steps[i-1]
		}
		break
	}
	for i := range execution.Assertions {
		if execution.Assertions[i].Status != qaschema.OutcomePass {
			b.FailedAssertions = append(b.FailedAssertions, execution.Assertions[i])
		}
	}

	b.Console, b.Network = c.logs(execution)
	b.Elements = c.elements(testCase)
	if prior, ok := c.Previous[execution.TestCaseID]; ok {
		b.Previous = &prior
	}
	return b
}

// logs reads the console and network artifacts this execution registered and
// keeps the interesting lines.
//
// A log we cannot read is a warning, never an error: the analyst has the step
// results and the screenshots either way, and failing a run because a console
// log went missing would trade a complete report for no report.
func (c Collector) logs(execution qaschema.ExecutionResult) ([]ConsoleEntry, []NetworkEntry) {
	if c.ArtifactDir == nil {
		return nil, nil
	}
	dir, err := c.ArtifactDir(execution.TestCaseID)
	if err != nil {
		c.warn("could not resolve the evidence directory", "testCaseRef", execution.TestCaseID, "error", err)
		return nil, nil
	}

	var console []ConsoleEntry
	var network []NetworkEntry
	for _, artifact := range execution.Artifacts {
		switch artifact.Kind {
		case qaschema.ArtifactKindConsole:
			var entries []ConsoleEntry
			if c.readArtifact(dir, artifact, &entries) {
				console = append(console, keepConsole(entries)...)
			}
		case qaschema.ArtifactKindNetwork:
			var entries []NetworkEntry
			if c.readArtifact(dir, artifact, &entries) {
				network = append(network, keepNetwork(entries)...)
			}
		}
	}
	return lastN(console, maxEntries), lastN(network, maxEntries)
}

func (c Collector) readArtifact(dir string, artifact qaschema.Artifact, into any) bool {
	// path.Base of the key, matching how the upload path finds the same file:
	// the executor names the object after the file it wrote.
	local := filepath.Join(dir, path.Base(artifact.Key))
	data, err := os.ReadFile(local) //nolint:gosec // a path this daemon handed the executor
	if err != nil {
		c.warn("could not read an evidence file", "artifactId", artifact.ID, "error", err)
		return false
	}
	if err := json.Unmarshal(data, into); err != nil {
		c.warn("an evidence file is not the JSON the contract describes", "artifactId", artifact.ID, "error", err)
		return false
	}
	return true
}

// elements is the part of the application map the failing case touches.
//
// By ref rather than by page, because a case that targets three refs on one
// page does not need the other forty elements of it, and the whole map is a
// document that can run to megabytes.
func (c Collector) elements(testCase *qaschema.TestCase) []Element {
	if c.AppMap == nil || testCase == nil {
		return nil
	}
	wanted := map[string]struct{}{}
	for i := range testCase.Steps {
		if ref, ok := targetRef(testCase.Steps[i].Target); ok {
			wanted[ref] = struct{}{}
		}
	}
	for i := range testCase.Assertions {
		if ref, ok := targetRef(testCase.Assertions[i].Target); ok {
			wanted[ref] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil
	}

	var out []Element
	for _, page := range c.AppMap.Pages {
		for _, element := range page.Elements {
			if _, ok := wanted[element.Ref]; !ok {
				continue
			}
			out = append(out, Element{
				Ref:      element.Ref,
				PagePath: page.Path,
				Type:     string(element.Type),
				Label:    deref(element.Label),
				Locators: element.Locators,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// Encode renders the bundle as the JSON file the model reads and the artifact
// a person can open.
func (b Bundle) Encode() ([]byte, error) {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("analysis: encode the evidence for %s: %w", b.TestCaseRef, err)
	}
	return data, nil
}

func keepConsole(entries []ConsoleEntry) []ConsoleEntry {
	out := make([]ConsoleEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Level == "error" || entry.Level == "warn" {
			out = append(out, entry)
		}
	}
	return out
}

func keepNetwork(entries []NetworkEntry) []NetworkEntry {
	out := make([]NetworkEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Failed() {
			out = append(out, entry)
		}
	}
	return out
}

func lastN[T any](in []T, n int) []T {
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func targetRef(t *qaschema.Target) (string, bool) {
	if t == nil || t.Ref == nil {
		return "", false
	}
	return *t.Ref, true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (c Collector) warn(msg string, args ...any) {
	if c.Logger == nil {
		return
	}
	c.Logger.Warn(msg, args...)
}
