package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ChinnakornP/longtest/daemon/runtime"
)

func runStatus(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("status", stderr)
	output := fs.String("output", "text", "output format: text or json")
	statePath := fs.String("state", "", "path to the daemon state file (default: platform state directory)")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	path := *statePath
	if path == "" {
		resolved, err := runtime.StatePath()
		if err != nil {
			return err
		}
		path = resolved
	}

	state, err := runtime.ReadState(path)
	if errors.Is(err, runtime.ErrNoState) {
		if *output == "json" {
			return writeJSON(stdout, map[string]any{"running": false, "connection": "stopped"})
		}
		return err
	}
	if err != nil {
		return err
	}

	// A state file outlives the process that wrote it, so status reports what
	// the pid says, not what the file claims: a crashed daemon must never be
	// reported as online.
	running := !state.Stale()
	connection := state.Connection
	if !running {
		connection = runtime.ConnectionStopped
	}

	if *output == "json" {
		return writeJSON(stdout, statusJSON{
			Running:     running,
			PID:         state.PID,
			Version:     state.Version,
			RuntimeID:   state.RuntimeID,
			RuntimeName: state.RuntimeName,
			ServerURL:   state.ServerURL,
			Connection:  string(connection),
			StartedAt:   state.StartedAt,
			UpdatedAt:   state.UpdatedAt,
			ConnectedAt: state.ConnectedAt,
			Reconnects:  state.Reconnects,
			LastError:   state.LastError,
			ActiveRuns:  state.ActiveRuns,
			Completed:   state.CompletedRun,
		})
	}

	out := printer{stdout}
	out.printf("runtime    %s", state.RuntimeID)
	if state.RuntimeName != "" {
		out.printf(" (%s)", state.RuntimeName)
	}
	out.printf("\nserver     %s\n", state.ServerURL)
	if running {
		out.printf("daemon     running, pid %d, version %s\n", state.PID, state.Version)
	} else {
		out.printf("daemon     not running (last update %s)\n", state.UpdatedAt.Format(time.RFC3339))
	}
	out.printf("connection %s\n", connection)
	if state.LastError != "" {
		out.printf("last error %s\n", state.LastError)
	}
	out.printf("runs       %d active, %d completed this session, %d reconnects\n",
		len(state.ActiveRuns), state.CompletedRun, state.Reconnects)
	for _, run := range state.ActiveRuns {
		phase := run.Phase
		if phase == "" {
			phase = "starting"
		}
		out.printf("  - %s  mode=%s phase=%s since=%s\n",
			run.RunID, run.Mode, phase, run.StartedAt.Format(time.RFC3339))
	}
	return nil
}

type statusJSON struct {
	Running     bool               `json:"running"`
	PID         int                `json:"pid"`
	Version     string             `json:"version"`
	RuntimeID   string             `json:"runtimeId"`
	RuntimeName string             `json:"runtimeName,omitempty"`
	ServerURL   string             `json:"serverUrl"`
	Connection  string             `json:"connection"`
	StartedAt   time.Time          `json:"startedAt"`
	UpdatedAt   time.Time          `json:"updatedAt"`
	ConnectedAt *time.Time         `json:"connectedAt,omitempty"`
	Reconnects  int                `json:"reconnects"`
	LastError   string             `json:"lastError,omitempty"`
	ActiveRuns  []runtime.RunState `json:"activeRuns"`
	Completed   int                `json:"completedRuns"`
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}
