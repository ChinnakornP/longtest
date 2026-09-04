package auth

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestNewSecretIsUniqueAndUrlSafe(t *testing.T) {
	t.Parallel()

	const iterations = 200
	seen := make(map[string]bool, iterations)
	for range iterations {
		secret, err := newSecret()
		if err != nil {
			t.Fatalf("newSecret: %v", err)
		}
		if seen[secret] {
			t.Fatal("newSecret returned a duplicate - the source of randomness is broken")
		}
		seen[secret] = true

		// It travels in a cookie and an Authorization header; anything outside
		// the URL-safe base64 alphabet would need escaping somewhere.
		if strings.ContainsAny(secret, "+/= ") {
			t.Fatalf("secret is not URL-safe: %q", secret)
		}
	}
}

func TestHashSecretIsTheColumnWidth(t *testing.T) {
	t.Parallel()

	// Every *_hash column CHECKs octet_length = 32. A hash of another width
	// would fail at the very bottom of the stack, in a transaction.
	hash := hashSecret("anything at all")
	if len(hash) != 32 {
		t.Fatalf("hashSecret returned %d bytes, want 32", len(hash))
	}
	if !bytes.Equal(hash, hashSecret("anything at all")) {
		t.Fatal("hashSecret is not deterministic")
	}
	if bytes.Equal(hash, hashSecret("anything at all ")) {
		t.Fatal("hashSecret collided on inputs differing by one byte")
	}
}

func TestNewRuntimeTokenIsPrefixedAndHashed(t *testing.T) {
	t.Parallel()

	token, hash, err := NewRuntimeToken()
	if err != nil {
		t.Fatalf("NewRuntimeToken: %v", err)
	}
	if !strings.HasPrefix(token, RuntimeTokenPrefix) {
		t.Fatalf("token %q does not carry the %q prefix", token, RuntimeTokenPrefix)
	}

	// The hash stored in runtime_tokens must be the hash of the WHOLE token
	// including its prefix, or authentication looks the wrong value up.
	lookup, err := HashBearerToken(token)
	if err != nil {
		t.Fatalf("HashBearerToken: %v", err)
	}
	if !bytes.Equal(hash, lookup) {
		t.Fatal("the stored hash is not what the authentication path computes")
	}
}

func TestNormalizePairingCode(t *testing.T) {
	t.Parallel()

	code, hash, err := NewPairingCode()
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	if len(code) != pairingCodeGroups*pairingCodeGroupSize+pairingCodeGroups-1 {
		t.Fatalf("unexpected code shape: %q", code)
	}

	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"as issued", code, true},
		{"lower case", strings.ToLower(code), true},
		{"without dashes", strings.ReplaceAll(code, "-", ""), true},
		{"with spaces", strings.ReplaceAll(code, "-", " "), true},
		{"surrounded by whitespace", "  " + code + "  ", true},
		{"empty", "", false},
		{"too short", strings.TrimSuffix(code, "X")[:5], false},
		{"too long", code + "ABCD", false},
		{"ambiguous letters are not in the alphabet", "ILOU-ILOU-ILOU", false},
		{"punctuation", code[:len(code)-1] + "!", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			normalised := NormalizePairingCode(tt.input)
			if !tt.valid {
				if normalised != "" {
					t.Fatalf("NormalizePairingCode(%q) = %q, want the empty string", tt.input, normalised)
				}
				if _, err := HashPairingCode(tt.input); !errors.Is(err, ErrMalformedCredential) {
					t.Fatalf("HashPairingCode(%q): got %v, want ErrMalformedCredential", tt.input, err)
				}
				return
			}

			// Every accepted spelling of the same code has to hash to the row
			// the issuer wrote, or a user who retypes it in lower case is told
			// their code is invalid.
			got, err := HashPairingCode(tt.input)
			if err != nil {
				t.Fatalf("HashPairingCode(%q): %v", tt.input, err)
			}
			if !bytes.Equal(got, hash) {
				t.Fatalf("HashPairingCode(%q) does not match the issued code's hash", tt.input)
			}
		})
	}
}

func TestPairingCodesAreUnique(t *testing.T) {
	t.Parallel()

	const iterations = 200
	seen := make(map[string]bool, iterations)
	for range iterations {
		code, _, err := NewPairingCode()
		if err != nil {
			t.Fatalf("NewPairingCode: %v", err)
		}
		if seen[code] {
			t.Fatalf("NewPairingCode repeated %q within %d draws", code, iterations)
		}
		seen[code] = true
	}
}

func TestHashBearerTokenBoundsItsInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{"empty", "", true},
		{"normal", "a-perfectly-ordinary-token", false},
		{"at the limit", strings.Repeat("t", 512), false},
		{"over the limit", strings.Repeat("t", 513), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := HashBearerToken(tt.token)
			if tt.wantErr != (err != nil) {
				t.Fatalf("HashBearerToken(%d bytes): got %v, wantErr=%v", len(tt.token), err, tt.wantErr)
			}
		})
	}
}
