package runtime

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkByName(t *testing.T, report Diagnosis, name string) Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("no %q check in %+v", name, report.Checks)
	return Check{}
}

// doctor exists to answer "why can this machine not run a test?" before a run
// has to fail to find out.
func TestDoctorReportsAnUnpairedMachine(t *testing.T) {
	withEnv(t, nil)

	report := Doctor(t.Context(), DoctorOptions{
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		StatePath:  filepath.Join(t.TempDir(), "state.json"),
	})

	if report.OK() {
		t.Fatal("an unpaired machine is not OK")
	}
	config := checkByName(t, report, "config")
	if config.Status != CheckError {
		t.Fatalf("config check = %+v", config)
	}
	if !strings.Contains(config.Hint, "qa-daemon pair") {
		t.Fatalf("the config check should say what to run: %+v", config)
	}
	// With no config there is nothing to probe, so the backend check is not
	// reported as a separate failure the operator has to chase.
	for _, check := range report.Checks {
		if check.Name == "backend" {
			t.Fatal("doctor probed a backend it has no address for")
		}
	}
}

func TestDoctorExplainsAMissingChromium(t *testing.T) {
	withEnv(t, nil)
	// An empty browsers cache: chromium was never installed.
	t.Setenv("PLAYWRIGHT_BROWSERS_PATH", t.TempDir())

	report := Doctor(t.Context(), DoctorOptions{
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		StatePath:  filepath.Join(t.TempDir(), "state.json"),
	})

	chromium := checkByName(t, report, "chromium")
	if chromium.Status != CheckError {
		t.Fatalf("chromium check = %+v", chromium)
	}
	if chromium.Hint == "" || !strings.Contains(chromium.Hint, "playwright install") {
		t.Fatalf("the chromium check must name the command that fixes it: %+v", chromium)
	}
}

func TestDoctorExplainsMissingAgents(t *testing.T) {
	withEnv(t, nil)
	// An empty PATH: no AI CLI can be found.
	t.Setenv("PATH", t.TempDir())

	report := Doctor(t.Context(), DoctorOptions{
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		StatePath:  filepath.Join(t.TempDir(), "state.json"),
	})

	agents := checkByName(t, report, "agents")
	if agents.Status != CheckWarn {
		// A missing AI CLI is a warning, not an error: approved regression
		// suites still run without one.
		t.Fatalf("agents check = %+v", agents)
	}
	if !strings.Contains(agents.Detail, "discovery") {
		t.Fatalf("the summary should say what stops working: %+v", agents)
	}
	claude := checkByName(t, report, "agent:claude")
	if claude.Status != CheckWarn || claude.Detail == "" {
		t.Fatalf("per-agent check = %+v", claude)
	}
}

func TestDoctorProbesTheBackend(t *testing.T) {
	withEnv(t, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(configPath, Config{
		ServerURL: srv.URL, RuntimeID: "9f6d1d1c-8b0a-4c3d-9e2f-1a2b3c4d5e6f", Token: "qart_x",
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspaces"),
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	report := Doctor(t.Context(), DoctorOptions{ConfigPath: configPath, StatePath: filepath.Join(t.TempDir(), "state.json")})

	backend := checkByName(t, report, "backend")
	if backend.Status != CheckOK {
		t.Fatalf("backend check = %+v", backend)
	}
	// The control-plane URL is reported so an operator can see what the daemon
	// will actually dial, not just what they typed.
	if !strings.Contains(backend.Detail, DaemonPath) {
		t.Fatalf("backend check should name the control-plane URL: %+v", backend)
	}

	workspace := checkByName(t, report, "workspace")
	if workspace.Status != CheckOK {
		t.Fatalf("workspace check = %+v", workspace)
	}
}

func TestDoctorReportsAnUnreachableBackend(t *testing.T) {
	withEnv(t, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(configPath, Config{
		ServerURL: url, RuntimeID: "r1", Token: "qart_x",
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspaces"),
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	report := Doctor(t.Context(), DoctorOptions{ConfigPath: configPath, StatePath: filepath.Join(t.TempDir(), "state.json")})

	backend := checkByName(t, report, "backend")
	if backend.Status != CheckError {
		t.Fatalf("backend check = %+v", backend)
	}
	if !strings.Contains(backend.Hint, "proxy") {
		t.Fatalf("an unreachable backend should point at the usual causes: %+v", backend)
	}
	if report.OK() {
		t.Fatal("a runtime that cannot reach the backend is not OK")
	}
}

func TestDoctorReportsAnUnwritableWorkspaceRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	withEnv(t, nil)

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(configPath, Config{
		ServerURL: "https://qa.test", RuntimeID: "r1", Token: "qart_x",
		WorkspaceRoot: filepath.Join(parent, "workspaces"),
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	report := Doctor(t.Context(), DoctorOptions{ConfigPath: configPath, StatePath: filepath.Join(t.TempDir(), "state.json")})
	workspace := checkByName(t, report, "workspace")
	if workspace.Status != CheckError {
		t.Fatalf("workspace check = %+v", workspace)
	}
}

func TestDoctorReportsWhetherTheDaemonIsRunning(t *testing.T) {
	withEnv(t, nil)

	statePath := filepath.Join(t.TempDir(), "state.json")
	report := Doctor(t.Context(), DoctorOptions{ConfigPath: filepath.Join(t.TempDir(), "c.json"), StatePath: statePath})
	if check := checkByName(t, report, "daemon"); check.Status != CheckWarn || check.Hint != "qa-daemon start" {
		t.Fatalf("daemon check = %+v", check)
	}

	if _, err := NewStateFile(statePath, State{Connection: ConnectionOnline}, nil); err != nil {
		t.Fatalf("NewStateFile: %v", err)
	}
	report = Doctor(t.Context(), DoctorOptions{ConfigPath: filepath.Join(t.TempDir(), "c.json"), StatePath: statePath})
	if check := checkByName(t, report, "daemon"); check.Status != CheckOK {
		t.Fatalf("daemon check = %+v", check)
	}
}

func TestDoctorReportsABrokenExecutor(t *testing.T) {
	withEnv(t, map[string]string{"QA_DAEMON_EXECUTOR": "qa-executor-that-is-not-installed"})

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(configPath, Config{
		ServerURL: "https://qa.test", RuntimeID: "r1", Token: "qart_x",
		WorkspaceRoot: filepath.Join(t.TempDir(), "workspaces"),
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	report := Doctor(t.Context(), DoctorOptions{ConfigPath: configPath, StatePath: filepath.Join(t.TempDir(), "state.json")})
	check := checkByName(t, report, "executor")
	if check.Status != CheckError {
		t.Fatalf("executor check = %+v", check)
	}
	if check.Hint == "" {
		t.Fatalf("the executor check should say how to fix it: %+v", check)
	}
}
