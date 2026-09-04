package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestManager(t *testing.T, retention Retention, now func() time.Time) *Manager {
	t.Helper()
	opts := []Option{}
	if now != nil {
		opts = append(opts, WithClock(now))
	}
	m, err := NewManager(filepath.Join(t.TempDir(), "workspaces"), retention, opts...)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestCreateMakesEveryPhase(t *testing.T) {
	m := newTestManager(t, DefaultRetention(), nil)

	ws, err := m.Create("project-1", "run-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, phase := range Phases {
		dir, err := ws.PhaseDir(phase)
		if err != nil {
			t.Fatalf("PhaseDir(%s): %v", phase, err)
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
		if perm := info.Mode().Perm(); perm != dirPerm {
			t.Fatalf("%s permissions = %o, want %o", dir, perm, dirPerm)
		}
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	m := newTestManager(t, DefaultRetention(), nil)

	first, err := m.Create("project-1", "run-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := first.WriteFile(PhaseDiscovery, "keep.json", []byte(`{}`)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	second, err := m.Create("project-1", "run-1")
	if err != nil {
		t.Fatalf("Create again: %v", err)
	}
	path, err := second.Path(PhaseDiscovery, "keep.json")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("re-creating the workspace lost %s: %v", path, err)
	}
}

// Runs must not see each other's files: that is the whole point of one
// directory per run, and it is an acceptance criterion of LONG-11.
func TestRunsAreIsolated(t *testing.T) {
	m := newTestManager(t, DefaultRetention(), nil)

	one, err := m.Create("project-1", "run-1")
	if err != nil {
		t.Fatalf("Create run-1: %v", err)
	}
	two, err := m.Create("project-1", "run-2")
	if err != nil {
		t.Fatalf("Create run-2: %v", err)
	}
	if one.Dir() == two.Dir() {
		t.Fatal("two runs share a directory")
	}

	if _, err := one.WriteFile(PhaseExecution, "secret.json", []byte(`{"token":"x"}`)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	twoDir, err := two.PhaseDir(PhaseExecution)
	if err != nil {
		t.Fatalf("PhaseDir: %v", err)
	}
	entries, err := os.ReadDir(twoDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("run-2 sees %d files from run-1", len(entries))
	}

	// And removing one run leaves the other intact.
	if err := one.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(two.Dir()); err != nil {
		t.Fatalf("removing run-1 damaged run-2: %v", err)
	}
}

func TestPathRejectsEscapes(t *testing.T) {
	m := newTestManager(t, DefaultRetention(), nil)
	ws, err := m.Create("project-1", "run-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tests := []struct {
		name  string
		parts []string
	}{
		{"traversal", []string{".."}},
		{"traversal deep", []string{"..", "..", "etc", "passwd"}},
		{"separator", []string{"../../etc/passwd"}},
		{"backslash", []string{`..\..\secrets`}},
		{"absolute", []string{"/etc/passwd"}},
		{"empty", []string{""}},
		{"nul", []string{"trace\x00.zip"}},
		{"dotfile", []string{".workspace.json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := ws.Path(PhaseExecution, tt.parts...); err == nil {
				t.Fatalf("expected an error, got %q", got)
			}
		})
	}
}

func TestPathRejectsUnknownPhase(t *testing.T) {
	m := newTestManager(t, DefaultRetention(), nil)
	ws, err := m.Create("project-1", "run-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := ws.Path("../../elsewhere", "x.json"); err == nil {
		t.Fatal("expected an unknown-phase error")
	}
}

func TestCreateRejectsUnsafeIdentifiers(t *testing.T) {
	m := newTestManager(t, DefaultRetention(), nil)

	tests := []struct{ project, run string }{
		{"", "run-1"},
		{"project-1", ""},
		{"..", "run-1"},
		{"project-1", ".."},
		{"a/b", "run-1"},
		{"project-1", "a/b"},
		{strings.Repeat("p", 500), "run-1"},
	}
	for _, tt := range tests {
		if _, err := m.Create(tt.project, tt.run); err == nil {
			t.Fatalf("Create(%q, %q) should have failed", tt.project, tt.run)
		}
	}
}

func TestOpenMissingWorkspace(t *testing.T) {
	m := newTestManager(t, DefaultRetention(), nil)
	if _, err := m.Open("project-1", "run-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open error = %v, want ErrNotFound", err)
	}
}

func TestSweepKeepsRunningWorkspaces(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	m := newTestManager(t, Retention{KeepCompleted: time.Hour, KeepFailed: time.Hour}, func() time.Time { return now })

	running, err := m.Create("project-1", "run-running")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Fast-forward well past both TTLs.
	now = now.Add(72 * time.Hour)

	swept, err := m.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(swept) != 0 {
		t.Fatalf("swept %d running workspaces", len(swept))
	}
	if _, err := os.Stat(running.Dir()); err != nil {
		t.Fatalf("running workspace was deleted: %v", err)
	}
}

func TestSweepAppliesOutcomeSpecificTTL(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	m := newTestManager(t, Retention{KeepCompleted: time.Hour, KeepFailed: 48 * time.Hour}, func() time.Time { return now })

	completed, err := m.Create("project-1", "run-completed")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	failed, err := m.Create("project-1", "run-failed")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Finish(completed, OutcomeCompleted); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := m.Finish(failed, OutcomeFailed); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	now = now.Add(2 * time.Hour)

	swept, err := m.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(swept) != 1 || swept[0].RunID != "run-completed" {
		t.Fatalf("swept = %+v, want only run-completed", swept)
	}
	if _, err := os.Stat(completed.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed workspace survived its TTL: %v", err)
	}
	if _, err := os.Stat(failed.Dir()); err != nil {
		t.Fatalf("failed workspace was swept too early: %v", err)
	}
}

func TestSweepEnforcesMaxRuns(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	m := newTestManager(t, Retention{KeepCompleted: 30 * 24 * time.Hour, KeepFailed: 30 * 24 * time.Hour, MaxRuns: 2},
		func() time.Time { return now })

	for _, run := range []string{"run-1", "run-2", "run-3"} {
		ws, err := m.Create("project-1", run)
		if err != nil {
			t.Fatalf("Create %s: %v", run, err)
		}
		if err := m.Finish(ws, OutcomeCompleted); err != nil {
			t.Fatalf("Finish %s: %v", run, err)
		}
		now = now.Add(time.Minute)
	}

	swept, err := m.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(swept) != 1 || swept[0].RunID != "run-1" {
		t.Fatalf("swept = %+v, want the oldest run only", swept)
	}
}

func TestSweepPrunesEmptyProjectDirs(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	m := newTestManager(t, Retention{KeepCompleted: time.Minute, KeepFailed: time.Minute}, func() time.Time { return now })

	ws, err := m.Create("project-1", "run-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Finish(ws, OutcomeCompleted); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	now = now.Add(time.Hour)

	if _, err := m.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Root(), "project-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty project directory survived: %v", err)
	}
}

func TestWriteFilePermissions(t *testing.T) {
	m := newTestManager(t, DefaultRetention(), nil)
	ws, err := m.Create("project-1", "run-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	path, err := ws.WriteFile(PhasePlanning, "out.json", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Fatalf("permissions = %o, want %o", perm, filePerm)
	}
}
