package security_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/security"
)

func TestFixtureStoreExposesNamesNotValues(t *testing.T) {
	s, err := security.NewFixtureStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("logged_in_as_admin", security.Credential{
		Username: fakeUser, Password: fakePassword,
	}); err != nil {
		t.Fatal(err)
	}
	names := s.Names()
	if len(names) != 1 || names[0] != "logged_in_as_admin" {
		t.Fatalf("unexpected names %v", names)
	}
	// The planner is handed Names(); nothing in that value may be a secret.
	for _, n := range names {
		if strings.Contains(n, fakePassword) || strings.Contains(n, fakeUser) {
			t.Fatalf("a fixture name carries a credential: %q", n)
		}
	}
}

func TestFixtureNamesMatchThePreconditionPattern(t *testing.T) {
	s, _ := security.NewFixtureStore(nil)
	// The schema's Precondition pattern is ^fixture:[a-z][a-z0-9_]{0,63}$, so a
	// name that cannot be referenced is not a name worth storing.
	for _, bad := range []string{"", "Admin", "1admin", "admin-user", "admin.user", strings.Repeat("a", 65)} {
		if err := s.Set(bad, security.Credential{}); err == nil {
			t.Errorf("fixture name %q should have been rejected", bad)
		}
	}
	if err := s.Set("logged_in_as_admin2", security.Credential{}); err != nil {
		t.Errorf("a valid name was rejected: %v", err)
	}
}

func TestFixtureResolveAcceptsThePrefixedForm(t *testing.T) {
	s, _ := security.NewFixtureStore(nil)
	if err := s.Set("logged_in_as_admin", security.Credential{Password: fakePassword}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Resolve("fixture:logged_in_as_admin")
	if err != nil || c.Password != fakePassword {
		t.Fatalf("resolve: %v / %q", err, c.Password)
	}
	if _, err := s.Resolve("fixture:logged_in_as_root"); !errors.Is(err, security.ErrNoSuchFixture) {
		t.Fatalf("expected ErrNoSuchFixture, got %v", err)
	}
}

func TestFixtureSealRoundTrip(t *testing.T) {
	key, err := security.NewFixtureKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := security.NewFixtureStore(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("logged_in_as_admin", security.Credential{
		Username: fakeUser, Password: fakePassword, TOTPSecret: fakeTOTP,
	}); err != nil {
		t.Fatal(err)
	}
	blob, err := s.Seal()
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// The backend stores this. A database dump must not be a set of logins.
	for _, secret := range []string{fakePassword, fakeUser, fakeTOTP} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("a secret is readable in the sealed blob: %s", blob)
		}
	}

	reopened, err := security.NewFixtureStore(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Open(blob); err != nil {
		t.Fatalf("open: %v", err)
	}
	c, err := reopened.Resolve("logged_in_as_admin")
	if err != nil {
		t.Fatal(err)
	}
	if c.Password != fakePassword || c.TOTPSecret != fakeTOTP {
		t.Fatal("round trip lost a value")
	}
}

func TestFixtureSealIsAuthenticated(t *testing.T) {
	key, _ := security.NewFixtureKey()
	s, _ := security.NewFixtureStore(key)
	if err := s.Set("logged_in_as_admin", security.Credential{Password: fakePassword}); err != nil {
		t.Fatal(err)
	}
	blob, err := s.Seal()
	if err != nil {
		t.Fatal(err)
	}

	// Flip a byte of the ciphertext: GCM must refuse it rather than return
	// garbage a caller might type into a login form.
	tampered := make([]byte, len(blob))
	copy(tampered, blob)
	i := strings.Index(string(tampered), `"c":"`) + 6
	tampered[i] ^= 0x01

	other, _ := security.NewFixtureStore(key)
	if err := other.Open(tampered); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}

	// And a wrong key fails the same way, without saying which it was.
	wrongKey, _ := security.NewFixtureKey()
	wrong, _ := security.NewFixtureStore(wrongKey)
	err = wrong.Open(blob)
	if err == nil {
		t.Fatal("a wrong key opened the store")
	}
	if strings.Contains(err.Error(), "key") {
		t.Fatalf("the error distinguishes a wrong key from tampering: %v", err)
	}
}

func TestFixtureStoreNeedsAKeyToSeal(t *testing.T) {
	s, _ := security.NewFixtureStore(nil)
	if _, err := s.Seal(); !errors.Is(err, security.ErrFixtureKeyMissing) {
		t.Fatalf("expected ErrFixtureKeyMissing, got %v", err)
	}
	if _, err := security.NewFixtureStore([]byte("too short")); err == nil {
		t.Fatal("a short key was accepted")
	}
}

func TestFixtureKeyFromEnv(t *testing.T) {
	t.Setenv(security.FixtureKeyEnv, "")
	if _, err := security.FixtureKeyFromEnv(); !errors.Is(err, security.ErrFixtureKeyMissing) {
		t.Fatalf("expected ErrFixtureKeyMissing, got %v", err)
	}
	t.Setenv(security.FixtureKeyEnv, "not-base64!!")
	if _, err := security.FixtureKeyFromEnv(); err == nil {
		t.Fatal("invalid base64 was accepted")
	}
}

func TestFixtureRegisterWithTeachesEveryValue(t *testing.T) {
	s, _ := security.NewFixtureStore(nil)
	if err := s.Set("logged_in_as_admin", security.Credential{
		Username: fakeUser, Password: fakePassword, TOTPSecret: fakeTOTP,
	}); err != nil {
		t.Fatal(err)
	}
	sc := security.NewScrubber()
	if err := s.RegisterWith(sc); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{fakeUser, fakePassword, fakeTOTP} {
		if !sc.Contains("prefix " + v + " suffix") {
			t.Errorf("the scrubber does not know %q", v)
		}
	}
}
