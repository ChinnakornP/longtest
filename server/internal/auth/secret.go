package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Every bearer credential this package issues - session cookies, invite
// tokens, runtime tokens, pairing codes - follows the same rule: a random
// secret is generated, its SHA-256 is stored, and the secret itself is
// returned to the caller exactly once and never persisted.
//
// SHA-256 rather than argon2 is deliberate. These are 128- to 256-bit random
// values, not user-chosen passwords, so there is no dictionary to attack and
// no benefit to a slow hash - while a slow hash on the session lookup would be
// paid on every single request.

const (
	// secretBytes is the entropy behind a session or bearer token. 32 bytes is
	// far past any brute-force horizon and keeps the encoded value short
	// enough for a cookie and an Authorization header.
	secretBytes = 32

	// RuntimeTokenPrefix marks a daemon credential so that a token pasted into
	// a chat or a log line is recognisable, and so a secret scanner can be
	// taught one pattern. It is not a secret and carries no information.
	RuntimeTokenPrefix = "qart_" //nolint:gosec // a label, not a credential

	// pairingCodeGroups x pairingCodeGroupSize characters from a 32-symbol
	// alphabet is 60 bits: not brute-forceable inside a 15-minute TTL, and
	// still typable by a human reading it off a screen.
	pairingCodeGroups    = 3
	pairingCodeGroupSize = 4
	// Crockford base32 without I, L, O and U: no character pair that a person
	// re-typing a code can confuse, and no accidental words.
	pairingCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// ErrMalformedCredential is returned for a value that cannot be a credential
// this package issued - the wrong length, or characters outside its alphabet.
// It is reported to clients as the same failure as "no such credential".
var ErrMalformedCredential = errors.New("malformed credential")

// newSecret returns a URL-safe random secret.
func newSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashSecret is the one-way function behind every *_hash column in the schema.
// The result is exactly 32 bytes, which every one of those columns CHECKs.
func hashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// NewRuntimeToken returns a prefixed daemon token and its hash. The token is
// shown once, in the response to POST /api/v1/runtimes/redeem.
func NewRuntimeToken() (token string, hash []byte, err error) {
	secret, err := newSecret()
	if err != nil {
		return "", nil, err
	}
	token = RuntimeTokenPrefix + secret
	return token, hashSecret(token), nil
}

// NewPairingCode returns a human-typable one-time code and its hash, e.g.
// "K7Q2-9FMR-3XT8".
func NewPairingCode() (code string, hash []byte, err error) {
	groups := make([]string, pairingCodeGroups)
	for g := range groups {
		chars := make([]byte, pairingCodeGroupSize)
		for i := range chars {
			// rand.Text-style rejection-free selection: the alphabet is
			// exactly 32 symbols, so 5 bits map onto it without bias.
			var b [1]byte
			if _, err := rand.Read(b[:]); err != nil {
				return "", nil, fmt.Errorf("generate pairing code: %w", err)
			}
			chars[i] = pairingCodeAlphabet[b[0]%32]
		}
		groups[g] = string(chars)
	}
	code = strings.Join(groups, "-")
	return code, hashSecret(NormalizePairingCode(code)), nil
}

// NormalizePairingCode canonicalises a code a person typed: case is ignored,
// separators are ignored. The hash is always taken of the normalised form, so
// "k7q2 9fmr 3xt8" and "K7Q2-9FMR-3XT8" are the same code.
//
// It returns "" when the input cannot be a pairing code, which callers treat
// as "no such code" rather than reporting a distinct error - a daemon does not
// need to know which of the two it got wrong.
func NormalizePairingCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			continue
		case strings.ContainsRune(pairingCodeAlphabet, r):
			b.WriteRune(r)
		default:
			return ""
		}
	}
	if b.Len() != pairingCodeGroups*pairingCodeGroupSize {
		return ""
	}
	return b.String()
}

// HashPairingCode normalises and hashes a code for lookup. An unusable code
// returns ErrMalformedCredential rather than a hash that cannot match, so the
// caller can skip the query.
func HashPairingCode(raw string) ([]byte, error) {
	normalised := NormalizePairingCode(raw)
	if normalised == "" {
		return nil, ErrMalformedCredential
	}
	return hashSecret(normalised), nil
}

// HashBearerToken hashes a session, invite or runtime token for lookup. Length
// is bounded first so an oversized header is rejected before it is hashed.
func HashBearerToken(token string) ([]byte, error) {
	const maxTokenLength = 512
	if token == "" || len(token) > maxTokenLength {
		return nil, ErrMalformedCredential
	}
	return hashSecret(token), nil
}

// NewInviteToken returns the secret behind an invite link. Unlike a runtime
// token it carries no prefix: it appears in a URL a person clicks, and a
// recognisable prefix there only helps somebody scraping a mailbox.
func NewInviteToken() (string, error) {
	return newSecret()
}
