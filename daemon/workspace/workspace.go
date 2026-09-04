// Package workspace creates and tears down the per-run directory that an AI
// CLI and the executor are confined to.
//
// The workspace is the agent's blast radius (ADR-003): everything an agent
// reads and writes lives under /workspaces/{projectId}/{runId}/{phase}/, and
// nothing outside it is reachable through a path the daemon builds. Two runs
// never share a directory, so one run cannot read another's prompt, evidence
// or output — including two runs of the same project on the same machine.
package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Phase is one stage of a run. Each gets its own directory so that a failed
// run can be read as a sequence of inputs and outputs rather than one flat
// pile of files.
type Phase string

// The phases in the order a full run walks them.
const (
	PhaseDiscovery Phase = "discovery"
	PhasePlanning  Phase = "planning"
	PhaseExecution Phase = "execution"
	PhaseAnalysis  Phase = "analysis"
)

// Phases is every phase directory a workspace is created with.
var Phases = []Phase{PhaseDiscovery, PhasePlanning, PhaseExecution, PhaseAnalysis}

// IsValid reports whether p is a phase this build knows about. An unknown
// phase is an error rather than a directory quietly created next to the real
// ones.
func (p Phase) IsValid() bool {
	for _, candidate := range Phases {
		if p == candidate {
			return true
		}
	}
	return false
}

// Outcome is how a run ended, which is what decides how long its workspace is
// kept.
type Outcome string

// The terminal outcomes a workspace can be closed with.
const (
	OutcomeCompleted Outcome = "completed"
	OutcomeFailed    Outcome = "failed"
	OutcomeCancelled Outcome = "cancelled"
)

// dirPerm is 0700 on every directory the daemon creates: a workspace holds the
// DOM of an internal application and, on a shared machine, must not be
// readable by another account.
const dirPerm os.FileMode = 0o700

// filePerm is 0600 for the same reason.
const filePerm os.FileMode = 0o600

// metaName is the per-run bookkeeping file the retention sweep reads. It is
// dot-prefixed so it cannot collide with a phase directory.
const metaName = ".workspace.json"

// ErrNotFound is returned when a workspace directory does not exist.
var ErrNotFound = errors.New("workspace: not found")

// Retention decides when a finished run's workspace is deleted.
//
// A failed run is kept longer than a successful one on purpose: the workspace
// is the only reproduction of what the model actually saw (ADR-003), and it is
// worthless the moment nobody is debugging. A running workspace is never
// swept, however old.
type Retention struct {
	// KeepCompleted is how long a completed run's workspace survives.
	KeepCompleted time.Duration
	// KeepFailed is how long a failed or cancelled run's workspace survives.
	KeepFailed time.Duration
	// MaxRuns caps how many finished workspaces are kept regardless of age;
	// the oldest are deleted first. Zero means no cap.
	MaxRuns int
}

// DefaultRetention keeps a day of successes and a week of failures.
func DefaultRetention() Retention {
	return Retention{
		KeepCompleted: 24 * time.Hour,
		KeepFailed:    7 * 24 * time.Hour,
		MaxRuns:       200,
	}
}

// Manager owns the workspace root and the retention policy applied to it.
type Manager struct {
	root      string
	retention Retention
	now       func() time.Time
}

// Option customises a Manager.
type Option func(*Manager)

// WithClock replaces the clock, so retention can be tested without sleeping.
func WithClock(now func() time.Time) Option {
	return func(m *Manager) { m.now = now }
}

// NewManager prepares the workspace root. The directory is created 0700 if it
// does not exist.
func NewManager(root string, retention Retention, opts ...Option) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root %q: %w", root, err)
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, fmt.Errorf("workspace: create root %q: %w", abs, err)
	}
	m := &Manager{root: abs, retention: retention, now: time.Now}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Root is the directory every workspace lives under.
func (m *Manager) Root() string { return m.root }

// meta is the bookkeeping the retention sweep reads. It is written on create
// and rewritten on finish; a workspace whose meta is missing or unreadable is
// treated as running, because deleting a workspace we cannot describe is worse
// than keeping it.
type meta struct {
	ProjectID  string     `json:"projectId"`
	RunID      string     `json:"runId"`
	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Outcome    Outcome    `json:"outcome,omitempty"`
}

// Workspace is one run's directory tree.
type Workspace struct {
	dir       string
	projectID string
	runID     string
}

// Create makes /{root}/{projectID}/{runID}/{phase} for every phase.
//
// Creating a workspace that already exists is not an error: a daemon that
// restarted mid-run re-opens the same directory rather than losing the
// evidence it already captured.
func (m *Manager) Create(projectID, runID string) (*Workspace, error) {
	if err := validateID("projectId", projectID); err != nil {
		return nil, err
	}
	if err := validateID("runId", runID); err != nil {
		return nil, err
	}

	dir := filepath.Join(m.root, projectID, runID)
	for _, phase := range Phases {
		if err := os.MkdirAll(filepath.Join(dir, string(phase)), dirPerm); err != nil {
			return nil, fmt.Errorf("workspace: create %s: %w", phase, err)
		}
	}

	ws := &Workspace{dir: dir, projectID: projectID, runID: runID}
	if _, err := os.Stat(filepath.Join(dir, metaName)); errors.Is(err, os.ErrNotExist) {
		if err := writeMeta(dir, meta{ProjectID: projectID, RunID: runID, CreatedAt: m.now().UTC()}); err != nil {
			return nil, err
		}
	}
	return ws, nil
}

// Open returns an existing workspace without creating anything.
func (m *Manager) Open(projectID, runID string) (*Workspace, error) {
	if err := validateID("projectId", projectID); err != nil {
		return nil, err
	}
	if err := validateID("runId", runID); err != nil {
		return nil, err
	}
	dir := filepath.Join(m.root, projectID, runID)
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, dir)
	}
	return &Workspace{dir: dir, projectID: projectID, runID: runID}, nil
}

// Finish records how a run ended so the retention sweep can tell a finished
// workspace from a running one.
func (m *Manager) Finish(ws *Workspace, outcome Outcome) error {
	current, err := readMeta(ws.dir)
	if err != nil {
		current = meta{ProjectID: ws.projectID, RunID: ws.runID, CreatedAt: m.now().UTC()}
	}
	finished := m.now().UTC()
	current.FinishedAt = &finished
	current.Outcome = outcome
	return writeMeta(ws.dir, current)
}

// Dir is the run's root directory.
func (w *Workspace) Dir() string { return w.dir }

// RunID is the run this workspace belongs to.
func (w *Workspace) RunID() string { return w.runID }

// ProjectID is the project this workspace belongs to.
func (w *Workspace) ProjectID() string { return w.projectID }

// PhaseDir is the directory for one phase.
func (w *Workspace) PhaseDir(phase Phase) (string, error) {
	if !phase.IsValid() {
		return "", fmt.Errorf("workspace: unknown phase %q", phase)
	}
	return filepath.Join(w.dir, string(phase)), nil
}

// Path resolves a path inside a phase directory, refusing anything that would
// escape the workspace.
//
// The relative parts can be derived from data observed on the page under test
// (a suggested file name, a test case ref a model wrote), so this validates
// rather than escapes: a name containing a separator or a traversal segment is
// rejected outright, and the resolved path is checked against the root as a
// second gate in case a symlink was planted inside the workspace.
func (w *Workspace) Path(phase Phase, parts ...string) (string, error) {
	base, err := w.PhaseDir(phase)
	if err != nil {
		return "", err
	}
	for _, part := range parts {
		if err := validateSegment(part); err != nil {
			return "", err
		}
	}
	full := filepath.Join(append([]string{base}, parts...)...)
	if !withinDir(base, full) {
		return "", fmt.Errorf("workspace: %q escapes %s", filepath.Join(parts...), phase)
	}
	return full, nil
}

// MkdirAll creates a directory inside a phase, applying the same path rules.
func (w *Workspace) MkdirAll(phase Phase, parts ...string) (string, error) {
	full, err := w.Path(phase, parts...)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(full, dirPerm); err != nil {
		return "", fmt.Errorf("workspace: mkdir %s: %w", full, err)
	}
	return full, nil
}

// WriteFile writes a file inside a phase directory with 0600.
func (w *Workspace) WriteFile(phase Phase, name string, data []byte) (string, error) {
	full, err := w.Path(phase, name)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(full, data, filePerm); err != nil {
		return "", fmt.Errorf("workspace: write %s: %w", full, err)
	}
	return full, nil
}

// Remove deletes the whole workspace.
func (w *Workspace) Remove() error {
	if err := os.RemoveAll(w.dir); err != nil {
		return fmt.Errorf("workspace: remove %s: %w", w.dir, err)
	}
	return nil
}

func withinDir(base, candidate string) bool {
	rel, err := filepath.Rel(base, candidate)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validateID bounds the identifiers that become directory names. They come
// from the backend as uuids, but they arrive over the wire, so they are
// checked here rather than trusted.
func validateID(label, id string) error {
	if id == "" {
		return fmt.Errorf("workspace: %s is empty", label)
	}
	if len(id) > 200 {
		return fmt.Errorf("workspace: %s is too long", label)
	}
	if err := validateSegment(id); err != nil {
		return fmt.Errorf("workspace: %s: %w", label, err)
	}
	return nil
}

func validateSegment(segment string) error {
	switch {
	case segment == "":
		return errors.New("workspace: empty path segment")
	case segment == "." || segment == "..":
		return fmt.Errorf("workspace: %q is a path traversal segment", segment)
	case strings.ContainsAny(segment, `/\`):
		return fmt.Errorf("workspace: %q contains a path separator", segment)
	case strings.ContainsRune(segment, 0):
		return errors.New("workspace: path segment contains a NUL byte")
	case strings.HasPrefix(segment, "."):
		// Keeps run files from colliding with .workspace.json and keeps a
		// model from writing a dotfile the operator will not notice.
		return fmt.Errorf("workspace: %q starts with a dot", segment)
	}
	return nil
}

func writeMeta(dir string, m meta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("workspace: encode metadata: %w", err)
	}
	path := filepath.Join(dir, metaName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, filePerm); err != nil {
		return fmt.Errorf("workspace: write metadata: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("workspace: replace metadata: %w", err)
	}
	return nil
}

func readMeta(dir string) (meta, error) {
	data, err := os.ReadFile(filepath.Join(dir, metaName)) //nolint:gosec // path built from the manager root
	if err != nil {
		return meta{}, fmt.Errorf("workspace: read metadata: %w", err)
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		return meta{}, fmt.Errorf("workspace: decode metadata: %w", err)
	}
	return m, nil
}
