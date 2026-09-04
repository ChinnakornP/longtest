package security

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ErrOutsideWorkspace is returned when a path would leave the run's directory.
var ErrOutsideWorkspace = errors.New("security: path escapes the run workspace")

// Workspace is the only filesystem surface a run is allowed to touch.
//
// Per ADR-003 an AI CLI exchanges files with the daemon inside a per-run
// directory, and that directory is the agent's blast radius. Every read and
// write the daemon performs on the run's behalf goes through here.
//
// The confinement is not a string comparison on the path. It is [os.Root],
// which resolves each component with openat2-style semantics: a symlink the
// AI CLI planted pointing at /etc, a `..` in the middle of a path, and an
// absolute path all fail at the syscall, and they keep failing if the
// attacker swaps a directory for a symlink between the check and the open.
// A path-prefix check cannot make that last guarantee.
type Workspace struct {
	root *os.Root
	dir  string
}

// OpenWorkspace confines access to dir, which must already exist.
func OpenWorkspace(dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("security: workspace path: %w", err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("security: open workspace: %w", err)
	}
	return &Workspace{root: root, dir: abs}, nil
}

// CreateWorkspace makes dir (and its parents) and confines access to it.
//
// The directory is created 0o700: a run's workspace holds the prompt, the
// model's output and any evidence not yet uploaded, on a machine that belongs
// to the customer and may have other users on it.
func CreateWorkspace(dir string) (*Workspace, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("security: create workspace: %w", err)
	}
	return OpenWorkspace(dir)
}

// Dir is the absolute path of the workspace root. It is what a sandboxed
// child process gets as its working directory and as $HOME.
func (w *Workspace) Dir() string { return w.dir }

// Close releases the root handle.
func (w *Workspace) Close() error { return w.root.Close() }

// checkRel rejects the shapes os.Root would reject anyway, but with an error
// that names the workspace rule rather than an errno.
func checkRel(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty path", ErrOutsideWorkspace)
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return fmt.Errorf("%w: %q is absolute", ErrOutsideWorkspace, name)
	}
	if vol := filepath.VolumeName(name); vol != "" {
		return fmt.Errorf("%w: %q names a volume", ErrOutsideWorkspace, name)
	}
	clean := path.Clean(filepath.ToSlash(name))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%w: %q traverses upward", ErrOutsideWorkspace, name)
	}
	return nil
}

// Open opens a file for reading, relative to the workspace root.
func (w *Workspace) Open(name string) (*os.File, error) {
	if err := checkRel(name); err != nil {
		return nil, err
	}
	f, err := w.root.Open(name)
	if err != nil {
		return nil, wrapEscape(name, err)
	}
	return f, nil
}

// ReadFile reads a file inside the workspace.
func (w *Workspace) ReadFile(name string) ([]byte, error) {
	f, err := w.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	return io.ReadAll(f)
}

// WriteFile writes a file inside the workspace, creating parent directories
// that are themselves inside the workspace.
//
// Files are 0o600 for the reason CreateWorkspace uses 0o700.
func (w *Workspace) WriteFile(name string, data []byte) error {
	if err := checkRel(name); err != nil {
		return err
	}
	if dir := path.Dir(filepath.ToSlash(name)); dir != "." {
		if err := w.MkdirAll(dir); err != nil {
			return err
		}
	}
	f, err := w.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return wrapEscape(name, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("security: write %q: %w", name, err)
	}
	return f.Close()
}

// MkdirAll creates a directory tree inside the workspace.
func (w *Workspace) MkdirAll(name string) error {
	if err := checkRel(name); err != nil {
		return err
	}
	parts := strings.Split(path.Clean(filepath.ToSlash(name)), "/")
	cur := ""
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		cur = path.Join(cur, p)
		if err := w.root.Mkdir(cur, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return wrapEscape(cur, err)
		}
	}
	return nil
}

// Walk visits every regular file in the workspace, yielding paths relative to
// the root. It is what the pre-upload credential scan and the end-of-run
// scrubber check iterate over.
func (w *Workspace) Walk(fn func(rel string, info fs.FileInfo) error) error {
	return filepath.Walk(w.dir, func(p string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(w.dir, p)
		if relErr != nil {
			return relErr
		}
		return fn(filepath.ToSlash(rel), info)
	})
}

func wrapEscape(name string, err error) error {
	// os.Root reports a traversal or symlink escape as EPERM/ENOTDIR/EXDEV
	// depending on the platform and the shape of the attempt. Callers care
	// that it was refused, not which errno said so.
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrInvalid) {
		return fmt.Errorf("%w: %q: %w", ErrOutsideWorkspace, name, err)
	}
	return fmt.Errorf("security: %q: %w", name, err)
}
