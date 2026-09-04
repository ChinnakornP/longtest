// Package runtime is the daemon control loop: it dials the backend over a
// single outbound WebSocket, reports what this machine can do, accepts run
// assignments and drives them to a result.
//
// Everything here assumes ADR-002: the daemon opens no inbound port, so the
// backend can never reach in. That shapes the whole package — reconnect is a
// first-class state rather than an error path, `qa-daemon status` reads a
// state file instead of querying a local API, and evidence goes straight to
// object storage rather than back up this connection.
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChinnakornP/longtest/daemon/executor"
	"github.com/ChinnakornP/longtest/daemon/workspace"
)

// Version is the daemon build reported in the hello frame and by
// `qa-daemon version`.
const Version = "0.1.0"

// DaemonPath is the control-plane endpoint. It is a constant rather than
// config: a daemon pointed at a different path is talking to something that is
// not this backend.
const DaemonPath = "/api/v1/daemon"

// configPerm is the only mode a config file may have. It holds a runtime
// token, which is a bearer credential for this organization's control plane.
const configPerm os.FileMode = 0o600

// Config is what `qa-daemon pair` writes and `qa-daemon start` reads.
//
// The token is the only secret in it, and it never leaves this struct: the
// logger sees Config through LogValue, which omits it.
type Config struct {
	ServerURL   string `json:"serverUrl"`
	RuntimeID   string `json:"runtimeId"`
	RuntimeName string `json:"runtimeName,omitempty"`
	OrgID       string `json:"orgId,omitempty"`
	Token       string `json:"runtimeToken"`

	// WorkspaceRoot is where per-run workspaces are created. Empty means the
	// platform default under the user's data directory.
	WorkspaceRoot string `json:"workspaceRoot,omitempty"`
	// ExecutorCommand is the sidecar argv. Empty means executor.DefaultCommand.
	ExecutorCommand []string `json:"executorCommand,omitempty"`
	// LogPath is the structured log file. Empty means the platform default.
	LogPath string `json:"logPath,omitempty"`

	Retention RetentionConfig `json:"retention,omitzero"`
}

// RetentionConfig is the workspace retention policy in units an operator can
// edit by hand.
type RetentionConfig struct {
	KeepCompletedHours int `json:"keepCompletedHours,omitempty"`
	KeepFailedHours    int `json:"keepFailedHours,omitempty"`
	MaxRuns            int `json:"maxRuns,omitempty"`
}

// Retention converts the configured policy, falling back to the defaults for
// any value left at zero.
func (r RetentionConfig) Retention() workspace.Retention {
	out := workspace.DefaultRetention()
	if r.KeepCompletedHours > 0 {
		out.KeepCompleted = time.Duration(r.KeepCompletedHours) * time.Hour
	}
	if r.KeepFailedHours > 0 {
		out.KeepFailed = time.Duration(r.KeepFailedHours) * time.Hour
	}
	if r.MaxRuns > 0 {
		out.MaxRuns = r.MaxRuns
	}
	return out
}

// LogValue makes Config safe to hand a logger: the runtime token is a bearer
// credential and must never reach a log file, a terminal or a bug report.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("serverUrl", c.ServerURL),
		slog.String("runtimeId", c.RuntimeID),
		slog.String("runtimeName", c.RuntimeName),
		slog.String("orgId", c.OrgID),
		slog.Bool("hasToken", c.Token != ""),
	)
}

// Validate checks that the config could plausibly connect.
func (c Config) Validate() error {
	var problems []error
	if strings.TrimSpace(c.ServerURL) == "" {
		problems = append(problems, errors.New("serverUrl is empty"))
	} else if _, err := c.WebSocketURL(); err != nil {
		problems = append(problems, err)
	}
	if strings.TrimSpace(c.RuntimeID) == "" {
		problems = append(problems, errors.New("runtimeId is empty"))
	}
	if strings.TrimSpace(c.Token) == "" {
		problems = append(problems, errors.New("runtimeToken is empty: run `qa-daemon pair` first"))
	}
	return errors.Join(problems...)
}

// WebSocketURL is the control-plane URL derived from the server URL. http
// becomes ws and https becomes wss, so an operator configures one address and
// cannot get the two out of step.
func (c Config) WebSocketURL() (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(c.ServerURL))
	if err != nil {
		return "", fmt.Errorf("serverUrl %q is not a URL: %w", c.ServerURL, err)
	}
	switch parsed.Scheme {
	case "http", "ws":
		parsed.Scheme = "ws"
	case "https", "wss":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("serverUrl scheme %q is not http(s)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("serverUrl %q has no host", c.ServerURL)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + DaemonPath
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// APIURL joins a path onto the configured server, for the few plain HTTP calls
// the daemon makes (pairing, and doctor's reachability probe).
func (c Config) APIURL(path string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(c.ServerURL))
	if err != nil {
		return "", fmt.Errorf("serverUrl %q is not a URL: %w", c.ServerURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("serverUrl scheme %q is not http(s)", parsed.Scheme)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + path
	return parsed.String(), nil
}

// Executor is the sidecar argv this config asks for.
func (c Config) Executor() []string {
	if len(c.ExecutorCommand) == 0 {
		return executor.DefaultCommand
	}
	return c.ExecutorCommand
}

// Env reads the environment. It is a variable so tests can supply their own
// without mutating the process.
var Env = os.Getenv

// ConfigPath returns the config file location: $QA_DAEMON_CONFIG, else the
// XDG config directory.
func ConfigPath() (string, error) {
	if explicit := strings.TrimSpace(Env("QA_DAEMON_CONFIG")); explicit != "" {
		return explicit, nil
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func configDir() (string, error) {
	if xdg := strings.TrimSpace(Env("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "qa-daemon"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("runtime: locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "qa-daemon"), nil
}

// StateDir is where the daemon keeps its status file and log.
func StateDir() (string, error) {
	if xdg := strings.TrimSpace(Env("XDG_STATE_HOME")); xdg != "" {
		return filepath.Join(xdg, "qa-daemon"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("runtime: locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "qa-daemon"), nil
}

// DefaultWorkspaceRoot is where per-run workspaces live when config names no
// other place.
func DefaultWorkspaceRoot() (string, error) {
	if xdg := strings.TrimSpace(Env("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "qa-daemon", "workspaces"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("runtime: locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "qa-daemon", "workspaces"), nil
}

// ErrNoConfig means this machine has not been paired yet.
var ErrNoConfig = errors.New("runtime: no daemon config; run `qa-daemon pair --code <code> --server <url>`")

// LoadConfig reads and validates the config file, then applies environment
// overrides.
//
// A config file that is readable by other accounts on the machine is refused
// rather than repaired: it holds a token that grants control-plane access to
// the whole organization, and silently chmod-ing it would hide the fact that
// something already read it.
func LoadConfig(path string) (Config, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Config{}, fmt.Errorf("%w (looked in %s)", ErrNoConfig, path)
	case err != nil:
		return Config{}, fmt.Errorf("runtime: stat config: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return Config{}, fmt.Errorf("runtime: %s is mode %04o and holds a runtime token; run: chmod 600 %s", path, perm, path)
	}

	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own config
	if err != nil {
		return Config{}, fmt.Errorf("runtime: read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("runtime: parse %s: %w", path, err)
	}

	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("runtime: %s is incomplete: %w", path, err)
	}
	return cfg, nil
}

// applyEnv lets a container override what a file would otherwise hold. This is
// how a runtime is provisioned without writing a token to a volume.
func (c *Config) applyEnv() {
	if v := strings.TrimSpace(Env("QA_DAEMON_SERVER")); v != "" {
		c.ServerURL = v
	}
	if v := strings.TrimSpace(Env("QA_DAEMON_TOKEN")); v != "" {
		c.Token = v
	}
	if v := strings.TrimSpace(Env("QA_DAEMON_RUNTIME_ID")); v != "" {
		c.RuntimeID = v
	}
	if v := strings.TrimSpace(Env("QA_DAEMON_WORKSPACE_ROOT")); v != "" {
		c.WorkspaceRoot = v
	}
	if v := strings.TrimSpace(Env("QA_DAEMON_EXECUTOR")); v != "" {
		c.ExecutorCommand = strings.Fields(v)
	}
}

// SaveConfig writes the config 0600, creating its directory 0700.
//
// The write is atomic: a daemon reading the file while pair rewrites it must
// see either the old config or the new one, never a truncated file with half a
// token in it.
func SaveConfig(path string, cfg Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("runtime: create config directory: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime: encode config: %w", err)
	}
	data = append(data, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, configPerm); err != nil {
		return fmt.Errorf("runtime: write config: %w", err)
	}
	// WriteFile applies the mode only when it creates the file, so an existing
	// temp file with a looser mode is corrected explicitly.
	if err := os.Chmod(tmp, configPerm); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("runtime: secure config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("runtime: replace config: %w", err)
	}
	return nil
}

// ResolveWorkspaceRoot returns the configured root or the platform default.
func (c Config) ResolveWorkspaceRoot() (string, error) {
	if root := strings.TrimSpace(c.WorkspaceRoot); root != "" {
		return root, nil
	}
	return DefaultWorkspaceRoot()
}

// ResolveLogPath returns the configured log file or the platform default.
func (c Config) ResolveLogPath() (string, error) {
	if path := strings.TrimSpace(c.LogPath); path != "" {
		return path, nil
	}
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.log"), nil
}
