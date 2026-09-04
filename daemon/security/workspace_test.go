package security_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/security"
)

func TestWorkspaceRefusesToEscape(t *testing.T) {
	ws, err := security.CreateWorkspace(filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer ws.Close() //nolint:errcheck // test cleanup

	for _, name := range []string{
		"../outside.txt",
		"a/../../outside.txt",
		"/etc/passwd",
		"",
		"..",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ws.WriteFile(name, []byte("x")); err == nil {
				t.Fatalf("write to %q was allowed", name)
			}
			if _, err := ws.ReadFile(name); err == nil {
				t.Fatalf("read of %q was allowed", name)
			}
		})
	}
}

// The interesting case: the path is relative and contains no "..", but a
// directory along it is a symlink out. A prefix check on the cleaned path
// accepts this; os.Root does not.
func TestWorkspaceRefusesASymlinkedDirectory(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "run")
	ws, err := security.CreateWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close() //nolint:errcheck // test cleanup

	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if data, err := ws.ReadFile("escape/secret.txt"); err == nil {
		t.Fatalf("read through a symlinked directory succeeded: %q", data)
	}
	if err := ws.WriteFile("escape/planted.txt", []byte("x")); err == nil {
		t.Fatal("write through a symlinked directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a file was created outside the workspace")
	}
}

func TestWorkspaceRoundTripsAndCreatesParents(t *testing.T) {
	ws, err := security.CreateWorkspace(filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close() //nolint:errcheck // test cleanup

	if err := ws.WriteFile("planning/nested/out.json", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ws.ReadFile("planning/nested/out.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("round trip returned %q", got)
	}

	// 0600 / 0700: the workspace holds a prompt and un-uploaded evidence on a
	// machine that may have other users.
	info, err := os.Stat(filepath.Join(ws.Dir(), "planning/nested/out.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("workspace file mode is %o, want 600", perm)
	}
	dirInfo, err := os.Stat(filepath.Join(ws.Dir(), "planning"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("workspace directory mode is %o, want 700", perm)
	}
}

func TestWorkspaceWalkYieldsRelativePaths(t *testing.T) {
	ws, err := security.CreateWorkspace(filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close() //nolint:errcheck // test cleanup

	for _, n := range []string{"a.txt", "sub/b.txt", "sub/deep/c.txt"} {
		if err := ws.WriteFile(n, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	if err := ws.Walk(func(rel string, _ os.FileInfo) error {
		if strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
			t.Fatalf("Walk yielded a non-relative path %q", rel)
		}
		seen = append(seen, rel)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 files, saw %v", seen)
	}
}

func TestOutsideWorkspaceErrorIsIdentifiable(t *testing.T) {
	ws, err := security.CreateWorkspace(filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close() //nolint:errcheck // test cleanup

	err = ws.WriteFile("../x", []byte("x"))
	if !errors.Is(err, security.ErrOutsideWorkspace) {
		t.Fatalf("expected ErrOutsideWorkspace, got %v", err)
	}
}
