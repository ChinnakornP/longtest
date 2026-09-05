package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// fakeCLI writes an executable shell script and returns its path.
func fakeCLI(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func lookup(paths map[string]string) func(string) (string, error) {
	return func(binary string) (string, error) {
		if path, ok := paths[binary]; ok {
			return path, nil
		}
		return "", errors.New("not found")
	}
}

func capByName(caps []qaschema.AgentCapability, name qaschema.AgentCapabilityName) qaschema.AgentCapability {
	for _, c := range caps {
		if c.Name == name {
			return c
		}
	}
	return qaschema.AgentCapability{}
}

func TestDetectReportsEveryKnownCLI(t *testing.T) {
	claude := fakeCLI(t, "claude", "echo '1.2.3 (Claude Code)'\n")

	caps := Detect(t.Context(), DetectOptions{
		LookPath: lookup(map[string]string{"claude": claude}),
		Timeout:  5 * time.Second,
		// This test is about which binaries are present, so the credential
		// question is answered for it. Readiness has its own tests.
		Auth: stubAuth(ReadinessReady, ""),
	})

	if len(caps) != len(Known) {
		t.Fatalf("reported %d agents, want %d — a missing CLI is reported, not omitted", len(caps), len(Known))
	}

	got := capByName(caps, qaschema.AgentCapabilityNameClaude)
	if !got.Ok {
		t.Fatalf("claude ok = false: %v", derefErr(got))
	}
	if got.Version == nil || *got.Version != "1.2.3 (Claude Code)" {
		t.Fatalf("claude version = %v", got.Version)
	}
	if got.Error != nil {
		t.Fatalf("a working CLI must not carry an error: %v", *got.Error)
	}

	missing := capByName(caps, qaschema.AgentCapabilityNameOpencode)
	if missing.Ok {
		t.Fatal("opencode reported ok, but it is not installed in this test")
	}
	if missing.Error == nil || !strings.Contains(*missing.Error, "PATH") {
		t.Fatalf("missing CLI error is not actionable: %v", derefErr(missing))
	}
}

func TestDetectReportsBrokenCLI(t *testing.T) {
	broken := fakeCLI(t, "claude", "echo 'cannot find module' >&2\nexit 1\n")

	caps := Detect(t.Context(), DetectOptions{
		LookPath: lookup(map[string]string{"claude": broken}),
		Only:     []qaschema.AgentCapabilityName{qaschema.AgentCapabilityNameClaude},
	})
	if len(caps) != 1 {
		t.Fatalf("Only was ignored: got %d", len(caps))
	}
	got := caps[0]
	if got.Ok {
		t.Fatal("a CLI that exits non-zero must not be reported ok")
	}
	if got.Error == nil || !strings.Contains(*got.Error, "cannot find module") {
		t.Fatalf("error should quote what the CLI said: %v", derefErr(got))
	}
}

func TestDetectTimesOutHangingCLI(t *testing.T) {
	hanging := fakeCLI(t, "claude", "sleep 30\n")

	start := time.Now()
	caps := Detect(t.Context(), DetectOptions{
		LookPath: lookup(map[string]string{"claude": hanging}),
		Timeout:  200 * time.Millisecond,
		Only:     []qaschema.AgentCapabilityName{qaschema.AgentCapabilityNameClaude},
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("detection waited %s for a hanging CLI", elapsed)
	}
	if caps[0].Ok {
		t.Fatal("a hanging CLI must not be reported ok")
	}
	if caps[0].Error == nil || !strings.Contains(*caps[0].Error, "did not answer") {
		t.Fatalf("error = %v", derefErr(caps[0]))
	}
}

func TestDetectProducesContractValidHello(t *testing.T) {
	claude := fakeCLI(t, "claude", "echo 1.2.3\n")
	caps := Detect(t.Context(), DetectOptions{LookPath: lookup(map[string]string{"claude": claude})})

	agents := make([]any, 0, len(caps))
	for _, c := range caps {
		entry := map[string]any{"name": string(c.Name), "ok": c.Ok}
		if c.Version != nil {
			entry["version"] = *c.Version
		}
		if c.Error != nil {
			entry["error"] = *c.Error
		}
		agents = append(agents, entry)
	}

	result, err := qaschema.Validate("daemon-envelope@1", map[string]any{
		"v":     float64(1),
		"type":  "hello",
		"msgId": "5b1f1d4a-3a2b-4c6d-9e8f-0a1b2c3d4e5f",
		"seq":   float64(0),
		"ts":    "2026-09-04T12:00:00Z",
		"payload": map[string]any{
			"runtimeId": "5b1f1d4a-3a2b-4c6d-9e8f-0a1b2c3d4e5f",
			"version":   "0.1.0",
			"os":        "linux",
			"browsers":  []any{"chromium"},
			"agents":    agents,
		},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !result.Valid {
		t.Fatalf("detected agents do not fit the hello contract: %v", result.Errors)
	}
}

func TestVersionIsTruncated(t *testing.T) {
	long := strings.Repeat("v", 200)
	cli := fakeCLI(t, "claude", "echo "+long+"\n")

	caps := Detect(t.Context(), DetectOptions{
		LookPath: lookup(map[string]string{"claude": cli}),
		Only:     []qaschema.AgentCapabilityName{qaschema.AgentCapabilityNameClaude},
	})
	if caps[0].Version == nil || len(*caps[0].Version) != 64 {
		t.Fatalf("version was not truncated to the contract maximum: %v", caps[0].Version)
	}
}

func derefErr(c qaschema.AgentCapability) string {
	if c.Error == nil {
		return "<nil>"
	}
	return *c.Error
}

// stubAuth answers the credential question the same way for every CLI, so a
// test about installation does not depend on what the developer happens to be
// logged into.
func stubAuth(r Readiness, detail string) AuthProbe {
	return func(CLI, string, Host) (Readiness, string) { return r, detail }
}
