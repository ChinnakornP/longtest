package security_test

import (
	"os"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/security"
)

// TestMain lets the test binary stand in for the daemon binary when a test
// builds a sandbox spec. Spec.Command re-execs SelfExe with a marker argv, so
// pointing SelfExe at the test binary and dispatching here exercises exactly
// the code path the daemon will run in production — including the part that
// applies the restrictions in the child rather than the parent.
func TestMain(m *testing.M) {
	if security.IsSandboxStub() {
		security.RunSandboxStub()
	}
	os.Exit(m.Run())
}
