package run

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

func TestParseMode(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    dbgen.RunMode
		wantErr bool
	}{
		{name: "discover", raw: "discover", want: dbgen.RunModeDiscover},
		{name: "plan", raw: "plan", want: dbgen.RunModePlan},
		{name: "execute", raw: "execute", want: dbgen.RunModeExecute},
		{name: "full", raw: "full", want: dbgen.RunModeFull},
		{name: "empty", raw: "", wantErr: true},
		{name: "unknown", raw: "explode", wantErr: true},
		// An unknown mode must be a named validation failure, not a database
		// enum error that surfaces as a 500.
		{name: "wrong case", raw: "Discover", wantErr: true},
		{name: "sql-ish", raw: "full; drop table runs", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMode(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseMode(%q) accepted it as %q", tc.raw, got)
				}
				assertStatus(t, err, http.StatusUnprocessableEntity)
				return
			}
			if err != nil {
				t.Fatalf("parseMode(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("parseMode(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestValidateIdempotencyKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "absent", key: ""},
		{name: "a uuid", key: uuid.NewString()},
		{name: "a hash", key: strings.Repeat("a", 64)},
		{name: "at the limit", key: strings.Repeat("k", maxIdempotencyKeyLength)},
		{name: "over the limit", key: strings.Repeat("k", maxIdempotencyKeyLength+1), wantErr: true},
		// The key goes into a unique index and into logs; surrounding
		// whitespace makes two keys that look identical behave differently.
		{name: "leading space", key: " key", wantErr: true},
		{name: "trailing newline", key: "key\n", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIdempotencyKey(tc.key)
			if tc.wantErr && err == nil {
				t.Fatalf("accepted %q", tc.key)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("rejected %q: %v", tc.key, err)
			}
		})
	}
}

// The verdict is derived from what landed, never taken from the daemon.
func TestTerminalStatusFor(t *testing.T) {
	tests := []struct {
		name       string
		counted    dbgen.Run
		payload    resultPayload
		wantStatus dbgen.RunStatus
		wantCode   string
		wantErr    bool
	}{
		{
			name:       "completed with everything passing",
			counted:    dbgen.Run{TotalCount: 3, PassedCount: 3},
			payload:    resultPayload{Status: "completed"},
			wantStatus: dbgen.RunStatusPassed,
		},
		{
			name: "completed but an execution failed",
			// The daemon says it finished; the run did not pass. Trusting the
			// daemon's word here would report a green run with a red test in it.
			counted:    dbgen.Run{TotalCount: 3, PassedCount: 2, FailedCount: 1},
			payload:    resultPayload{Status: "completed"},
			wantStatus: dbgen.RunStatusFailed,
		},
		{
			name:       "completed but an execution errored",
			counted:    dbgen.Run{TotalCount: 2, PassedCount: 1, ErrorCount: 1},
			payload:    resultPayload{Status: "completed"},
			wantStatus: dbgen.RunStatusFailed,
		},
		{
			name:       "skipped executions are not failures",
			counted:    dbgen.Run{TotalCount: 2, PassedCount: 1, SkippedCount: 1},
			payload:    resultPayload{Status: "completed"},
			wantStatus: dbgen.RunStatusPassed,
		},
		{
			name:       "cancelled",
			counted:    dbgen.Run{TotalCount: 1, FailedCount: 1},
			payload:    resultPayload{Status: "cancelled"},
			wantStatus: dbgen.RunStatusCancelled,
		},
		{
			name: "the harness broke",
			// `error`, not `failed`: the application under test never got to
			// misbehave, and a report that conflates the two is unreadable.
			counted: dbgen.Run{},
			payload: resultPayload{
				Status: "failed",
				Error:  &qaschema.RunError{Code: qaschema.RunErrorCodeBrowserLaunchFailed, Message: "chromium did not start"},
			},
			wantStatus: dbgen.RunStatusError,
			wantCode:   "browser_launch_failed",
		},
		{
			name:       "the harness broke without saying why",
			payload:    resultPayload{Status: "failed"},
			wantStatus: dbgen.RunStatusError,
			wantCode:   "internal",
		},
		{name: "an unknown status", payload: resultPayload{Status: "exploded"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := terminalStatusFor(tc.counted, tc.payload)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted status %q as %q", tc.payload.Status, got.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("terminalStatusFor: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("got %q, want %q", got.Status, tc.wantStatus)
			}
			if tc.wantCode != "" && got.ErrorCode != tc.wantCode {
				t.Fatalf("got error code %q, want %q", got.ErrorCode, tc.wantCode)
			}
			if tc.wantStatus != dbgen.RunStatusError && got.ErrorCode != "" {
				t.Fatalf("a %s run carries an error code %q", got.Status, got.ErrorCode)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := map[dbgen.RunStatus]bool{
		dbgen.RunStatusQueued:    false,
		dbgen.RunStatusAssigned:  false,
		dbgen.RunStatusRunning:   false,
		dbgen.RunStatusPassed:    true,
		dbgen.RunStatusFailed:    true,
		dbgen.RunStatusCancelled: true,
		dbgen.RunStatusError:     true,
	}
	for status, want := range terminal {
		if got := isTerminal(status); got != want {
			t.Errorf("isTerminal(%q) = %v, want %v", status, got, want)
		}
	}
}

// A selection that names the same case twice must not read as a missing one.
func TestDedupeIDsKeepsOrder(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	got := dedupeIDs([]uuid.UUID{a, b, a, c, b})
	want := []uuid.UUID{a, b, c}

	if len(got) != len(want) {
		t.Fatalf("got %d ids, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at %d got %s, want %s", i, got[i], want[i])
		}
	}
}

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	var apiErr *httpx.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not an *httpx.Error: %v", err)
	}
	if apiErr.Status != want {
		t.Fatalf("got status %d, want %d (%v)", apiErr.Status, want, err)
	}
}
