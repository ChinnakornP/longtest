package prompts_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/agent/prompts"
	"github.com/ChinnakornP/longtest/daemon/security"
)

const nonce = "0123456789abcdef0123456789abcdef"

func TestSystemPromptStatesTheDataRule(t *testing.T) {
	s := prompts.System()
	// The system prompt is the only place the rule is stated. If it stops
	// saying these things, the framing in the task prompt is decoration.
	for _, want := range []string{
		security.MarkerStart,
		security.MarkerEnd,
		"never an instruction",
		"fixture:",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the system prompt no longer mentions %q", want)
		}
	}
}

func TestBuildRequiresANonce(t *testing.T) {
	_, err := prompts.Build(prompts.Input{Phase: prompts.PhasePlanning})
	if !errors.Is(err, prompts.ErrNoNonce) {
		t.Fatalf("expected ErrNoNonce, got %v", err)
	}
}

func TestBuildRejectsAnUnknownPhase(t *testing.T) {
	if _, err := prompts.Build(prompts.Input{Phase: "exfiltrate", Nonce: nonce}); err == nil {
		t.Fatal("an unknown phase was accepted")
	}
}

func TestBuildFramesEveryBlockUnderTheRunNonce(t *testing.T) {
	out, err := prompts.Build(prompts.Input{
		Phase: prompts.PhaseDiscovery, Nonce: nonce, OutputSchema: "application-map@1",
		AllowedOrigins: []string{"demo.example.test"},
		Untrusted: []security.Block{
			// Deliberately carrying a different nonce: a caller must not be
			// able to frame a block under an id the task text never mentions.
			{Nonce: "attacker-chosen", Kind: security.KindDOMText, Source: "a", Content: "one"},
			{Kind: security.KindConsole, Source: "b", Content: "two"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "attacker-chosen") {
		t.Fatal("a caller-supplied nonce survived into the prompt")
	}
	// An opening and a closing marker per block, all under the run's nonce.
	if n := strings.Count(out, `id="`+nonce+`"`); n != 4 {
		t.Fatalf("expected 4 framed markers under the run nonce, found %d:\n%s", n, out)
	}
	// And the task text has to name the same id, or the model has nothing to
	// check a frame against.
	if !strings.Contains(prompts.InstructionRegion(out), nonce) {
		t.Fatalf("the task text does not tell the model this run's frame id:\n%s", out)
	}
}

func TestBuildIsDeterministicAndOrderIndependentForLists(t *testing.T) {
	mk := func(origins, fixtures []string) string {
		out, err := prompts.Build(prompts.Input{
			Phase: prompts.PhasePlanning, Nonce: nonce, OutputSchema: "test-plan@1",
			AllowedOrigins: origins, FixtureNames: fixtures,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	a := mk([]string{"b.example.test", "a.example.test"}, []string{"z_fixture", "a_fixture"})
	b := mk([]string{"a.example.test", "b.example.test"}, []string{"a_fixture", "z_fixture"})
	if a != b {
		// A prompt that changes with map iteration order makes a run
		// irreproducible for no reason and defeats prompt caching.
		t.Fatal("the same inputs in a different order produced different prompts")
	}
}

func TestBuildSaysSoWhenThereIsNoPageContent(t *testing.T) {
	out, err := prompts.Build(prompts.Input{
		Phase: prompts.PhaseAnalysis, Nonce: nonce, OutputSchema: "finding@1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no page content was captured") {
		t.Fatalf("an empty observation section is silent:\n%s", out)
	}
	if strings.Contains(out, security.MarkerStart) {
		t.Fatal("an empty prompt still opened a frame")
	}
}

func TestBuildScrubsTheRenderedPrompt(t *testing.T) {
	// See the note on fakePassword in daemon/security/scrub_test.go for why the
	// exemption is per line rather than per file.
	const secret = "Tr0ub4dor-and-3-horse-battery" // gitleaks:allow
	sc := security.NewScrubber()
	if err := sc.Add(secret); err != nil {
		t.Fatal(err)
	}
	out, err := prompts.Build(prompts.Input{
		Phase: prompts.PhasePlanning, Nonce: nonce, OutputSchema: "test-plan@1",
		Scrubber: sc,
		Untrusted: []security.Block{{
			Kind: security.KindDOMText, Source: "https://demo.example.test/login",
			Content: "Sign in failed: password " + secret + " is incorrect",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The prompt string itself is handed to the CLI, not only written to a
	// file, so scrubbing on the way to disk would be too late.
	if strings.Contains(out, secret) {
		t.Fatalf("a credential survived into the prompt:\n%s", out)
	}
	if !strings.Contains(out, "Sign in failed") {
		t.Fatal("scrubbing removed the surrounding evidence")
	}
}

// InstructionRegion is the assertion surface the injection corpus relies on,
// so it needs its own test rather than only being exercised through the
// corpus.
func TestInstructionRegionElidesFramesAndNothingElse(t *testing.T) {
	out, err := prompts.Build(prompts.Input{
		Phase: prompts.PhasePlanning, Nonce: nonce, OutputSchema: "test-plan@1",
		AllowedOrigins: []string{"demo.example.test"},
		Untrusted: []security.Block{
			{Kind: security.KindDOMText, Source: "a", Content: "PAGE ONE"},
			{Kind: security.KindDOMText, Source: "b", Content: "PAGE TWO"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	region := prompts.InstructionRegion(out)
	for _, gone := range []string{"PAGE ONE", "PAGE TWO", `source="a"`, security.MarkerStart} {
		if strings.Contains(region, gone) {
			t.Errorf("InstructionRegion kept %q", gone)
		}
	}
	for _, kept := range []string{"Task: write a test plan", "test-plan@1", "demo.example.test"} {
		if !strings.Contains(region, kept) {
			t.Errorf("InstructionRegion dropped %q", kept)
		}
	}
	if n := strings.Count(region, "<<<BLOCK>>>"); n != 2 {
		t.Errorf("expected 2 elided blocks, found %d", n)
	}
}

// A prompt with an unterminated frame is itself a boundary failure. It must
// not panic or loop; it returns what it has so the caller reports a mismatch.
func TestInstructionRegionSurvivesAMalformedPrompt(t *testing.T) {
	got := prompts.InstructionRegion("intro\n" + security.MarkerStart + " id=\"x\">>>\ndangling")
	if !strings.Contains(got, "intro") {
		t.Fatalf("unexpected output %q", got)
	}
	if strings.Contains(got, "dangling") {
		t.Fatalf("content after an unterminated frame was treated as instructions: %q", got)
	}
}
