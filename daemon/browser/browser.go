// Package browser finds the Chromium build the executor drives.
//
// The daemon does not launch Chromium itself — the Node executor owns the
// browser (ADR-001). What the daemon needs is an answer to "can a run start at
// all?", because a missing browser is the single most common reason a fresh
// runtime cannot execute anything, and discovering it when a run is already
// assigned wastes the run and reports a confusing failure.
package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrNotInstalled means no Playwright Chromium build was found. It is a
// distinct error because the fix is a specific command, which doctor prints.
var ErrNotInstalled = errors.New("browser: playwright chromium is not installed")

// InstallHint is what a human has to run to fix ErrNotInstalled.
const InstallHint = "npx playwright install chromium"

// Chromium is a browser build found on this machine.
type Chromium struct {
	// ExecutablePath is the Chromium binary Playwright would launch.
	ExecutablePath string
	// Build is the Playwright build directory name, e.g. "chromium-1187".
	Build string
	// Source says where the path came from: "env" or "playwright-cache".
	Source string
}

// Options customise detection, mainly so tests do not depend on what is
// installed on the machine running them.
type Options struct {
	// CacheDir overrides the Playwright browsers directory.
	CacheDir string
	// Env looks up environment variables; nil means os.Getenv.
	Env func(string) string
	// GOOS overrides the platform layout; empty means runtime.GOOS.
	GOOS string
}

func (o Options) env(key string) string {
	if o.Env != nil {
		return o.Env(key)
	}
	return os.Getenv(key)
}

func (o Options) goos() string {
	if o.GOOS != "" {
		return o.GOOS
	}
	return runtime.GOOS
}

// Detect finds the Chromium build Playwright would use.
//
// It looks in the two places Playwright itself looks, in the same order: an
// explicit executable override, then the browsers cache. It deliberately does
// not shell out to `npx playwright` to ask — that costs a Node startup on
// every heartbeat-adjacent code path, and fails for a reason ("npx not found")
// that has nothing to do with the question.
func Detect(opts Options) (Chromium, error) {
	if override := strings.TrimSpace(opts.env("QA_DAEMON_CHROMIUM")); override != "" {
		if err := executable(override); err != nil {
			return Chromium{}, fmt.Errorf("browser: QA_DAEMON_CHROMIUM=%s: %w", override, err)
		}
		return Chromium{ExecutablePath: override, Build: filepath.Base(filepath.Dir(override)), Source: "env"}, nil
	}

	cacheDir := opts.CacheDir
	if cacheDir == "" {
		cacheDir = defaultCacheDir(opts)
	}
	if cacheDir == "" {
		return Chromium{}, fmt.Errorf("%w: no browsers directory to look in", ErrNotInstalled)
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return Chromium{}, fmt.Errorf("%w: %s is not readable: %w", ErrNotInstalled, cacheDir, err)
	}

	builds := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "chromium-") {
			builds = append(builds, entry.Name())
		}
	}
	if len(builds) == 0 {
		return Chromium{}, fmt.Errorf("%w: no chromium-* build under %s (run: %s)", ErrNotInstalled, cacheDir, InstallHint)
	}
	// Newest build last. Sorting by name would put chromium-999 after
	// chromium-1234, so the revision is compared as the number it is.
	slices.SortFunc(builds, func(a, b string) int { return revisionOf(a) - revisionOf(b) })

	var missing []string
	for i := len(builds) - 1; i >= 0; i-- {
		build := builds[i]
		for _, rel := range executableNames(opts.goos()) {
			candidate := filepath.Join(cacheDir, build, rel)
			if err := executable(candidate); err == nil {
				return Chromium{ExecutablePath: candidate, Build: build, Source: "playwright-cache"}, nil
			}
		}
		missing = append(missing, build)
	}
	return Chromium{}, fmt.Errorf("%w: %s contains %s but no runnable binary in it (run: %s)",
		ErrNotInstalled, cacheDir, strings.Join(missing, ", "), InstallHint)
}

// Version asks the browser what it is. It is only used by doctor, where the
// extra process is worth the certainty that the binary actually runs — a
// Chromium missing a shared library is present on disk and still unusable.
func Version(ctx context.Context, c Chromium) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	//nolint:gosec // G204: the path came from Detect, not from user input.
	out, err := exec.CommandContext(ctx, c.ExecutablePath, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("browser: %s --version: %w", c.ExecutablePath, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// defaultCacheDir mirrors Playwright's own resolution order.
func defaultCacheDir(opts Options) string {
	if explicit := strings.TrimSpace(opts.env("PLAYWRIGHT_BROWSERS_PATH")); explicit != "" && explicit != "0" {
		return explicit
	}
	home := opts.env("HOME")
	switch opts.goos() {
	case "darwin":
		if home == "" {
			return ""
		}
		return filepath.Join(home, "Library", "Caches", "ms-playwright")
	case "windows":
		if local := opts.env("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "ms-playwright")
		}
		return ""
	default:
		if cache := strings.TrimSpace(opts.env("XDG_CACHE_HOME")); cache != "" {
			return filepath.Join(cache, "ms-playwright")
		}
		if home == "" {
			return ""
		}
		return filepath.Join(home, ".cache", "ms-playwright")
	}
}

// revisionOf parses the number out of "chromium-1234". A directory that does
// not end in a number sorts first, which keeps it from being preferred over a
// real build.
func revisionOf(build string) int {
	_, digits, ok := strings.Cut(build, "-")
	if !ok {
		return 0
	}
	revision, err := strconv.Atoi(digits)
	if err != nil {
		return 0
	}
	return revision
}

// executableNames lists the layouts Playwright has shipped, newest first.
// The directory was renamed from chrome-linux to chrome-linux64 (and
// chrome-win to chrome-win64), and a daemon that only knows the old name
// reports a machine with a perfectly good browser as broken.
func executableNames(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			filepath.Join("chrome-mac", "Chromium.app", "Contents", "MacOS", "Chromium"),
			filepath.Join("chrome-mac-arm64", "Chromium.app", "Contents", "MacOS", "Chromium"),
		}
	case "windows":
		return []string{
			filepath.Join("chrome-win64", "chrome.exe"),
			filepath.Join("chrome-win", "chrome.exe"),
		}
	default:
		return []string{
			filepath.Join("chrome-linux64", "chrome"),
			filepath.Join("chrome-linux", "chrome"),
			filepath.Join("chrome-linux", "headless_shell"),
		}
	}
}

func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("not found: %w", err)
	}
	if info.IsDir() {
		return errors.New("is a directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return errors.New("is not executable")
	}
	return nil
}
