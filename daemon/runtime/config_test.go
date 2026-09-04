package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withEnv(t *testing.T, values map[string]string) {
	t.Helper()
	original := Env
	Env = func(key string) string { return values[key] }
	t.Cleanup(func() { Env = original })
}

func TestSaveConfigWritesRestrictedPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")

	if err := SaveConfig(path, Config{ServerURL: "https://qa.test", RuntimeID: "r1", Token: "qart_secret"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode = %04o, want 0600", perm)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Fatalf("config directory mode = %04o, want 0700", perm)
	}
}

func TestLoadConfigRefusesWorldReadableToken(t *testing.T) {
	withEnv(t, nil)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(path, Config{ServerURL: "https://qa.test", RuntimeID: "r1", Token: "qart_secret"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("a config readable by other accounts must be refused: it holds a runtime token")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("error should tell the operator how to fix it: %v", err)
	}
}

func TestLoadConfigReportsMissingPairing(t *testing.T) {
	withEnv(t, nil)
	_, err := LoadConfig(filepath.Join(t.TempDir(), "config.json"))
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("error = %v, want ErrNoConfig", err)
	}
	if !strings.Contains(err.Error(), "qa-daemon pair") {
		t.Fatalf("error should name the command that fixes it: %v", err)
	}
}

func TestLoadConfigAppliesEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(path, Config{ServerURL: "https://file.test", RuntimeID: "file-runtime", Token: "qart_file"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	withEnv(t, map[string]string{
		"QA_DAEMON_SERVER":   "https://env.test",
		"QA_DAEMON_TOKEN":    "qart_env",
		"QA_DAEMON_EXECUTOR": "node /opt/qa-executor.js",
	})

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ServerURL != "https://env.test" || cfg.Token != "qart_env" {
		t.Fatalf("environment did not override the file: %+v", cfg.LogValue())
	}
	if got := cfg.Executor(); len(got) != 2 || got[0] != "node" {
		t.Fatalf("executor command = %v", got)
	}
}

func TestWebSocketURL(t *testing.T) {
	tests := []struct {
		server string
		want   string
		fails  bool
	}{
		{server: "https://qa.example.com", want: "wss://qa.example.com/api/v1/daemon"},
		{server: "http://localhost:8080", want: "ws://localhost:8080/api/v1/daemon"},
		{server: "https://qa.example.com/", want: "wss://qa.example.com/api/v1/daemon"},
		{server: "https://qa.example.com/base", want: "wss://qa.example.com/base/api/v1/daemon"},
		{server: "wss://qa.example.com", want: "wss://qa.example.com/api/v1/daemon"},
		{server: "ftp://qa.example.com", fails: true},
		{server: "not a url at all", fails: true},
		{server: "", fails: true},
	}
	for _, tt := range tests {
		got, err := Config{ServerURL: tt.server}.WebSocketURL()
		if tt.fails {
			if err == nil {
				t.Fatalf("WebSocketURL(%q) = %q, want an error", tt.server, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("WebSocketURL(%q): %v", tt.server, err)
		}
		if got != tt.want {
			t.Fatalf("WebSocketURL(%q) = %q, want %q", tt.server, got, tt.want)
		}
	}
}

// The runtime token is an organization-wide control-plane credential. A log
// line carrying it would put it into every log aggregator the operator runs.
func TestConfigNeverLogsTheToken(t *testing.T) {
	cfg := Config{ServerURL: "https://qa.test", RuntimeID: "r1", Token: "qart_super_secret_value"}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("starting", "config", cfg)

	if strings.Contains(buf.String(), "qart_super_secret_value") {
		t.Fatalf("the runtime token reached the log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "hasToken") {
		t.Fatalf("the log should still say whether a token is present: %s", buf.String())
	}
}

func TestValidateCollectsEveryProblem(t *testing.T) {
	err := Config{}.Validate()
	if err == nil {
		t.Fatal("an empty config must not validate")
	}
	for _, want := range []string{"serverUrl", "runtimeId", "runtimeToken"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %s: %v", want, err)
		}
	}
}

func TestRetentionDefaultsAndOverrides(t *testing.T) {
	defaults := RetentionConfig{}.Retention()
	if defaults.KeepCompleted == 0 || defaults.KeepFailed == 0 {
		t.Fatalf("zero config should fall back to defaults, got %+v", defaults)
	}
	if defaults.KeepFailed <= defaults.KeepCompleted {
		t.Fatal("a failed run's workspace should outlive a successful one: it is the only reproduction")
	}

	custom := RetentionConfig{KeepCompletedHours: 2, KeepFailedHours: 3, MaxRuns: 7}.Retention()
	if custom.KeepCompleted.Hours() != 2 || custom.KeepFailed.Hours() != 3 || custom.MaxRuns != 7 {
		t.Fatalf("custom retention = %+v", custom)
	}
}

func TestConfigPathHonoursEnvironment(t *testing.T) {
	withEnv(t, map[string]string{"QA_DAEMON_CONFIG": "/etc/qa/daemon.json"})
	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if got != "/etc/qa/daemon.json" {
		t.Fatalf("ConfigPath = %q", got)
	}

	withEnv(t, map[string]string{"XDG_CONFIG_HOME": "/home/u/.config"})
	got, err = ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if got != "/home/u/.config/qa-daemon/config.json" {
		t.Fatalf("ConfigPath = %q", got)
	}
}

func TestSaveConfigIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(path, Config{ServerURL: "https://a.test", RuntimeID: "r1", Token: "qart_a"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := SaveConfig(path, Config{ServerURL: "https://b.test", RuntimeID: "r2", Token: "qart_b"}); err != nil {
		t.Fatalf("SaveConfig again: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("the config was left unparsable: %v", err)
	}
	if cfg.RuntimeID != "r2" {
		t.Fatalf("runtimeId = %q, want the newer one", cfg.RuntimeID)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the temp file was left behind")
	}
}
