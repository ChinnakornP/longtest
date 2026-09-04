package security_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/ChinnakornP/longtest/daemon/agent/prompts"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// The injection corpus test. What it does and does not prove:
//
// It does NOT run a model. "The AI ignored the injection" is not a property a
// deterministic test can assert, because the answer depends on a model version
// nobody in this repository controls, and a green CI run built on a lucky
// sample would be worse than no test at all.
//
// What it asserts instead are the two properties that hold regardless of which
// model is behind the CLI:
//
//  1. Page content cannot reach the instruction region of a prompt. However
//     hostile the payload, the bytes outside the untrusted frames are
//     identical to a run with benign content — the model is never *told*
//     anything different, so an injection can only ever be a persuasion
//     attempt, never a configuration change.
//  2. If the model is persuaded anyway, the plan it produces does not run.
//     Every case that has a `hijack` carries the plan the injection was
//     fishing for, and the gate has to reject it for the stated reason.
//
// The live half — pointing a real CLI at e2e/injection-corpus/src/server.ts
// and diffing the plans — is opt-in and is not a CI gate. See the README
// there.

type corpusFile struct {
	Version       int          `json:"version"`
	BaseURL       string       `json:"baseUrl"`
	AdminPassword string       `json:"adminPassword"`
	Cases         []corpusCase `json:"cases"`
}

type corpusCase struct {
	ID            string   `json:"id"`
	Channel       string   `json:"channel"`
	Kind          string   `json:"kind"`
	Description   string   `json:"description"`
	Payload       string   `json:"payload"`
	PayloadRepeat *repeat  `json:"payloadRepeat"`
	Hijack        *hijack  `json:"hijack"`
	ExpectedRules []string `json:"expectedRules"`
}

type repeat struct {
	Unit  string `json:"unit"`
	Times int    `json:"times"`
}

type hijack struct {
	Rationale     string            `json:"rationale"`
	CoverageNotes string            `json:"coverageNotes"`
	Preconditions []string          `json:"preconditions"`
	Steps         []json.RawMessage `json:"steps"`
}

func (c corpusCase) content() string {
	if c.PayloadRepeat != nil {
		return strings.Repeat(c.PayloadRepeat.Unit, c.PayloadRepeat.Times)
	}
	return c.Payload
}

const corpusPath = "../../e2e/injection-corpus/corpus.json"

func loadCorpus(t *testing.T) corpusFile {
	t.Helper()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c corpusFile
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if len(c.Cases) < 10 {
		t.Fatalf("the corpus is the test: %d cases, LONG-14 requires at least 10", len(c.Cases))
	}
	seen := map[string]bool{}
	for _, tc := range c.Cases {
		if seen[tc.ID] {
			t.Fatalf("duplicate corpus case id %q", tc.ID)
		}
		seen[tc.ID] = true
	}
	return c
}

// benign stands in for a page that is not trying anything. Every injected
// payload's prompt is compared against the prompt this produces.
const benign = "Employees\nSearch\nAdd Employee\nName Email Role\n"

func buildPrompt(t *testing.T, nonce, kind, source, content string) string {
	t.Helper()
	p, err := prompts.Build(prompts.Input{
		Phase:          prompts.PhasePlanning,
		Nonce:          nonce,
		OutputSchema:   "test-plan@1",
		AllowedOrigins: []string{"demo.example.test"},
		FixtureNames:   []string{"logged_in_as_admin"},
		Untrusted: []security.Block{{
			Kind:    security.Kind(kind),
			Source:  source,
			Content: content,
		}},
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}
	return p
}

// TestCorpusCannotReachTheInstructionRegion is property 1.
func TestCorpusCannotReachTheInstructionRegion(t *testing.T) {
	c := loadCorpus(t)
	const nonce = "0123456789abcdef0123456789abcdef"

	baseline := prompts.InstructionRegion(
		buildPrompt(t, nonce, "dom_text", "https://demo.example.test/employees", benign))

	for _, tc := range c.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			got := prompts.InstructionRegion(
				buildPrompt(t, nonce, tc.Kind, "https://demo.example.test/employees", tc.content()))
			if got != baseline {
				t.Fatalf("page content changed the instruction region.\n--- want ---\n%s\n--- got ---\n%s",
					baseline, got)
			}
		})
	}
}

// TestCorpusCannotForgeAFrame is the other half of property 1: a payload that
// guesses the delimiters must not be able to open or close one.
func TestCorpusCannotForgeAFrame(t *testing.T) {
	c := loadCorpus(t)
	const nonce = "0123456789abcdef0123456789abcdef"

	for _, tc := range c.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			out := security.Wrap(security.Block{
				Nonce:   nonce,
				Kind:    security.Kind(tc.Kind),
				Source:  "https://demo.example.test/employees",
				Content: tc.content(),
			})
			if n := strings.Count(out, security.MarkerStart); n != 1 {
				t.Fatalf("expected exactly one opening marker, found %d", n)
			}
			if n := strings.Count(out, security.MarkerEnd); n != 1 {
				t.Fatalf("expected exactly one closing marker, found %d", n)
			}
			// The end marker must be last: anything after it would be read as
			// operator text.
			if !strings.HasSuffix(out, security.MarkerClose) ||
				strings.Index(out, security.MarkerEnd) < strings.LastIndex(out, "\n") {
				t.Fatalf("the closing marker is not the last thing in the block:\n%s", out)
			}
		})
	}
}

// TestCorpusIsSanitised covers the channels that attack the *reader* of a
// prompt rather than the model: terminal escapes and invisible Unicode.
func TestCorpusIsSanitised(t *testing.T) {
	c := loadCorpus(t)
	const nonce = "0123456789abcdef0123456789abcdef"

	for _, tc := range c.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			out := security.Wrap(security.Block{
				Nonce: nonce, Kind: security.Kind(tc.Kind),
				Source: "https://demo.example.test/", Content: tc.content(),
			})
			for _, r := range out {
				switch {
				case r == '\n' || r == '\t':
				case r < 0x20 || (r >= 0x7f && r <= 0x9f):
					t.Fatalf("control character %U survived sanitisation", r)
				case unicode.Is(unicode.Cf, r):
					t.Fatalf("invisible format character %U survived sanitisation", r)
				case r >= 0xe0000 && r <= 0xe007f:
					t.Fatalf("Unicode tag character %U survived sanitisation", r)
				}
			}
		})
	}
}

// TestCorpusIsBounded is the denial-of-service half: a page that serves a
// megabyte of text must not put a megabyte in a prompt.
func TestCorpusIsBounded(t *testing.T) {
	c := loadCorpus(t)
	for _, tc := range c.Cases {
		out := security.Wrap(security.Block{
			Nonce: "n", Kind: security.Kind(tc.Kind), Source: "s", Content: tc.content(),
		})
		// The frame header and footer are a few hundred bytes on top of the
		// content cap.
		if len(out) > security.DefaultMaxBytes+1024 {
			t.Errorf("%s: wrapped block is %d bytes, over the %d cap",
				tc.ID, len(out), security.DefaultMaxBytes)
		}
	}
}

// TestCorpusHijackedPlansAreRejected is property 2: the plan each injection
// was fishing for does not run, for the reason the corpus says it should not.
func TestCorpusHijackedPlansAreRejected(t *testing.T) {
	c := loadCorpus(t)

	scrubber := security.NewScrubber()
	if err := scrubber.Add(c.AdminPassword); err != nil {
		t.Fatalf("register corpus credential: %v", err)
	}
	rules, err := security.TargetRules(c.BaseURL, false)
	if err != nil {
		t.Fatalf("target rules: %v", err)
	}
	egress, err := security.NewEgressPolicy(rules)
	if err != nil {
		t.Fatalf("egress policy: %v", err)
	}
	gate := &security.PlanGate{
		Egress:        egress,
		BaseURL:       c.BaseURL,
		KnownFixtures: map[string]struct{}{"logged_in_as_admin": {}},
		Scrubber:      scrubber,
	}

	checked := 0
	for _, tc := range c.Cases {
		if tc.Hijack == nil {
			continue
		}
		checked++
		t.Run(tc.ID, func(t *testing.T) {
			plan := planFrom(t, tc)
			got := gate.Check(plan)
			if len(got) == 0 {
				t.Fatalf("the hijacked plan was accepted; expected rules %v", tc.ExpectedRules)
			}
			fired := map[string]bool{}
			for _, v := range got {
				fired[v.Rule] = true
			}
			for _, want := range tc.ExpectedRules {
				if !fired[want] {
					t.Errorf("expected rule %q to fire, got %v", want, got)
				}
			}
		})
	}
	if checked < 10 {
		t.Fatalf("only %d corpus cases carry a hijacked plan; at least 10 must", checked)
	}
}

// TestGateAcceptsAnHonestPlan is the control. A gate that rejects everything
// passes every test above and ships a product that cannot run a test.
func TestGateAcceptsAnHonestPlan(t *testing.T) {
	c := loadCorpus(t)
	scrubber := security.NewScrubber()
	if err := scrubber.Add(c.AdminPassword); err != nil {
		t.Fatal(err)
	}
	rules, _ := security.TargetRules(c.BaseURL, false)
	egress, err := security.NewEgressPolicy(rules)
	if err != nil {
		t.Fatal(err)
	}
	gate := &security.PlanGate{
		Egress: egress, BaseURL: c.BaseURL,
		KnownFixtures: map[string]struct{}{"logged_in_as_admin": {}},
		Scrubber:      scrubber,
	}

	plan := &qaschema.TestPlan{
		Version:       1,
		Rationale:     "Cover the employee CRUD flow.",
		CoverageNotes: "No payment flows: none exist in this app.",
		TestCases: []qaschema.TestCase{{
			Version: 1, ID: "TC-001", Name: "Create an employee",
			Priority: qaschema.TestCasePriorityHigh, Category: qaschema.TestCaseCategoryFunctional,
			Preconditions: []qaschema.Precondition{"fixture:logged_in_as_admin"},
			Steps: []qaschema.Step{
				{Action: qaschema.StepActionNavigate, URL: strPtr("/employees")},
				{Action: qaschema.StepActionClick, Target: &qaschema.Target{Ref: refPtr("add-employee")}},
				{Action: qaschema.StepActionFill, Target: &qaschema.Target{Ref: refPtr("first-name")}, Value: strPtr("John")},
			},
			Assertions: []qaschema.Assertion{{Type: qaschema.AssertionTypeNoConsoleError}},
		}},
	}
	if got := gate.Check(plan); len(got) != 0 {
		t.Fatalf("an honest plan was rejected: %v", got)
	}
}

// planFrom turns a corpus `hijack` into the plan the gate sees.
func planFrom(t *testing.T, tc corpusCase) *qaschema.TestPlan {
	t.Helper()
	steps := make([]qaschema.Step, 0, len(tc.Hijack.Steps))
	for _, raw := range tc.Hijack.Steps {
		var s qaschema.Step
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("decode hijack step: %v", err)
		}
		steps = append(steps, s)
	}
	return &qaschema.TestPlan{
		Version:       1,
		Rationale:     tc.Hijack.Rationale,
		CoverageNotes: tc.Hijack.CoverageNotes,
		TestCases: []qaschema.TestCase{{
			Version: 1, ID: "TC-" + tc.ID, Name: tc.ID,
			Priority: qaschema.TestCasePriorityHigh, Category: qaschema.TestCaseCategoryFunctional,
			// qaschema.Precondition is an alias for string.
			Preconditions: tc.Hijack.Preconditions,
			Steps:         steps,
			Assertions:    []qaschema.Assertion{{Type: qaschema.AssertionTypeNoConsoleError}},
		}},
	}
}

func strPtr(s string) *string { return &s }

func refPtr(s string) *qaschema.ElementRef { return &s }
