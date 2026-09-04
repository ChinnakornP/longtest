package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// The daemon exposes no inbound port (ADR-002), so `qa-daemon status` cannot
// ask a running daemon anything over a socket. Instead the daemon publishes a
// state file after every state change and status reads it, checking the
// recorded pid to tell "running" from "left behind by a crash".

// StateFileName is the published status document inside the state directory.
const StateFileName = "state.json"

// ConnectionState is what the control-plane connection is doing.
type ConnectionState string

// The connection states a daemon reports.
const (
	ConnectionConnecting ConnectionState = "connecting"
	ConnectionOnline     ConnectionState = "online"
	ConnectionOffline    ConnectionState = "offline"
	ConnectionStopped    ConnectionState = "stopped"
)

// RunState is one run this daemon is working on.
type RunState struct {
	RunID     string    `json:"runId"`
	ProjectID string    `json:"projectId"`
	Mode      string    `json:"mode"`
	Phase     string    `json:"phase"`
	StartedAt time.Time `json:"startedAt"`
}

// State is the published status document.
type State struct {
	PID          int             `json:"pid"`
	Version      string          `json:"version"`
	RuntimeID    string          `json:"runtimeId"`
	RuntimeName  string          `json:"runtimeName,omitempty"`
	ServerURL    string          `json:"serverUrl"`
	Connection   ConnectionState `json:"connection"`
	StartedAt    time.Time       `json:"startedAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
	ConnectedAt  *time.Time      `json:"connectedAt,omitempty"`
	LastError    string          `json:"lastError,omitempty"`
	Reconnects   int             `json:"reconnects"`
	ActiveRuns   []RunState      `json:"activeRuns"`
	CompletedRun int             `json:"completedRuns"`
}

// Stale reports whether the daemon that wrote this state is gone. A state file
// outlives a crash, and reporting a dead daemon as online is the one answer
// status must never give.
func (s State) Stale() bool {
	if s.Connection == ConnectionStopped {
		return true
	}
	return !processAlive(s.PID)
}

// StateFile publishes the daemon's status atomically.
type StateFile struct {
	path string

	mu    sync.Mutex
	state State
	now   func() time.Time
}

// NewStateFile prepares the state directory and returns a publisher.
func NewStateFile(path string, initial State, now func() time.Time) (*StateFile, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("runtime: create state directory: %w", err)
	}
	initial.PID = os.Getpid()
	initial.Version = Version
	if initial.StartedAt.IsZero() {
		initial.StartedAt = now().UTC()
	}
	sf := &StateFile{path: path, state: initial, now: now}
	if err := sf.publish(); err != nil {
		return nil, err
	}
	return sf, nil
}

// Path is where the state document is written.
func (s *StateFile) Path() string { return s.path }

// Update applies a mutation and republishes. A failure to write is not fatal
// to the daemon — status becomes stale, runs do not stop — so callers log it
// rather than abandoning a run over it.
func (s *StateFile) Update(mutate func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	mutate(&s.state)
	return s.publish()
}

// Snapshot returns the current state.
func (s *StateFile) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state
	out.ActiveRuns = slices.Clone(s.state.ActiveRuns)
	return out
}

func (s *StateFile) publish() error {
	s.state.UpdatedAt = s.now().UTC()
	if s.state.ActiveRuns == nil {
		s.state.ActiveRuns = []RunState{}
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime: encode state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("runtime: write state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("runtime: replace state: %w", err)
	}
	return nil
}

// ErrNoState means no daemon has ever published status here.
var ErrNoState = errors.New("runtime: no daemon state file; is the daemon running?")

// ReadState loads a published state document.
func ReadState(path string) (State, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own state directory
	switch {
	case errors.Is(err, os.ErrNotExist):
		return State{}, fmt.Errorf("%w (looked in %s)", ErrNoState, path)
	case err != nil:
		return State{}, fmt.Errorf("runtime: read state: %w", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("runtime: parse %s: %w", path, err)
	}
	return state, nil
}

// StatePath is the default location of the state document.
func StatePath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, StateFileName), nil
}
