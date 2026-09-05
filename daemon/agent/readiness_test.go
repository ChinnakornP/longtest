package agent

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// hostWith describes a machine: which variables are set and which paths exist.
func hostWith(home string, env map[string]string, paths ...string) Host {
	present := map[string]bool{}
	for _, p := range paths {
		present[p] = true
	}
	return Host{
		Getenv:  func(k string) string { return env[k] },
		Exists:  func(p string) bool { return present[p] },
		HomeDir: home,
	}
}

// The three states an operator can be in are three different answers, because
// they are three different fixes: install it, log in, or nothing.
func TestDetectDistinguishesMissingUnauthenticatedAndReady(t *testing.T) {
	installed := fakeCLI(t, "claude", "echo '2.1.0 (Claude Code)'\n")
	home := "/home/tester"
	configDir := filepath.Join(home, ".claude")

	for _, tc := range []struct {
		name      string
		lookPath  map[string]string
		host      Host
		want      Readiness
		wantOK    bool
		detailHas string
	}{
		{
			name:      "not installed",
			lookPath:  map[string]string{},
			host:      hostWith(home, nil),
			want:      ReadinessMissing,
			detailHas: "not on PATH",
		},
		{
			name:      "installed, never logged in",
			lookPath:  map[string]string{"claude": installed},
			host:      hostWith(home, nil),
			want:      ReadinessUnauthenticated,
			detailHas: "no credential was found",
		},
		{
			name:     "logged in through the config directory",
			lookPath: map[string]string{"claude": installed},
			host:     hostWith(home, nil, filepath.Join(configDir, ".credentials.json")),
			want:     ReadinessReady,
			wantOK:   true,
		},
		{
			name:     "authenticated through the environment",
			lookPath: map[string]string{"claude": installed},
			host:     hostWith(home, map[string]string{"ANTHROPIC_API_KEY": "sk-test"}),
			want:     ReadinessReady,
			wantOK:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := DetectAll(t.Context(), DetectOptions{
				LookPath: lookup(tc.lookPath),
				Only:     []qaschema.AgentCapabilityName{qaschema.AgentCapabilityNameClaude},
				Host:     &tc.host,
			})
			if len(caps) != 1 {
				t.Fatalf("caps = %d", len(caps))
			}
			got := caps[0]
			if got.Readiness != tc.want {
				t.Fatalf("readiness = %q, want %q (detail: %s)", got.Readiness, tc.want, got.Detail)
			}
			if got.Usable() != tc.wantOK {
				t.Fatalf("usable = %v, want %v", got.Usable(), tc.wantOK)
			}
			if tc.detailHas != "" && !strings.Contains(got.Detail, tc.detailHas) {
				t.Fatalf("detail = %q, want it to mention %q", got.Detail, tc.detailHas)
			}
		})
	}
}

// "Log in" is only useful advice if it comes with the command.
func TestUnauthenticatedDetailNamesTheLoginCommand(t *testing.T) {
	installed := fakeCLI(t, "claude", "echo 2.1.0\n")
	host := hostWith("/home/tester", nil)

	caps := DetectAll(t.Context(), DetectOptions{
		LookPath: lookup(map[string]string{"claude": installed}),
		Only:     []qaschema.AgentCapabilityName{qaschema.AgentCapabilityNameClaude},
		Host:     &host,
	})
	if !strings.Contains(caps[0].Detail, "claude setup-token") {
		t.Fatalf("detail = %q", caps[0].Detail)
	}
}

// An installed-but-unauthenticated CLI still reports its version: the operator
// needs to know which build is refusing to run.
func TestUnauthenticatedCLIStillReportsItsVersion(t *testing.T) {
	installed := fakeCLI(t, "claude", "echo '2.1.0 (Claude Code)'\n")
	host := hostWith("/home/tester", nil)

	caps := Detect(t.Context(), DetectOptions{
		LookPath: lookup(map[string]string{"claude": installed}),
		Only:     []qaschema.AgentCapabilityName{qaschema.AgentCapabilityNameClaude},
		Host:     &host,
	})
	if caps[0].Ok {
		t.Fatal("an unauthenticated CLI was reported ok")
	}
	if caps[0].Version == nil || *caps[0].Version != "2.1.0 (Claude Code)" {
		t.Fatalf("version = %v", caps[0].Version)
	}
}

// CLAUDE_CONFIG_DIR is what the provider mounts into the sandbox, so detection
// and launching must agree on where it is.
func TestClaudeConfigDirFollowsTheEnvironment(t *testing.T) {
	host := hostWith("/home/tester", map[string]string{"CLAUDE_CONFIG_DIR": "/etc/claude"})
	if got := ClaudeConfigDir(host); got != "/etc/claude" {
		t.Fatalf("config dir = %q", got)
	}
	host = hostWith("/home/tester", nil)
	if got := ClaudeConfigDir(host); got != "/home/tester/.claude" {
		t.Fatalf("config dir = %q", got)
	}
}

// The registry's detection is the daemon's single source of truth for the
// hello frame, so it must cover every provider it can dispatch to.
func TestRegistryDetectsEveryRegisteredProvider(t *testing.T) {
	claude := NewMockProvider(MockOptions{As: qaschema.AgentCapabilityNameClaude})
	opencode := NewMockProvider(MockOptions{
		As: qaschema.AgentCapabilityNameOpencode,
		Capability: &Capability{
			Name: qaschema.AgentCapabilityNameOpencode, Readiness: ReadinessMissing,
			Detail: "opencode is not on PATH",
		},
	})
	registry := NewRegistry(claude, opencode)

	caps := registry.Detect(t.Context())
	if len(caps) != 2 {
		t.Fatalf("caps = %d", len(caps))
	}
	if !caps[0].Usable() || caps[1].Usable() {
		t.Fatalf("caps = %+v", caps)
	}

	wire := Schema(caps)
	if wire[1].Error == nil || !strings.Contains(*wire[1].Error, "not on PATH") {
		t.Fatalf("the hello frame does not say why opencode is unavailable: %+v", wire[1])
	}
}

// A run that named no agent gets the first usable one; a run that named an
// unusable one gets an error rather than a substitute, because two runs of the
// same suite against different models are not comparable.
func TestSelectFallsBackButNeverSubstitutes(t *testing.T) {
	broken := NewMockProvider(MockOptions{
		As:         qaschema.AgentCapabilityNameClaude,
		Capability: &Capability{Readiness: ReadinessUnauthenticated, Detail: "not logged in"},
	})
	working := NewMockProvider(MockOptions{As: qaschema.AgentCapabilityNameOpencode})
	registry := NewRegistry(broken, working)

	got, capability, err := registry.Select(t.Context(), "", "")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if got.Name() != qaschema.AgentCapabilityNameOpencode || capability.Name != qaschema.AgentCapabilityNameOpencode {
		t.Fatalf("selected %q", got.Name())
	}

	if _, _, err := registry.Select(t.Context(), qaschema.AgentCapabilityNameClaude, ""); err == nil {
		t.Fatal("an explicitly requested unusable agent was silently replaced")
	}
}

func TestSelectSaysWhyNothingIsUsable(t *testing.T) {
	registry := NewRegistry(NewMockProvider(MockOptions{
		As:         qaschema.AgentCapabilityNameClaude,
		Capability: &Capability{Readiness: ReadinessMissing, Detail: "claude is not on PATH"},
	}))
	_, _, err := registry.Select(t.Context(), "", "")
	if err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("error = %v", err)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Status != StatusUnavailable {
		t.Fatalf("error is not an unavailable failure: %#v", err)
	}
}
