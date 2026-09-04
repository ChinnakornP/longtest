package browser

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeCache(t *testing.T, goos string, builds ...string) string {
	t.Helper()
	cache := t.TempDir()
	// One layout per build is enough: Detect only has to find the first
	// candidate that exists.
	layout := executableNames(goos)[0]
	for _, build := range builds {
		full := filepath.Join(cache, build, layout)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write: %v", err)
		}
	}
	return cache
}

func TestDetectPrefersNewestBuild(t *testing.T) {
	// Sorted by name, chromium-999 would win. Revisions are numbers.
	cache := fakeCache(t, "linux", "chromium-999", "chromium-1100", "chromium-1187")

	got, err := Detect(Options{CacheDir: cache, GOOS: "linux", Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Build != "chromium-1187" {
		t.Fatalf("build = %q, want chromium-1187", got.Build)
	}
	if got.Source != "playwright-cache" {
		t.Fatalf("source = %q", got.Source)
	}
}

func TestDetectHonoursExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chrome")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write: %v", err)
	}

	got, err := Detect(Options{Env: func(key string) string {
		if key == "QA_DAEMON_CHROMIUM" {
			return path
		}
		return ""
	}})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.ExecutablePath != path || got.Source != "env" {
		t.Fatalf("got %+v", got)
	}
}

func TestDetectReportsWhyItFailed(t *testing.T) {
	empty := t.TempDir()

	_, err := Detect(Options{CacheDir: empty, GOOS: "linux", Env: func(string) string { return "" }})
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("error = %v, want ErrNotInstalled", err)
	}
	// doctor prints this error verbatim, so it has to name the directory it
	// looked in and the command that fixes it.
	if !strings.Contains(err.Error(), empty) || !strings.Contains(err.Error(), InstallHint) {
		t.Fatalf("error is not actionable: %v", err)
	}
}

func TestDetectReportsBuildWithoutBinary(t *testing.T) {
	cache := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cache, "chromium-1187"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := Detect(Options{CacheDir: cache, GOOS: "linux", Env: func(string) string { return "" }})
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("error = %v, want ErrNotInstalled", err)
	}
	if !strings.Contains(err.Error(), "chromium-1187") {
		t.Fatalf("error should name the broken build: %v", err)
	}
}

func TestDetectRejectsUnusableOverride(t *testing.T) {
	dir := t.TempDir()
	_, err := Detect(Options{Env: func(key string) string {
		if key == "QA_DAEMON_CHROMIUM" {
			return dir // a directory, not a binary
		}
		return ""
	}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "QA_DAEMON_CHROMIUM") {
		t.Fatalf("error should name the override: %v", err)
	}
}

// Playwright renamed the browser directory between releases; both layouts have
// to resolve, or a machine with a working browser reads as broken.
func TestDetectHandlesEveryKnownLayout(t *testing.T) {
	for _, layout := range executableNames("linux") {
		t.Run(layout, func(t *testing.T) {
			cache := t.TempDir()
			full := filepath.Join(cache, "chromium-1187", layout)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(full, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // test fixture
				t.Fatalf("write: %v", err)
			}

			got, err := Detect(Options{CacheDir: cache, GOOS: "linux", Env: func(string) string { return "" }})
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got.ExecutablePath != full {
				t.Fatalf("ExecutablePath = %q, want %q", got.ExecutablePath, full)
			}
		})
	}
}

func TestDefaultCacheDir(t *testing.T) {
	tests := []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{"linux xdg", "linux", map[string]string{"XDG_CACHE_HOME": "/x/cache", "HOME": "/home/u"}, "/x/cache/ms-playwright"},
		{"linux home", "linux", map[string]string{"HOME": "/home/u"}, "/home/u/.cache/ms-playwright"},
		{"darwin", "darwin", map[string]string{"HOME": "/Users/u"}, "/Users/u/Library/Caches/ms-playwright"},
		{"windows", "windows", map[string]string{"LOCALAPPDATA": `C:\Users\u\AppData\Local`}, filepath.Join(`C:\Users\u\AppData\Local`, "ms-playwright")},
		{"explicit", "linux", map[string]string{"PLAYWRIGHT_BROWSERS_PATH": "/opt/pw"}, "/opt/pw"},
		{"explicit zero means default", "linux", map[string]string{"PLAYWRIGHT_BROWSERS_PATH": "0", "HOME": "/home/u"}, "/home/u/.cache/ms-playwright"},
		{"nothing", "linux", map[string]string{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultCacheDir(Options{GOOS: tt.goos, Env: func(key string) string { return tt.env[key] }})
			if got != tt.want {
				t.Fatalf("defaultCacheDir = %q, want %q", got, tt.want)
			}
		})
	}
}
