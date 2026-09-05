//go:build !windows

package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Adopt is how the sandbox and this package compose: security.Spec builds an
// exec.Cmd whose SysProcAttr carries clone flags that must be requested at
// fork time, and rebuilding it from Options would silently drop them.
func TestAdoptKeepsTheCallersProcessAttributes(t *testing.T) {
	//nolint:noctx // Terminate is this package's cancellation path; a
	// CommandContext here would signal only the direct child, which is the
	// behaviour Adopt exists to replace.
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	attr := &syscall.SysProcAttr{}
	cmd.SysProcAttr = attr

	child, err := Adopt(cmd)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	// The same struct, added to rather than replaced: everything the sandbox
	// put in it — clone flags, uid mappings, the parent-death signal — has to
	// survive being adopted.
	if cmd.SysProcAttr != attr {
		t.Fatal("the caller's process attributes were replaced wholesale")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("the adopted child did not get its own process group")
	}

	var exit *exec.ExitError
	if err := child.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("wait = %v", err)
	}
}

func TestAdoptTerminatesTheWholeTree(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "survived")

	//nolint:noctx // Terminate is this package's cancellation path; a
	// CommandContext here would signal only the direct child, which is the
	// behaviour Adopt exists to replace.
	cmd := exec.Command("/bin/sh", "-c", "(sleep 3; touch "+marker+") & sleep 60")
	child, err := Adopt(cmd)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if err := child.Terminate(context.Background(), 200*time.Millisecond); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a grandchild outlived the tree it was in")
	}
}

func TestAdoptRefusesNil(t *testing.T) {
	if _, err := Adopt(nil); err == nil {
		t.Fatal("Adopt(nil) was accepted")
	}
}
