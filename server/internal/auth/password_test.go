package auth

import (
	"errors"
	"strings"
	"testing"
)

// The hasher is exercised at the cheap parameters: what is under test is the
// encoding, the parsing and the comparison, none of which depend on cost.
func testHasher() *Hasher { return NewHasher(FastPasswordParams()) }

func TestHashAndVerify(t *testing.T) {
	t.Parallel()
	h := testHasher()

	tests := []struct {
		name     string
		password string
	}{
		{"ascii", "correct horse battery staple"},
		{"unicode", "รหัสผ่านภาษาไทย-ที่ยาวพอ"},
		{"punctuation only", `!@#$%^&*()_+-=[]{}|;:,.<>?`},
		{"at the maximum length", strings.Repeat("a", MaxPasswordLength)},
		{"minimum length", strings.Repeat("x", MinPasswordLength)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := h.Hash(tt.password)
			if err != nil {
				t.Fatalf("hash: %v", err)
			}
			if strings.Contains(encoded, tt.password) {
				t.Fatal("the encoded hash contains the plaintext password")
			}
			if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
				t.Fatalf("unexpected hash format: %q", encoded)
			}

			if err := h.Verify(encoded, tt.password); err != nil {
				t.Fatalf("verify the correct password: %v", err)
			}
			if err := h.Verify(encoded, tt.password+"x"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("verify a wrong password: got %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

func TestHashIsSaltedPerCall(t *testing.T) {
	t.Parallel()
	h := testHasher()

	first, err := h.Hash("the same password twice")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	second, err := h.Hash("the same password twice")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	// Equal hashes would mean an unsalted scheme, where one rainbow table
	// covers every account at once.
	if first == second {
		t.Fatal("hashing the same password twice produced identical hashes")
	}
	for _, encoded := range []string{first, second} {
		if err := h.Verify(encoded, "the same password twice"); err != nil {
			t.Fatalf("verify: %v", err)
		}
	}
}

func TestVerifyRejectsUnusableHashes(t *testing.T) {
	t.Parallel()
	h := testHasher()

	valid, err := h.Hash("a perfectly fine password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	parts := strings.Split(valid, "$")

	tests := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"not a phc string", "not-a-hash"},
		{"too few fields", "$argon2id$v=19$m=64,t=1,p=1$c2FsdA"},
		{"another algorithm", "$argon2i$v=19$" + strings.Join(parts[3:], "$")},
		{"bcrypt hash from an older seed", "$2a$10$abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ012"},
		{"wrong argon2 version", "$argon2id$v=16$" + strings.Join(parts[3:], "$")},
		{"unreadable parameters", "$argon2id$v=19$m=nope,t=1,p=1$" + parts[4] + "$" + parts[5]},
		{"zero cost", "$argon2id$v=19$m=0,t=0,p=0$" + parts[4] + "$" + parts[5]},
		{"salt is not base64", "$argon2id$v=19$m=64,t=1,p=1$!!!!$" + parts[5]},
		{"key is not base64", "$argon2id$v=19$m=64,t=1,p=1$" + parts[4] + "$!!!!"},
		{"empty salt", "$argon2id$v=19$m=64,t=1,p=1$$" + parts[5]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := h.Verify(tt.encoded, "a perfectly fine password")
			if !errors.Is(err, ErrUnsupportedHash) {
				t.Fatalf("got %v, want ErrUnsupportedHash - an unparsable row must not be "+
					"reported as a wrong password", err)
			}
		})
	}
}

func TestVerifyRejectsAnOverlongPassword(t *testing.T) {
	t.Parallel()
	h := testHasher()

	encoded, err := h.Hash("a normal password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	// Far past the limit: this must be refused before any argon2 work happens.
	err = h.Verify(encoded, strings.Repeat("x", MaxPasswordLength+1))
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("got %v, want ErrInvalidCredentials", err)
	}
	if _, err := h.Hash(strings.Repeat("x", MaxPasswordLength+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("hash an overlong password: got %v, want ErrPasswordTooLong", err)
	}
}

func TestValidatePassword(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		password string
		want     error
	}{
		{"empty", "", ErrPasswordTooShort},
		{"one below the minimum", strings.Repeat("a", MinPasswordLength-1), ErrPasswordTooShort},
		{"exactly the minimum", strings.Repeat("a", MinPasswordLength), nil},
		{"long but allowed", strings.Repeat("a", MaxPasswordLength), nil},
		{"one above the maximum", strings.Repeat("a", MaxPasswordLength+1), ErrPasswordTooLong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidatePassword(tt.password); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestNeedsRehash(t *testing.T) {
	t.Parallel()

	weak := NewHasher(FastPasswordParams())
	strong := NewHasher(PasswordParams{Memory: 8192, Iterations: 3, Parallelism: 1, SaltLength: 16, KeyLength: 32})

	weakHash, err := weak.Hash("some password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	strongHash, err := strong.Hash("some password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if !strong.NeedsRehash(weakHash) {
		t.Fatal("a hash made with weaker parameters should be flagged for upgrade")
	}
	if strong.NeedsRehash(strongHash) {
		t.Fatal("a hash at the current parameters should not be flagged")
	}
	if !strong.NeedsRehash("garbage") {
		t.Fatal("an unparsable hash should be flagged for upgrade")
	}
	// The stronger hash still verifies under the weaker hasher: parameters
	// travel with the hash, so raising them does not lock anybody out.
	if err := weak.Verify(strongHash, "some password"); err != nil {
		t.Fatalf("verify a stronger hash with weaker settings: %v", err)
	}
}

func TestVerifyDummyDoesNotPanic(t *testing.T) {
	t.Parallel()
	// It has no observable result by design; what matters is that the login
	// path can always call it.
	testHasher().VerifyDummy("whatever the caller sent")
}
