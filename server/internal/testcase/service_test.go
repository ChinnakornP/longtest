package testcase

import (
	"testing"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
)

// The review lifecycle is deliberately not a free-for-all: an archived case is
// reinstated as a draft rather than jumping straight back into the regression
// suite a reviewer never re-read.
func TestAllowedTransition(t *testing.T) {
	const (
		draft    = dbgen.TestCaseStatusDraft
		approved = dbgen.TestCaseStatusApproved
		archived = dbgen.TestCaseStatusArchived
	)

	tests := []struct {
		name string
		from dbgen.TestCaseStatus
		to   dbgen.TestCaseStatus
		want bool
	}{
		{name: "a draft is approved", from: draft, to: approved, want: true},
		{name: "a draft is discarded", from: draft, to: archived, want: true},
		{name: "an approved case is retired", from: approved, to: archived, want: true},
		{name: "an approved case goes back for rework", from: approved, to: draft, want: true},
		{name: "an archived case is reopened as a draft", from: archived, to: draft, want: true},
		{name: "an archived case cannot rejoin the suite directly", from: archived, to: approved},
		{name: "an unknown status is never a source", from: dbgen.TestCaseStatus("bogus"), to: draft},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := allowedTransition(tc.from, tc.to); got != tc.want {
				t.Fatalf("allowedTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

func TestParseStatus(t *testing.T) {
	for _, raw := range []string{"draft", "approved", "archived"} {
		if _, err := parseStatus(raw); err != nil {
			t.Errorf("parseStatus(%q): %v", raw, err)
		}
	}
	for _, raw := range []string{"", "Draft", "blessed", "draft "} {
		if _, err := parseStatus(raw); err == nil {
			t.Errorf("parseStatus(%q) accepted it", raw)
		}
	}
}

// An absent filter means "every status", which is a different thing from a
// filter that happens to match nothing.
func TestParseStatusFilter(t *testing.T) {
	filter, err := parseStatusFilter("")
	if err != nil {
		t.Fatalf("empty filter: %v", err)
	}
	if filter.Valid {
		t.Fatal("an empty filter was turned into a status")
	}

	filter, err = parseStatusFilter("approved")
	if err != nil {
		t.Fatalf("approved filter: %v", err)
	}
	if !filter.Valid || filter.TestCaseStatus != dbgen.TestCaseStatusApproved {
		t.Fatalf("got %+v, want approved", filter)
	}

	if _, err := parseStatusFilter("nonsense"); err == nil {
		t.Fatal("accepted an unknown status as a filter")
	}
}
