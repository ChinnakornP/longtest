package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// PasswordParams are the argon2id cost parameters.
//
// They are written into every hash, so changing them here does not invalidate
// existing passwords: verification always uses the parameters stored with the
// hash it is checking.
type PasswordParams struct {
	// Memory in KiB. The dominant cost, and what makes a GPU attack expensive.
	Memory uint32
	// Iterations (argon2's "time" parameter).
	Iterations uint32
	// Parallelism is the number of lanes.
	Parallelism uint8
	// SaltLength in bytes.
	SaltLength uint32
	// KeyLength of the derived key, in bytes.
	KeyLength uint32
}

// DefaultPasswordParams follows the OWASP argon2id recommendation
// (m=19456 KiB, t=2, p=1), which is the lowest-memory configuration on their
// list and therefore the one that keeps a small backend responsive under a
// burst of logins. A login costs roughly 20 MiB and a few milliseconds.
func DefaultPasswordParams() PasswordParams {
	return PasswordParams{
		Memory:      19 * 1024,
		Iterations:  2,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// FastPasswordParams is for tests only. A table-driven test that hashes twenty
// passwords at production cost spends seconds doing it; correctness of the
// encoding and the comparison does not depend on the cost.
func FastPasswordParams() PasswordParams {
	return PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
}

const (
	// MinPasswordLength matches cmd/seed: a length rule is the only password
	// policy here, because composition rules push users towards
	// "Password1!" without adding entropy.
	MinPasswordLength = 12
	// MaxPasswordLength bounds the work an unauthenticated caller can ask for.
	// argon2 cost is dominated by memory, not input length, but there is no
	// reason to hash a megabyte.
	MaxPasswordLength = 256

	argon2Algorithm = "argon2id"

	// maxHashComponentBytes bounds the salt and the key decoded out of a
	// stored hash. Nothing legitimate is anywhere near this; the point is that
	// a corrupted row cannot make argon2 allocate an absurd key.
	maxHashComponentBytes = 1024
)

// Password errors. ErrInvalidCredentials is deliberately shared between "no
// such user" and "wrong password": telling them apart is a user-enumeration
// oracle on an unauthenticated endpoint.
var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrPasswordTooShort and ErrPasswordTooLong are validation failures, not
	// authentication failures; they only ever come from signup.
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d characters", MaxPasswordLength)
	// ErrUnsupportedHash means the stored hash is not one this build can
	// verify (a different algorithm, or a corrupted row).
	ErrUnsupportedHash = errors.New("unsupported password hash")
)

// Hasher hashes and verifies passwords with argon2id.
//
// The zero value is not usable; use NewHasher.
type Hasher struct {
	params PasswordParams
	// dummy is a real hash of a random secret, computed once, used to spend
	// the same work on a login for an e-mail that does not exist. Without it,
	// "unknown user" returns in microseconds and "wrong password" in
	// milliseconds, which is a usable enumeration oracle.
	dummy func() string
}

// NewHasher returns a Hasher with the given cost parameters.
func NewHasher(params PasswordParams) *Hasher {
	h := &Hasher{params: params}
	h.dummy = sync.OnceValue(func() string {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			// crypto/rand failing is not a recoverable condition, but this
			// value is only used to burn time, so a fixed fallback is better
			// than taking the process down on a login.
			secret = []byte("dummy-verification-target-not-a-credential")
		}
		encoded, err := h.Hash(base64.RawStdEncoding.EncodeToString(secret))
		if err != nil {
			return ""
		}
		return encoded
	})
	return h
}

// ValidatePassword applies the length policy. It is called by signup, not by
// login: an existing password that no longer meets the policy must still be
// able to sign in and change it.
func ValidatePassword(password string) error {
	switch {
	case len(password) < MinPasswordLength:
		return ErrPasswordTooShort
	case len(password) > MaxPasswordLength:
		return ErrPasswordTooLong
	default:
		return nil
	}
}

// Hash returns the PHC-encoded argon2id hash of password:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>
//
// The parameters travel with the hash so they can be raised later without a
// migration.
func (h *Hasher) Hash(password string) (string, error) {
	if len(password) > MaxPasswordLength {
		return "", ErrPasswordTooLong
	}

	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt,
		h.params.Iterations, h.params.Memory, h.params.Parallelism, h.params.KeyLength)

	return fmt.Sprintf("$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2Algorithm, argon2.Version,
		h.params.Memory, h.params.Iterations, h.params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether password matches encoded.
//
// It returns ErrInvalidCredentials for a mismatch and ErrUnsupportedHash for a
// stored value it cannot parse, so a corrupted row is not silently reported as
// a wrong password.
func (h *Hasher) Verify(encoded, password string) error {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return err
	}
	if len(password) > MaxPasswordLength {
		// Refuse before spending the memory; a caller cannot have a valid
		// password this long because Hash rejects one too.
		return ErrInvalidCredentials
	}

	got := argon2.IDKey([]byte(password), salt,
		params.Iterations, params.Memory, params.Parallelism, byteLen(want))

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrInvalidCredentials
	}
	return nil
}

// VerifyDummy spends the same work as Verify against a throwaway hash. Login
// calls it when the e-mail is unknown so that both outcomes take comparable
// time.
func (h *Hasher) VerifyDummy(password string) {
	if encoded := h.dummy(); encoded != "" {
		_ = h.Verify(encoded, password)
	}
}

// NeedsRehash reports whether a stored hash was produced with weaker
// parameters than the current ones, so login can transparently upgrade it.
func (h *Hasher) NeedsRehash(encoded string) bool {
	params, _, key, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return params.Memory < h.params.Memory ||
		params.Iterations < h.params.Iterations ||
		byteLen(key) < h.params.KeyLength
}

// decodeHash parses the PHC string produced by Hash.
func decodeHash(encoded string) (PasswordParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", algorithm, version, params, salt, key
	if len(parts) != 6 || parts[0] != "" {
		return PasswordParams{}, nil, nil, fmt.Errorf("%w: malformed", ErrUnsupportedHash)
	}
	if parts[1] != argon2Algorithm {
		return PasswordParams{}, nil, nil, fmt.Errorf("%w: %q", ErrUnsupportedHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return PasswordParams{}, nil, nil, fmt.Errorf("%w: unreadable version", ErrUnsupportedHash)
	}
	if version != argon2.Version {
		return PasswordParams{}, nil, nil, fmt.Errorf("%w: argon2 version %d", ErrUnsupportedHash, version)
	}

	var params PasswordParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&params.Memory, &params.Iterations, &params.Parallelism); err != nil {
		return PasswordParams{}, nil, nil, fmt.Errorf("%w: unreadable parameters", ErrUnsupportedHash)
	}
	if params.Memory == 0 || params.Iterations == 0 || params.Parallelism == 0 {
		return PasswordParams{}, nil, nil, fmt.Errorf("%w: zero cost parameter", ErrUnsupportedHash)
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) == 0 || len(salt) > maxHashComponentBytes {
		return PasswordParams{}, nil, nil, fmt.Errorf("%w: unreadable salt", ErrUnsupportedHash)
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(key) == 0 || len(key) > maxHashComponentBytes {
		return PasswordParams{}, nil, nil, fmt.Errorf("%w: unreadable key", ErrUnsupportedHash)
	}

	params.SaltLength = byteLen(salt)
	params.KeyLength = byteLen(key)
	return params, salt, key, nil
}

// byteLen is len() as the uint32 the argon2 API takes. decodeHash rejects
// anything longer than maxHashComponentBytes before this is reached, so the
// conversion cannot truncate.
func byteLen(b []byte) uint32 {
	if len(b) > maxHashComponentBytes {
		return maxHashComponentBytes
	}
	return uint32(len(b)) //nolint:gosec // bounded by maxHashComponentBytes above
}
