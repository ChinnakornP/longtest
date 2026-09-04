package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChinnakornP/longtest/daemon/agent"
	"github.com/ChinnakornP/longtest/daemon/browser"
	"github.com/ChinnakornP/longtest/daemon/proc"
)

// CheckStatus is how a doctor check came out.
type CheckStatus string

// The three outcomes a check can have. A warning is something that will stop
// some runs but not all — a missing AI CLI blocks discovery and planning while
// leaving a regression run of approved test cases perfectly runnable.
const (
	CheckOK    CheckStatus = "ok"
	CheckWarn  CheckStatus = "warn"
	CheckError CheckStatus = "error"
)

// Check is one diagnosis.
type Check struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail"`
	// Hint is the command or edit that fixes it, when there is one.
	Hint string `json:"hint,omitempty"`
}

// Diagnosis is the whole doctor report.
type Diagnosis struct {
	Version string  `json:"version"`
	Checks  []Check `json:"checks"`
}

// OK reports whether nothing is broken outright.
func (d Diagnosis) OK() bool {
	for _, check := range d.Checks {
		if check.Status == CheckError {
			return false
		}
	}
	return true
}

// DoctorOptions locate the files doctor inspects.
type DoctorOptions struct {
	ConfigPath string
	StatePath  string
	// HTTPClient probes the backend; nil means a short-timeout default.
	HTTPClient *http.Client
}

// Doctor answers "why can this machine not run a test?" without needing a run
// to fail first.
//
// Every failing check carries the reason and the command that fixes it: the
// two questions an operator has when a runtime shows up unusable are "what is
// missing" and "what do I type", and a check that answers only the first is
// half a diagnostic.
func Doctor(ctx context.Context, opts DoctorOptions) Diagnosis {
	report := Diagnosis{Version: Version}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	cfg, cfgCheck := doctorConfig(opts.ConfigPath)
	report.Checks = append(report.Checks, cfgCheck)

	if cfgCheck.Status == CheckOK {
		report.Checks = append(report.Checks, doctorBackend(ctx, cfg, client))
	}

	report.Checks = append(report.Checks, doctorBrowser(ctx))
	report.Checks = append(report.Checks, doctorExecutor(ctx, cfg))
	report.Checks = append(report.Checks, doctorAgents(ctx)...)
	report.Checks = append(report.Checks, doctorWorkspace(cfg))
	report.Checks = append(report.Checks, doctorDaemon(opts.StatePath))
	return report
}

func doctorConfig(path string) (Config, Check) {
	check := Check{Name: "config"}
	if path == "" {
		resolved, err := ConfigPath()
		if err != nil {
			check.Status, check.Detail = CheckError, err.Error()
			return Config{}, check
		}
		path = resolved
	}

	cfg, err := LoadConfig(path)
	switch {
	case errors.Is(err, ErrNoConfig):
		check.Status = CheckError
		check.Detail = fmt.Sprintf("this machine is not paired (%s does not exist)", path)
		check.Hint = "qa-daemon pair --code <pairing-code> --server <url>"
		return Config{}, check
	case err != nil:
		check.Status = CheckError
		check.Detail = err.Error()
		if strings.Contains(err.Error(), "chmod") {
			check.Hint = fmt.Sprintf("chmod 600 %s", path)
		}
		return Config{}, check
	}

	check.Status = CheckOK
	check.Detail = fmt.Sprintf("paired as runtime %s against %s", cfg.RuntimeID, cfg.ServerURL)
	return cfg, check
}

func doctorBackend(ctx context.Context, cfg Config, client *http.Client) Check {
	check := Check{Name: "backend"}

	endpoint, err := cfg.APIURL("/healthz")
	if err != nil {
		check.Status, check.Detail = CheckError, err.Error()
		return check
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		check.Status, check.Detail = CheckError, err.Error()
		return check
	}
	resp, err := client.Do(req)
	if err != nil {
		check.Status = CheckError
		check.Detail = fmt.Sprintf("cannot reach %s: %v", endpoint, err)
		check.Hint = "check serverUrl in the daemon config, and that this machine can reach it (proxy, VPN, firewall)"
		return check
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		check.Status = CheckError
		check.Detail = fmt.Sprintf("%s answered %s", endpoint, resp.Status)
		return check
	}
	wsURL, _ := cfg.WebSocketURL()
	check.Status = CheckOK
	check.Detail = fmt.Sprintf("%s is reachable; control plane is %s", endpoint, wsURL)
	return check
}

func doctorBrowser(ctx context.Context) Check {
	check := Check{Name: "chromium"}

	found, err := browser.Detect(browser.Options{})
	if err != nil {
		check.Status = CheckError
		check.Detail = err.Error()
		check.Hint = browser.InstallHint
		return check
	}

	version, err := browser.Version(ctx, found)
	if err != nil {
		check.Status = CheckError
		check.Detail = fmt.Sprintf("%s is installed but will not run: %v", found.ExecutablePath, err)
		check.Hint = browser.InstallHint
		return check
	}
	check.Status = CheckOK
	check.Detail = fmt.Sprintf("%s (%s, from %s)", version, found.Build, found.Source)
	return check
}

func doctorExecutor(ctx context.Context, cfg Config) Check {
	check := Check{Name: "executor"}
	command := cfg.Executor()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var out lineBuffer
	cmd, err := proc.Start(proc.Options{
		Name:   command[0],
		Args:   append(append([]string{}, command[1:]...), "--version"),
		Stdout: &out,
		Stderr: &out,
	})
	if err != nil {
		check.Status = CheckError
		check.Detail = fmt.Sprintf("cannot run %s: %v", strings.Join(command, " "), err)
		check.Hint = "install the executor sidecar, or set executorCommand in the daemon config"
		return check
	}

	select {
	case <-cmd.Done():
		if err := cmd.Wait(); err != nil {
			check.Status = CheckError
			check.Detail = fmt.Sprintf("%s --version failed: %v: %s", command[0], err, out.First())
			return check
		}
	case <-ctx.Done():
		_ = cmd.Terminate(context.WithoutCancel(ctx), time.Second)
		check.Status = CheckError
		check.Detail = fmt.Sprintf("%s --version did not answer", command[0])
		return check
	}

	check.Status = CheckOK
	check.Detail = out.First()
	return check
}

func doctorAgents(ctx context.Context) []Check {
	caps := agent.Detect(ctx, agent.DetectOptions{})

	checks := make([]Check, 0, len(caps)+1)
	usable := 0
	for _, capability := range caps {
		check := Check{Name: "agent:" + string(capability.Name)}
		switch {
		case capability.Ok:
			usable++
			check.Status = CheckOK
			if capability.Version != nil {
				check.Detail = *capability.Version
			} else {
				check.Detail = "installed"
			}
		default:
			// One missing CLI is a warning: a runtime with any usable agent
			// can still take AI work, and a runtime with none can still run
			// approved regression suites.
			check.Status = CheckWarn
			if capability.Error != nil {
				check.Detail = *capability.Error
			}
		}
		checks = append(checks, check)
	}

	summary := Check{Name: "agents"}
	if usable == 0 {
		summary.Status = CheckWarn
		summary.Detail = "no AI CLI is usable on this machine: discovery, planning and analysis will fail, execution of approved test cases will not"
		summary.Hint = "install one of: " + agentInstallHints()
	} else {
		summary.Status = CheckOK
		summary.Detail = fmt.Sprintf("%d of %d AI CLIs usable", usable, len(caps))
	}
	return append(checks, summary)
}

func agentInstallHints() string {
	hints := make([]string, 0, len(agent.Known))
	for _, cli := range agent.Known {
		hints = append(hints, fmt.Sprintf("%s (%s)", cli.Binary, cli.Install))
	}
	return strings.Join(hints, ", ")
}

func doctorWorkspace(cfg Config) Check {
	check := Check{Name: "workspace"}

	root, err := cfg.ResolveWorkspaceRoot()
	if err != nil {
		check.Status, check.Detail = CheckError, err.Error()
		return check
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		check.Status = CheckError
		check.Detail = fmt.Sprintf("cannot create %s: %v", root, err)
		return check
	}
	probe := filepath.Join(root, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		check.Status = CheckError
		check.Detail = fmt.Sprintf("cannot write to %s: %v", root, err)
		return check
	}
	_ = os.Remove(probe)

	check.Status = CheckOK
	check.Detail = fmt.Sprintf("%s is writable", root)
	return check
}

func doctorDaemon(statePath string) Check {
	check := Check{Name: "daemon"}
	if statePath == "" {
		resolved, err := StatePath()
		if err != nil {
			check.Status, check.Detail = CheckWarn, err.Error()
			return check
		}
		statePath = resolved
	}

	state, err := ReadState(statePath)
	if errors.Is(err, ErrNoState) {
		check.Status = CheckWarn
		check.Detail = "the daemon does not appear to have run on this machine"
		check.Hint = "qa-daemon start"
		return check
	}
	if err != nil {
		check.Status, check.Detail = CheckWarn, err.Error()
		return check
	}
	if state.Stale() {
		check.Status = CheckWarn
		check.Detail = fmt.Sprintf("the daemon is not running (last seen %s)", state.UpdatedAt.Format(time.RFC3339))
		check.Hint = "qa-daemon start"
		return check
	}

	check.Status = CheckOK
	check.Detail = fmt.Sprintf("running as pid %d, connection %s", state.PID, state.Connection)
	return check
}

// lineBuffer keeps the first line of a child's output, which is all a version
// probe needs and all a diagnostic should print.
type lineBuffer struct {
	data []byte
}

func (b *lineBuffer) Write(p []byte) (int, error) {
	if len(b.data) < 4096 {
		b.data = append(b.data, p...)
	}
	return len(p), nil
}

func (b *lineBuffer) First() string {
	for line := range strings.SplitSeq(strings.TrimSpace(string(b.data)), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
