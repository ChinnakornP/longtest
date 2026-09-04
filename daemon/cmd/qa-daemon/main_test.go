package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/runtime"
)

func execute(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	err = run(t.Context(), args, &out, &errBuf)
	return out.String(), errBuf.String(), err
}

func TestVersionAndHelp(t *testing.T) {
	stdout, _, err := execute(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(stdout, runtime.Version) {
		t.Fatalf("version output = %q", stdout)
	}

	stdout, _, err = execute(t, "help")
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, command := range []string{"pair", "start", "status", "doctor"} {
		if !strings.Contains(stdout, command) {
			t.Fatalf("help does not mention %q: %q", command, stdout)
		}
	}
}

func TestNoArgsAndUnknownCommand(t *testing.T) {
	if _, _, err := execute(t); !errors.Is(err, errUsage) {
		t.Fatalf("no args returned %v", err)
	}
	_, stderr, err := execute(t, "frobnicate")
	if !errors.Is(err, errUsage) {
		t.Fatalf("unknown command returned %v", err)
	}
	if !strings.Contains(stderr, "frobnicate") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestPairWritesTheConfigWithoutPrintingTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != runtime.RedeemPath {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"runtimeId":"9f6d1d1c-8b0a-4c3d-9e2f-1a2b3c4d5e6f",
			"runtimeToken":"qart_super_secret",
			"runtimeName":"ci-runner",
			"orgId":"3a2b1c0d-4e5f-6a7b-8c9d-0e1f2a3b4c5d"
		}`))
	}))
	defer srv.Close()

	configPath := filepath.Join(t.TempDir(), "config.json")
	stdout, _, err := execute(t, "pair", "--code", "k7q2-9fmr-3xt8", "--server", srv.URL,
		"--name", "ci-runner", "--config", configPath)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	// A terminal scrollback and a CI log are both places an organization-wide
	// credential must never end up.
	if strings.Contains(stdout, "qart_super_secret") {
		t.Fatalf("pair printed the runtime token: %q", stdout)
	}
	if !strings.Contains(stdout, "ci-runner") || !strings.Contains(stdout, configPath) {
		t.Fatalf("pair output = %q", stdout)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode = %04o, want 0600", perm)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg runtime.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.Token != "qart_super_secret" || cfg.ServerURL != srv.URL {
		t.Fatalf("config = %+v", cfg.LogValue())
	}
}

func TestPairRequiresItsFlags(t *testing.T) {
	if _, _, err := execute(t, "pair", "--code", "k7q2-9fmr-3xt8"); err == nil {
		t.Fatal("pair without --server should fail")
	}
	if _, _, err := execute(t, "pair", "--server", "https://qa.test"); err == nil {
		t.Fatal("pair without --code should fail")
	}
}

func TestStatusReportsWhenNothingIsRunning(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")

	stdout, _, err := execute(t, "status", "--state", statePath, "--output", "json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("status --output json is not JSON: %v (%q)", err, stdout)
	}
	if payload["running"] != false {
		t.Fatalf("status = %v", payload)
	}
}

func TestStatusReadsThePublishedState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	state, err := runtime.NewStateFile(statePath, runtime.State{
		RuntimeID:  "9f6d1d1c-8b0a-4c3d-9e2f-1a2b3c4d5e6f",
		ServerURL:  "https://qa.test",
		Connection: runtime.ConnectionOnline,
	}, nil)
	if err != nil {
		t.Fatalf("NewStateFile: %v", err)
	}
	if err := state.Update(func(s *runtime.State) {
		s.ActiveRuns = []runtime.RunState{{RunID: "run-1", Mode: "full", Phase: "execute"}}
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	stdout, _, err := execute(t, "status", "--state", statePath)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{"9f6d1d1c", "https://qa.test", "online", "run-1", "execute"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("status output is missing %q:\n%s", want, stdout)
		}
	}

	stdout, _, err = execute(t, "status", "--state", statePath, "--output", "json")
	if err != nil {
		t.Fatalf("status --output json: %v", err)
	}
	var payload struct {
		Running    bool `json:"running"`
		ActiveRuns []struct {
			RunID string `json:"runId"`
		} `json:"activeRuns"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.Running || len(payload.ActiveRuns) != 1 {
		t.Fatalf("status json = %+v", payload)
	}
}

// A state file left behind by a crashed daemon must not read as "online".
func TestStatusReportsAStaleStateAsStopped(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{
		"pid": 4194303, "version":"0.1.0", "runtimeId":"r1", "serverUrl":"https://qa.test",
		"connection":"online", "activeRuns":[]
	}`), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	stdout, _, err := execute(t, "status", "--state", statePath, "--output", "json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["running"] == true {
		t.Skip("pid 4194303 exists on this machine")
	}
	if payload["connection"] != "stopped" {
		t.Fatalf("a stale state reported connection %v", payload["connection"])
	}
}

func TestDoctorExitsNonZeroAndPrintsJSON(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	statePath := filepath.Join(t.TempDir(), "state.json")

	stdout, _, err := execute(t, "doctor", "--config", configPath, "--state", statePath, "--output", "json")
	if err == nil {
		t.Fatal("doctor on an unpaired machine should exit non-zero, so provisioning scripts can gate on it")
	}
	var report runtime.Diagnosis
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("doctor --output json is not JSON: %v (%q)", err, stdout)
	}
	if len(report.Checks) == 0 {
		t.Fatal("doctor reported no checks")
	}

	stdout, _, _ = execute(t, "doctor", "--config", configPath, "--state", statePath)
	if !strings.Contains(stdout, "config") || !strings.Contains(stdout, "qa-daemon pair") {
		t.Fatalf("doctor text output = %q", stdout)
	}
}

func TestStartRefusesToRunUnpaired(t *testing.T) {
	err := runStart(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "config.json")}, io.Discard)
	if !errors.Is(err, runtime.ErrNoConfig) {
		t.Fatalf("start returned %v, want ErrNoConfig", err)
	}
}
