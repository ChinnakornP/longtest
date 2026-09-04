package security

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ErrSandboxUnsupported is returned when the host cannot enforce the spec and
// the caller did not explicitly accept running without it.
var ErrSandboxUnsupported = errors.New("security: this platform cannot enforce the sandbox")

// stubArg is the argv[1] marker that turns a re-exec of our own binary into
// the sandbox stub. It is not a user-facing subcommand.
const stubArg = "__qa_sandbox_exec"

// stubSpecEnv carries the serialised spec to the stub.
const stubSpecEnv = "QA_SANDBOX_SPEC"

// NetworkPolicy says what a sandboxed child may reach.
type NetworkPolicy string

const (
	// NetworkNone gives the child an empty network namespace: no loopback,
	// no DNS, no route to anywhere. Used for phases that only transform files
	// the daemon already fetched.
	NetworkNone NetworkPolicy = "none"

	// NetworkProxy points the child at the egress proxy and nothing else. The
	// proxy is the enforcement point for [EgressPolicy]; the environment
	// variables set here only make a well-behaved client use it. See the
	// residual-risk note in docs/SECURITY.md.
	NetworkProxy NetworkPolicy = "proxy"

	// NetworkHost gives the child the daemon's own network. It is what the
	// browser needs to reach an app on the customer's LAN, and it is never
	// correct for an AI CLI.
	NetworkHost NetworkPolicy = "host"
)

// Limits bound what one child process can consume before the kernel stops it.
//
// These are not performance tuning. A hostile page can make a run fetch an
// endless response, and an AI CLI that has been talked into `while true; do
// mkdir x; done` should hit a wall inside its own workspace rather than fill
// the customer's disk.
type Limits struct {
	// CPUSeconds is RLIMIT_CPU: SIGKILL after this much CPU time. Distinct
	// from Wall, which also covers a process that is merely stuck.
	CPUSeconds uint64
	// AddressSpaceBytes is RLIMIT_AS.
	AddressSpaceBytes uint64
	// MaxFileBytes is RLIMIT_FSIZE: the largest single file the child may
	// write. Bounds a runaway log or a downloaded artifact.
	MaxFileBytes uint64
	// MaxProcs is RLIMIT_NPROC: fork-bomb ceiling. It counts every process
	// owned by the uid, so it must leave headroom for the daemon itself.
	MaxProcs uint64
	// MaxOpenFiles is RLIMIT_NOFILE.
	MaxOpenFiles uint64
	// Wall is the wall-clock deadline. Zero means the caller's context is the
	// only deadline.
	Wall time.Duration
}

// DefaultAgentLimits bound an AI CLI invocation.
//
// An agent run is a handful of HTTP round trips and some file writes. The
// numbers are generous enough that a legitimate long reasoning turn finishes
// and tight enough that nothing here fills a laptop.
func DefaultAgentLimits() Limits {
	return Limits{
		CPUSeconds:        300,
		AddressSpaceBytes: 4 << 30,
		MaxFileBytes:      256 << 20,
		MaxProcs:          256,
		MaxOpenFiles:      1024,
		Wall:              15 * time.Minute,
	}
}

// DefaultExecutorLimits bound the Playwright sidecar. Chromium is far hungrier
// than a CLI, and a trace for a long test case is a large single file.
func DefaultExecutorLimits() Limits {
	return Limits{
		CPUSeconds:        1800,
		AddressSpaceBytes: 8 << 30,
		MaxFileBytes:      2 << 30,
		MaxProcs:          1024,
		MaxOpenFiles:      4096,
		Wall:              30 * time.Minute,
	}
}

// Spec is a sandbox configuration. Build one per child process.
type Spec struct {
	// WorkspaceDir is the child's working directory, its $HOME, and the only
	// path it may write to. Required.
	WorkspaceDir string

	// ReadOnlyPaths are additional paths the child may read and execute:
	// system libraries, the CLI binary, and the provider's own credential
	// directory. Read-only is deliberate for that last one — the CLI needs to
	// read the token it was authenticated with, and must not be able to
	// rewrite the operator's config.
	ReadOnlyPaths []string

	// ReadWritePaths are paths outside the workspace the child may also write
	// to. It exists for exactly one category of thing — device files every
	// program assumes, /dev/null above all — and should never grow to include
	// a real directory.
	ReadWritePaths []string

	// Limits bound resource consumption.
	Limits Limits

	// Network selects the egress policy.
	Network NetworkPolicy

	// ProxyURL is used when Network is NetworkProxy.
	ProxyURL string

	// EnvAllow names environment variables inherited from the daemon. Nothing
	// else is inherited: the daemon's own process environment holds the
	// runtime pairing token and the artifact-store credentials, and an AI CLI
	// reading those is exactly the escalation this package exists to prevent.
	EnvAllow []string

	// EnvSet are variables set explicitly, overriding anything inherited.
	EnvSet map[string]string

	// SelfExe is the binary re-executed as the sandbox stub. Empty means
	// os.Executable. Tests set it to the test binary.
	SelfExe string

	// AllowUnsandboxed permits running on a host that cannot enforce the
	// spec. It exists so a developer on macOS can work on the pipeline; a
	// daemon that sets it logs a warning per run and reports it in `hello`.
	AllowUnsandboxed bool
}

// stubSpec is the wire form handed to the stub process. Keeping it separate
// from Spec makes it obvious that only these fields cross the exec boundary.
type stubSpec struct {
	WorkspaceDir   string        `json:"workspace_dir"`
	ReadOnlyPaths  []string      `json:"read_only_paths"`
	ReadWritePaths []string      `json:"read_write_paths"`
	Limits         Limits        `json:"limits"`
	Network        NetworkPolicy `json:"network"`
}

// DefaultReadOnlyPaths returns the system paths a CLI needs to start,
// filtered to the ones that exist on this host.
//
// It is a list, not "everything except the workspace": the point of the
// sandbox is that a compromised agent reading ~/.ssh, ~/.aws or another
// customer's run directory fails at the syscall.
func DefaultReadOnlyPaths() []string {
	candidates := []string{
		"/bin", "/sbin", "/lib", "/lib64", "/usr", "/opt",
		"/etc/ssl", "/etc/ca-certificates", "/etc/pki",
		"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf",
		"/etc/localtime", "/etc/passwd", "/etc/group",
		"/proc/self", "/dev/urandom", "/dev/random",
	}
	return existing(candidates)
}

// DefaultReadWritePaths returns the device files a sandboxed program needs to
// be able to write to. /dev/null is not optional: a shell redirect to it is in
// almost every command line, and denying it turns a working CLI into a
// permission error with no obvious cause.
func DefaultReadWritePaths() []string {
	return existing([]string{"/dev/null", "/dev/zero", "/dev/full"})
}

func existing(candidates []string) []string {
	out := make([]string, 0, len(candidates))
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// Command builds the sandboxed child.
//
// The returned *exec.Cmd runs a re-exec of this binary, which applies the
// limits to itself and only then execs name. Doing it in the child is what
// makes the limits inescapable: there is no window between "process exists"
// and "process is restricted" in which it could spawn something.
func (s Spec) Command(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	if s.WorkspaceDir == "" {
		return nil, errors.New("security: sandbox spec needs a workspace directory")
	}
	if s.Network == NetworkProxy && s.ProxyURL == "" {
		return nil, errors.New("security: NetworkProxy needs a ProxyURL")
	}
	if !sandboxSupported() && !s.AllowUnsandboxed {
		return nil, fmt.Errorf("%w: %s/%s; set AllowUnsandboxed to run anyway",
			ErrSandboxUnsupported, runtime.GOOS, runtime.GOARCH)
	}

	self := s.SelfExe
	if self == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("security: locate self: %w", err)
		}
		self = exe
	}

	ro := s.ReadOnlyPaths
	if ro == nil {
		ro = DefaultReadOnlyPaths()
	}
	rw := s.ReadWritePaths
	if rw == nil {
		rw = DefaultReadWritePaths()
	}
	payload, err := json.Marshal(stubSpec{
		WorkspaceDir:   s.WorkspaceDir,
		ReadOnlyPaths:  ro,
		ReadWritePaths: rw,
		Limits:         s.Limits,
		Network:        s.Network,
	})
	if err != nil {
		return nil, fmt.Errorf("security: encode sandbox spec: %w", err)
	}

	argv := append([]string{stubArg, "--", name}, args...)
	//nolint:gosec // self is our own executable and argv is built here, not
	// taken from a page or a model.
	cmd := exec.CommandContext(ctx, self, argv...)
	cmd.Dir = s.WorkspaceDir
	cmd.Env = append(s.env(), stubSpecEnv+"="+string(payload))
	applyProcAttr(cmd, s)

	if s.Limits.Wall > 0 {
		// WaitDelay bounds the gap between "we killed it" and "its pipes
		// closed": a child that ignores the signal must not hold the run open.
		cmd.WaitDelay = 10 * time.Second
	}
	return cmd, nil
}

// env builds the child environment from the allowlist plus explicit values.
func (s Spec) env() []string {
	out := map[string]string{
		// A sandboxed child's home is its workspace. Anything the CLI decides
		// to cache, write or scribble lands where it is allowed to write and
		// gets deleted with the run.
		"HOME":   s.WorkspaceDir,
		"TMPDIR": s.WorkspaceDir + "/tmp",
		"PWD":    s.WorkspaceDir,
		// Deny-by-default egress: with no proxy configured, a client that
		// honours these reaches nothing. NetworkNone additionally removes the
		// route in the kernel.
		"HTTP_PROXY":  "",
		"HTTPS_PROXY": "",
		"ALL_PROXY":   "",
		"NO_PROXY":    "",
	}
	for _, k := range s.EnvAllow {
		if v, ok := os.LookupEnv(k); ok {
			out[k] = v
		}
	}
	if s.Network == NetworkProxy {
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			out[k] = s.ProxyURL
		}
		// Empty NO_PROXY, explicitly: a non-empty one is a hole in the policy.
		out["NO_PROXY"] = ""
		out["no_proxy"] = ""
	}
	for k, v := range s.EnvSet {
		out[k] = v
	}

	env := make([]string, 0, len(out))
	for k, v := range out {
		env = append(env, k+"="+v)
	}
	sort.Strings(env) // deterministic, so a prompt-region test can compare
	return env
}

// BaseEnvAllow is the minimum a CLI needs to start. Providers extend it with
// their own variable — never with a wildcard.
func BaseEnvAllow() []string {
	return []string{"PATH", "LANG", "LC_ALL", "TZ", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR"}
}

// RunSandboxStub applies the spec to the current process and execs the target.
// It never returns on success.
//
// Call it from main before any other work:
//
//	if security.IsSandboxStub() {
//	    security.RunSandboxStub()
//	}
func RunSandboxStub() {
	if err := runStub(); err != nil {
		fmt.Fprintln(os.Stderr, "qa-sandbox:", err)
		// 126 is the shell's "found but could not execute" code, which is
		// what a sandbox refusal is.
		os.Exit(126)
	}
}

// IsSandboxStub reports whether this process was started as a sandbox stub.
func IsSandboxStub() bool {
	return len(os.Args) > 1 && os.Args[1] == stubArg
}

func runStub() error {
	rest := os.Args[2:]
	if len(rest) < 2 || rest[0] != "--" {
		return errors.New("stub invoked without a command")
	}
	argv := rest[1:]

	raw, ok := os.LookupEnv(stubSpecEnv)
	if !ok {
		return errors.New("stub invoked without " + stubSpecEnv)
	}
	var spec stubSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return fmt.Errorf("decode %s: %w", stubSpecEnv, err)
	}

	if err := applyStub(spec); err != nil {
		return err
	}

	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, stubSpecEnv+"=") {
			continue // do not leak the spec into the sandboxed program
		}
		env = append(env, kv)
	}

	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("look up %q: %w", argv[0], err)
	}
	return execve(path, argv, env)
}
