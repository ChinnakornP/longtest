package org

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
)

// The slug is the human-readable, URL-safe handle of an organization. It is
// derived from the name once, at creation, and is never recomputed: renaming
// an organization must not change a URL somebody bookmarked.

const (
	// maxSlugLength matches the CHECK in migration 00002: a DNS label.
	maxSlugLength = 63
	// fallbackSlug is used when a name contains no usable characters at all
	// (an org called "!!!", or one written entirely in a script we do not
	// transliterate).
	fallbackSlug = "org"
	// numberedSuffixAttempts is how many "-2", "-3" ... variants are tried
	// before falling back to a random suffix.
	numberedSuffixAttempts = 8
	randomSuffixLength     = 6
	// slugSuffixAlphabet has no vowels, so a random suffix cannot spell
	// something unfortunate, and no ambiguous glyphs.
	slugSuffixAlphabet = "23456789bcdfghjkmnpqrstvwxz"
)

// Slugify converts a display name into a candidate slug.
//
// The output always satisfies the schema's CHECK
// (^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$) or is the fallback, so a name in
// any script produces something insertable rather than a constraint violation
// at the bottom of the stack.
func Slugify(name string) string {
	var b strings.Builder
	lastWasDash := false

	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastWasDash = false
		default:
			// Any run of non-alphanumerics - spaces, punctuation, or a
			// character outside ASCII - collapses to a single separator.
			if !lastWasDash && b.Len() > 0 {
				b.WriteByte('-')
				lastWasDash = true
			}
		}
	}

	slug := strings.Trim(b.String(), "-")
	if len(slug) > maxSlugLength {
		slug = strings.Trim(slug[:maxSlugLength], "-")
	}
	if slug == "" {
		return fallbackSlug
	}
	return slug
}

// availableSlug returns a slug that no organization is using yet.
//
// It probes rather than inserting-and-retrying because a failed INSERT aborts
// the surrounding transaction: signup creates the user, the org, the
// membership and the session in one transaction, and a unique violation
// halfway through would poison all of it.
//
// The probe is not a lock, so two organizations created with the same name at
// the same instant can still collide on the unique index. The final candidate
// carries 28 bits of randomness precisely so that outcome needs a real
// coincidence rather than merely a busy second; if it happens anyway the
// caller sees a 409 and can retry.
func availableSlug(ctx context.Context, q dbgen.Querier, name string) (string, error) {
	base := Slugify(name)

	candidates := make([]string, 0, numberedSuffixAttempts+1)
	candidates = append(candidates, base)
	for i := 2; i <= numberedSuffixAttempts; i++ {
		candidates = append(candidates, withSuffix(base, fmt.Sprintf("%d", i)))
	}
	random, err := randomSuffix()
	if err != nil {
		return "", err
	}
	candidates = append(candidates, withSuffix(base, random))

	for _, candidate := range candidates {
		_, err := q.GetOrganizationBySlug(ctx, candidate)
		if err == nil {
			continue // taken
		}
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return candidate, nil
		}
		return "", fmt.Errorf("check organization slug: %w", db.Classify(err))
	}

	// Every candidate including the random one is taken. This is not a state
	// worth retrying automatically.
	return "", fmt.Errorf("could not find a free slug for %q", base)
}

// withSuffix appends "-suffix", trimming the base so the result still fits the
// length limit.
func withSuffix(base, suffix string) string {
	room := maxSlugLength - len(suffix) - 1
	if room < 1 {
		return suffix
	}
	if len(base) > room {
		base = strings.Trim(base[:room], "-")
	}
	if base == "" {
		base = fallbackSlug
	}
	return base + "-" + suffix
}

func randomSuffix() (string, error) {
	b := make([]byte, randomSuffixLength)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate slug suffix: %w", err)
	}
	for i, v := range b {
		b[i] = slugSuffixAlphabet[int(v)%len(slugSuffixAlphabet)]
	}
	return string(b), nil
}
