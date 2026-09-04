package org

import (
	"regexp"
	"strings"
	"testing"
)

// The CHECK from migration 00002. A slug that does not match it is rejected at
// the bottom of a transaction, so Slugify has to guarantee the shape itself.
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func TestSlugify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "Acme", "acme"},
		{"two words", "Acme QA", "acme-qa"},
		{"already a slug", "acme-qa", "acme-qa"},
		{"mixed case and spacing", "  ACME   QA  ", "acme-qa"},
		{"punctuation collapses", "Acme, Inc. (QA!)", "acme-inc-qa"},
		{"underscores are separators", "acme_qa_team", "acme-qa-team"},
		{"digits are kept", "Team 42", "team-42"},
		{"leading and trailing junk", "---Acme---", "acme"},
		{"runs of separators collapse", "a  ///  b", "a-b"},
		{"non-ascii collapses to separators", "บริษัท ทดสอบ", fallbackSlug},
		{"mixed script keeps the ascii", "Acme บริษัท", "acme"},
		{"nothing usable", "!!! ???", fallbackSlug},
		{"empty", "", fallbackSlug},
		{"whitespace only", "   \t\n ", fallbackSlug},
		{"emoji", "🚀🚀🚀", fallbackSlug},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Slugify(tt.in); got != tt.want {
				t.Fatalf("Slugify(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Whatever a user types as an organization name, the derived slug must be
// insertable: the alternative is a signup that fails on a CHECK constraint.
func TestSlugifyAlwaysProducesAnInsertableSlug(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"", "-", "---", "!", "a", "A", "42",
		strings.Repeat("x", 500),
		strings.Repeat("very long organization name ", 20),
		strings.Repeat("-", 100) + "a",
		"a" + strings.Repeat("-", 100),
		"บริษัท ทดสอบ จำกัด",
		"Ünïcödé Ørg",
		"🚀 Rocket QA 🚀",
		"<script>alert(1)</script>",
		"'; DROP TABLE organizations; --",
		strings.Repeat("ab-", 40),
	}

	for _, in := range inputs {
		slug := Slugify(in)
		if !slugPattern.MatchString(slug) {
			t.Fatalf("Slugify(%q) = %q, which the schema's CHECK rejects", truncate(in), slug)
		}
		if len(slug) > maxSlugLength {
			t.Fatalf("Slugify(%q) is %d characters, over the %d limit", truncate(in), len(slug), maxSlugLength)
		}
	}
}

func TestWithSuffixStaysInsertable(t *testing.T) {
	t.Parallel()

	bases := []string{"acme", strings.Repeat("x", maxSlugLength), strings.Repeat("y", maxSlugLength-2), "a"}
	suffixes := []string{"2", "9", "abcdef", strings.Repeat("z", maxSlugLength)}

	for _, base := range bases {
		for _, suffix := range suffixes {
			got := withSuffix(base, suffix)
			if !slugPattern.MatchString(got) {
				t.Fatalf("withSuffix(%q, %q) = %q, which the schema's CHECK rejects",
					truncate(base), suffix, got)
			}
		}
	}
}

func TestRandomSuffix(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for range 100 {
		got, err := randomSuffix()
		if err != nil {
			t.Fatalf("randomSuffix: %v", err)
		}
		if len(got) != randomSuffixLength {
			t.Fatalf("randomSuffix() = %q, want %d characters", got, randomSuffixLength)
		}
		for _, r := range got {
			if !strings.ContainsRune(slugSuffixAlphabet, r) {
				t.Fatalf("randomSuffix() = %q, which contains %q from outside the alphabet", got, r)
			}
		}
		seen[got] = true
	}
	// 27^6 possibilities; a handful of collisions in 100 draws would mean the
	// alphabet mapping is broken, not bad luck.
	if len(seen) < 95 {
		t.Fatalf("randomSuffix produced only %d distinct values in 100 draws", len(seen))
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
