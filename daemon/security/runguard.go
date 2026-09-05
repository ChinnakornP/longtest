package security

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"sort"
)

// RunGuard ties one run's confinement together: its workspace, its scrubber,
// its credential fixtures and its frame nonce.
//
// It exists so there is exactly one answer to "where does a credential get
// removed?". Every path out of a run — a prompt, a workspace file, the run
// log, a `run.event` frame, an artifact body — goes through a method here.
// A future call site that invents its own path is a bug that shows up in
// [RunGuard.Verify] rather than a silent leak.
//
// The daemon builds one per run and passes it down; nothing below it needs to
// know a credential exists.
type RunGuard struct {
	Workspace *Workspace
	Scrubber  *Scrubber
	Fixtures  *FixtureStore
	// Nonce frames every untrusted block in this run's prompts.
	Nonce string
}

// NewRunGuard creates the workspace, registers the fixture credentials with a
// fresh scrubber, and mints the run's frame nonce.
//
// The registration happens here, before the caller can produce any output at
// all. A scrubber that learns a credential after the first log line is not a
// control, so there is no constructor that lets a run start without it.
func NewRunGuard(dir string, fixtures *FixtureStore) (*RunGuard, error) {
	ws, err := CreateWorkspace(dir)
	if err != nil {
		return nil, err
	}
	sc := NewScrubber()
	if fixtures != nil {
		if err := fixtures.RegisterWith(sc); err != nil {
			_ = ws.Close()
			return nil, err
		}
	}
	return &RunGuard{Workspace: ws, Scrubber: sc, Fixtures: fixtures, Nonce: NewNonce()}, nil
}

// Close releases the workspace handle.
func (r *RunGuard) Close() error { return r.Workspace.Close() }

// WriteFile scrubs and writes a workspace file. This is how a prompt, an
// application map or a test plan reaches disk.
func (r *RunGuard) WriteFile(name string, data []byte) error {
	return r.Workspace.WriteFile(name, r.Scrubber.Bytes(data))
}

// WriteJSON scrubs and writes a JSON document, walking the structure rather
// than the bytes so an escaped credential is caught too.
func (r *RunGuard) WriteJSON(name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("security: encode %s: %w", name, err)
	}
	clean, err := r.Scrubber.JSON(raw)
	if err != nil {
		return err
	}
	return r.Workspace.WriteFile(name, clean)
}

// LogWriter wraps a log sink. The AI CLI's stdout and stderr go through it,
// which is the one place a credential most reliably shows up: a CLI echoing
// the form it filled, or a stack trace from the app under test.
func (r *RunGuard) LogWriter(w io.Writer) io.WriteCloser {
	return r.Scrubber.Writer(w)
}

// Event scrubs a `run.event` payload on its way to the backend.
func (r *RunGuard) Event(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("security: encode event: %w", err)
	}
	clean, err := r.Scrubber.JSON(raw)
	if err != nil {
		return nil, err
	}
	return clean, nil
}

// Artifact scrubs an artifact body before it is uploaded.
//
// It applies to the text-shaped artifacts — network log, console log, HTML
// snapshot. A screenshot is pixels and a trace is a zip: neither can be
// scrubbed by substring replacement, and pretending otherwise would be worse
// than saying so. See the residual-risk section of docs/SECURITY.md.
func (r *RunGuard) Artifact(body []byte) []byte {
	return r.Scrubber.Bytes(body)
}

// Leak is one place a credential was found by [RunGuard.Verify].
type Leak struct {
	// Path is relative to the workspace root.
	Path string
	// Bytes is the file's size, for triage. The offending content is
	// deliberately not captured.
	Bytes int64
}

// Verify re-scans the whole workspace for registered secrets.
//
// It is a backstop, not the control: everything above already scrubs. Verify
// catches the case the control cannot — a file some future code wrote with
// os.WriteFile instead of [RunGuard.WriteFile]. The daemon runs it before
// uploading anything and fails the run if it returns non-empty, because a
// workspace that leaked once has no reason to be trusted for the rest.
func (r *RunGuard) Verify() ([]Leak, error) {
	var leaks []Leak
	err := r.Workspace.Walk(func(rel string, info fs.FileInfo) error {
		// Skip anything too large to hold in memory; those are traces and
		// videos, which Artifact already declines to scrub.
		const maxScan = 32 << 20
		if info.Size() > maxScan {
			return nil
		}
		data, err := r.Workspace.ReadFile(rel)
		if err != nil {
			return err
		}
		if r.Scrubber.Contains(string(data)) {
			leaks = append(leaks, Leak{Path: rel, Bytes: info.Size()})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(leaks, func(i, j int) bool { return leaks[i].Path < leaks[j].Path })
	return leaks, nil
}
