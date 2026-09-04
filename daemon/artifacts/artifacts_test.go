package artifacts

import (
	"testing"
	"time"
)

func TestObjectKey(t *testing.T) {
	day := time.Date(2026, 9, 4, 23, 30, 0, 0, time.UTC)

	got, err := ObjectKey(day, "run-123", "TC-001", "trace.zip")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "runs/2026-09-04/run-123/TC-001/trace.zip"; got != want {
		t.Fatalf("ObjectKey = %q, want %q", got, want)
	}
}

func TestObjectKeyRejectsUnsafeSegments(t *testing.T) {
	day := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		runID      string
		testCaseID string
		file       string
	}{
		{"empty name", "run-123", "TC-001", ""},
		{"traversal in name", "run-123", "TC-001", ".."},
		{"separator in name", "run-123", "TC-001", "../../etc/passwd"},
		{"backslash in name", "run-123", "TC-001", `..\..\secrets`},
		{"separator in run id", "run/123", "TC-001", "trace.zip"},
		{"separator in test case id", "run-123", "TC/001", "trace.zip"},
		{"nul byte", "run-123", "TC-001", "trace.zip\x00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := ObjectKey(day, tt.runID, tt.testCaseID, tt.file); err == nil {
				t.Fatalf("expected an error, got key %q", got)
			}
		})
	}
}
