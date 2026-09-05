package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ErrNoSuchFixture is returned for a fixture name that is not registered.
var ErrNoSuchFixture = errors.New("security: no such fixture")

// ErrFixtureKeyMissing is returned when the store has no key to open its
// ciphertext with.
var ErrFixtureKeyMissing = errors.New("security: fixture encryption key is not configured")

// FixtureKeyEnv names the environment variable holding the store key,
// base64-standard-encoded, 32 bytes.
const FixtureKeyEnv = "QA_FIXTURE_KEY"

// Credential is one target-application login.
//
// It has no MarshalJSON: the zero value of "make it hard to serialise by
// accident" is having no encoder at all. Anything that needs to persist a
// credential goes through [FixtureStore.Seal].
type Credential struct {
	Username string
	Password string
	// TOTPSecret is optional and treated exactly like Password.
	TOTPSecret string
}

// secrets returns the values a [Scrubber] must know about.
func (c Credential) secrets() []string {
	out := make([]string, 0, 3)
	for _, v := range []string{c.Password, c.TOTPSecret, c.Username} {
		if len(v) >= MinSecretLen {
			out = append(out, v)
		}
	}
	return out
}

// FixtureStore holds the target application's credentials for a run.
//
// The rule the whole design serves: a credential is referenced by name
// everywhere a model, a prompt, a workspace file, an event or an artifact can
// see it, and its value exists only here and in the executor at the moment it
// is typed into a form. The planner is told the *names* — that is what lets it
// write `preconditions: ["fixture:logged_in_as_admin"]` — and nothing else.
//
// At rest the values are AES-256-GCM sealed under a key that lives in the
// operator's environment or secret manager, never in the workspace and never
// in the database next to the ciphertext.
type FixtureStore struct {
	key   []byte
	creds map[string]Credential
}

// NewFixtureStore builds an empty store. key must be 32 bytes; a nil key means
// the store can serve credentials it was given in memory but cannot Seal or
// Open ciphertext.
func NewFixtureStore(key []byte) (*FixtureStore, error) {
	if key != nil && len(key) != 32 {
		return nil, fmt.Errorf("security: fixture key must be 32 bytes, got %d", len(key))
	}
	return &FixtureStore{key: key, creds: map[string]Credential{}}, nil
}

// FixtureKeyFromEnv reads and decodes [FixtureKeyEnv].
func FixtureKeyFromEnv() ([]byte, error) {
	raw, ok := os.LookupEnv(FixtureKeyEnv)
	if !ok || raw == "" {
		return nil, ErrFixtureKeyMissing
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("security: decode %s: %w", FixtureKeyEnv, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("security: %s decodes to %d bytes, want 32", FixtureKeyEnv, len(key))
	}
	return key, nil
}

// NewFixtureKey mints a key for an operator to store in their secret manager.
func NewFixtureKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("security: generate fixture key: %w", err)
	}
	return key, nil
}

// fixtureName is the shape a precondition may reference. It matches the
// Precondition pattern in test-case.schema.json, so a name that is storable
// here is a name a planner can legally emit.
func validFixtureName(name string) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("security: fixture name %q must be 1-64 characters", name)
	}
	if name[0] < 'a' || name[0] > 'z' {
		return fmt.Errorf("security: fixture name %q must start with a lowercase letter", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return fmt.Errorf("security: fixture name %q may only contain [a-z0-9_]", name)
		}
	}
	return nil
}

// Set registers a credential under a name.
func (s *FixtureStore) Set(name string, c Credential) error {
	if err := validFixtureName(name); err != nil {
		return err
	}
	s.creds[name] = c
	return nil
}

// Names lists the registered fixture names, sorted.
//
// This is the entire surface the planner is given. It is a deliberate design
// point that this method returns []string and not []Credential.
func (s *FixtureStore) Names() []string {
	out := make([]string, 0, len(s.creds))
	for k := range s.creds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// KnownFixtures returns the set a [PlanGate] validates preconditions against.
func (s *FixtureStore) KnownFixtures() map[string]struct{} {
	out := make(map[string]struct{}, len(s.creds))
	for k := range s.creds {
		out[k] = struct{}{}
	}
	return out
}

// Resolve returns a credential by name. Only the executor calls it, at the
// moment it fills a login form.
func (s *FixtureStore) Resolve(name string) (Credential, error) {
	name = strings.TrimPrefix(name, "fixture:")
	c, ok := s.creds[name]
	if !ok {
		return Credential{}, fmt.Errorf("%w: %q", ErrNoSuchFixture, name)
	}
	return c, nil
}

// RegisterWith teaches a scrubber every value this store holds.
//
// Call it once, at run start, before anything else in the run can produce
// output. A scrubber that learns a credential after the first log line has
// already been written is not a control.
func (s *FixtureStore) RegisterWith(sc *Scrubber) error {
	for name, c := range s.creds {
		for _, v := range c.secrets() {
			if err := sc.Add(v); err != nil {
				return fmt.Errorf("security: register fixture %q: %w", name, err)
			}
		}
	}
	return nil
}

// sealed is the on-disk / in-database form.
type sealed struct {
	Version int    `json:"v"`
	Nonce   []byte `json:"n"`
	Cipher  []byte `json:"c"`
}

// Seal encrypts the whole store under the configured key.
//
// The output is what the backend persists per project. It is opaque to the
// backend: the daemon holds the key, so a database dump is not a set of
// customer logins.
func (s *FixtureStore) Seal() ([]byte, error) {
	if s.key == nil {
		return nil, ErrFixtureKeyMissing
	}
	// G117 flags marshalling a struct with a field named Password. That is
	// what this function is for: the bytes never leave it unencrypted, and the
	// next three statements seal them under AES-256-GCM.
	plain, err := json.Marshal(s.creds) //nolint:gosec // sealed below, see comment
	if err != nil {
		return nil, fmt.Errorf("security: encode fixtures: %w", err)
	}
	gcm, err := newGCM(s.key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("security: fixture nonce: %w", err)
	}
	return json.Marshal(sealed{Version: 1, Nonce: nonce, Cipher: gcm.Seal(nil, nonce, plain, nil)})
}

// Open decrypts a store previously produced by Seal.
func (s *FixtureStore) Open(blob []byte) error {
	if s.key == nil {
		return ErrFixtureKeyMissing
	}
	var env sealed
	if err := json.Unmarshal(blob, &env); err != nil {
		return fmt.Errorf("security: decode sealed fixtures: %w", err)
	}
	if env.Version != 1 {
		return fmt.Errorf("security: sealed fixtures version %d is unsupported", env.Version)
	}
	gcm, err := newGCM(s.key)
	if err != nil {
		return err
	}
	if len(env.Nonce) != gcm.NonceSize() {
		return errors.New("security: sealed fixtures have a malformed nonce")
	}
	plain, err := gcm.Open(nil, env.Nonce, env.Cipher, nil)
	if err != nil {
		// Deliberately vague: distinguishing "wrong key" from "tampered
		// ciphertext" tells an attacker which of the two they achieved.
		return errors.New("security: sealed fixtures could not be opened")
	}
	creds := map[string]Credential{}
	if err := json.Unmarshal(plain, &creds); err != nil {
		return fmt.Errorf("security: decode fixtures: %w", err)
	}
	for name := range creds {
		if err := validFixtureName(name); err != nil {
			return err
		}
	}
	s.creds = creds
	return nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("security: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("security: gcm: %w", err)
	}
	return gcm, nil
}
