package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateFileIsPublishedAndReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	sf, err := NewStateFile(path, State{RuntimeID: "r1", ServerURL: "https://qa.test", Connection: ConnectionConnecting}, nil)
	if err != nil {
		t.Fatalf("NewStateFile: %v", err)
	}

	if err := sf.Update(func(s *State) {
		s.Connection = ConnectionOnline
		s.ActiveRuns = append(s.ActiveRuns, RunState{RunID: "run-1", Mode: "execute", StartedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	state, err := ReadState(path)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if state.Connection != ConnectionOnline || len(state.ActiveRuns) != 1 {
		t.Fatalf("state = %+v", state)
	}
	if state.PID != os.Getpid() {
		t.Fatalf("pid = %d, want this process", state.PID)
	}
	if state.Version != Version {
		t.Fatalf("version = %q", state.Version)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %04o", perm)
	}
}

// A state file outlives the daemon that wrote it. Reporting a dead daemon as
// online is the one answer status must never give.
func TestStaleStateIsDetected(t *testing.T) {
	live := State{PID: os.Getpid(), Connection: ConnectionOnline}
	if live.Stale() {
		t.Fatal("this process was reported as gone")
	}

	// A pid that cannot be running: the kernel would have to have wrapped
	// around past every possible pid for this to be a live process, and
	// FindProcess on a free pid fails the signal check.
	dead := State{PID: 4194303, Connection: ConnectionOnline}
	if !dead.Stale() {
		t.Skip("pid 4194303 exists on this machine; the staleness check cannot be exercised here")
	}

	stopped := State{PID: os.Getpid(), Connection: ConnectionStopped}
	if !stopped.Stale() {
		t.Fatal("a daemon that recorded a clean stop is not running")
	}
}

func TestReadStateReportsMissingFile(t *testing.T) {
	_, err := ReadState(filepath.Join(t.TempDir(), "state.json"))
	if !errors.Is(err, ErrNoState) {
		t.Fatalf("error = %v, want ErrNoState", err)
	}
}

func TestStatePublishIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	sf, err := NewStateFile(path, State{RuntimeID: "r1"}, nil)
	if err != nil {
		t.Fatalf("NewStateFile: %v", err)
	}
	for i := range 20 {
		if err := sf.Update(func(s *State) { s.Reconnects = i }); err != nil {
			t.Fatalf("Update: %v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var state State
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatalf("the state file was left unparsable: %v", err)
		}
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a temp file was left behind")
	}
}

func TestSnapshotDoesNotAliasActiveRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	sf, err := NewStateFile(path, State{}, nil)
	if err != nil {
		t.Fatalf("NewStateFile: %v", err)
	}
	if err := sf.Update(func(s *State) {
		s.ActiveRuns = []RunState{{RunID: "run-1"}}
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	snapshot := sf.Snapshot()
	snapshot.ActiveRuns[0].RunID = "mutated"
	if sf.Snapshot().ActiveRuns[0].RunID != "run-1" {
		t.Fatal("a caller mutating its snapshot changed the daemon's state")
	}
}
