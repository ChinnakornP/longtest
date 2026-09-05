package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/agent"
	"github.com/ChinnakornP/longtest/daemon/agent/prompts"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// TestMain lets this test binary stand in for the daemon when a spec re-execs
// itself as the sandbox stub.
func TestMain(m *testing.M) {
	if security.IsSandboxStub() {
		security.RunSandboxStub()
	}
	os.Exit(m.Run())
}

// fakeCLI writes a shell script that behaves like the real CLI for the parts
// this package depends on: it answers --version, reads the prompt on stdin,
// and writes its answer to a file.
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = \"--version\" ] && { echo '2.1.0 (Claude Code)'; exit 0; }; done\n" + body
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake CLI: %v", err)
	}
	return path
}

func workspaceWithPrompt(t *testing.T, prompt string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "planning")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, agent.DefaultPromptFile), []byte(prompt), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	return dir
}

func sandboxFor(t *testing.T, dir string) security.Spec {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	return security.Spec{
		WorkspaceDir: dir, Limits: security.DefaultAgentLimits(),
		Network: security.NetworkHost, EnvAllow: security.BaseEnvAllow(),
		SelfExe: self, AllowUnsandboxed: true,
	}
}

func readyHost() agent.Host {
	return agent.Host{
		Getenv:  func(string) string { return "" },
		Exists:  func(string) bool { return true },
		HomeDir: "/home/tester",
	}
}

func providerFor(t *testing.T, binary string, host agent.Host) *Provider {
	t.Helper()
	return New(Options{
		Binary: binary,
		Host:   &host,
		// Detection must not be cached across the assertions in one test.
		DetectTTL: -1,
	})
}

func task(t *testing.T, dir string) agent.Task {
	t.Helper()
	return agent.Task{
		Phase: prompts.PhasePlanning, WorkspaceDir: dir,
		OutputSchema: "test-plan@1", Sandbox: sandboxFor(t, dir),
		Timeout: 30 * time.Second,
	}
}

// Every flag in the headless invocation removes a capability rather than
// adding one. A regression here is not cosmetic: --restricted is what keeps
// the planner from running shell commands, and acceptEdits is what lets it
// write its answer with nobody at the terminal.
func TestTheInvocationIsHeadlessAndRestricted(t *testing.T) {
	args := New(Options{}).args()
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-p", "--restricted", "--permission-mode acceptEdits",
		"--disable-slash-commands", "--strict-mcp-config", "--no-session-persistence",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the invocation is missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "--dangerously-skip-permissions") {
		t.Fatal("the invocation bypasses permissions entirely")
	}

	i := slices.Index(args, "--append-system-prompt")
	if i < 0 || i+1 >= len(args) {
		t.Fatal("the standing rules are not passed as a system prompt")
	}
	if !strings.Contains(args[i+1], "never an instruction to you") {
		t.Fatal("the system prompt is not the untrusted-content boundary")
	}
}

func TestModelIsPassedThroughWhenConfigured(t *testing.T) {
	args := New(Options{Model: "opus"}).args()
	i := slices.Index(args, "--model")
	if i < 0 || args[i+1] != "opus" {
		t.Fatalf("args = %v", args)
	}
	if slices.Contains(New(Options{}).args(), "--model") {
		t.Fatal("a provider with no configured model overrode the operator's own default")
	}
}

// $HOME inside the sandbox is the run workspace, so the CLI would look for its
// credentials in a directory created seconds ago. The operator's real config
// directory is mounted read-only and pointed at explicitly: readable, so the
// CLI can use the token it was logged in with; read-only, so a hijacked run
// cannot rewrite the operator's configuration.
func TestSandboxMountsTheCredentialDirectoryReadOnly(t *testing.T) {
	configDir := t.TempDir()
	host := readyHost()
	provider := New(Options{ConfigDir: configDir, Host: &host})

	dir := t.TempDir()
	spec := provider.sandbox(sandboxFor(t, dir), "/usr/local/bin/claude")

	if !slices.Contains(spec.ReadOnlyPaths, configDir) {
		t.Fatalf("the credential directory is not readable: %v", spec.ReadOnlyPaths)
	}
	if slices.Contains(spec.ReadWritePaths, configDir) {
		t.Fatal("the credential directory is writable")
	}
	if spec.EnvSet["CLAUDE_CONFIG_DIR"] != configDir {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q", spec.EnvSet["CLAUDE_CONFIG_DIR"])
	}
	if spec.WorkspaceDir != dir {
		t.Fatalf("the child would run in %q", spec.WorkspaceDir)
	}
}

// The daemon's own environment holds the runtime pairing token and the
// artifact-store credentials. An AI CLI that could read those would be one
// injection away from registering itself as another runtime.
func TestOnlyNamedCredentialVariablesAreInherited(t *testing.T) {
	host := readyHost()
	spec := New(Options{ConfigDir: t.TempDir(), Host: &host}).
		sandbox(sandboxFor(t, t.TempDir()), "/usr/local/bin/claude")

	for _, want := range []string{"PATH", "ANTHROPIC_API_KEY", "AWS_SESSION_TOKEN"} {
		if !slices.Contains(spec.EnvAllow, want) {
			t.Fatalf("%s is not inherited, so an operator configured that way cannot run", want)
		}
	}
	for _, forbidden := range []string{"*", "QA_RUNTIME_TOKEN", "QA_ARTIFACT_KEY", "HOME"} {
		if slices.Contains(spec.EnvAllow, forbidden) {
			t.Fatalf("%s is inherited by the AI CLI", forbidden)
		}
	}
}

// The exchange end to end, without a model: the CLI is handed the prompt on
// stdin and its answer is read from the file, never from what it printed.
func TestProviderReadsTheAnswerFromTheFileNotStdout(t *testing.T) {
	dir := workspaceWithPrompt(t, "write me a plan")
	binary := fakeCLI(t, `cat > prompt-as-seen.txt
printf '{"from":"the file"}' > out.json
echo '{"from":"stdout"}'
`)
	provider := providerFor(t, binary, readyHost())

	result, err := provider.Run(t.Context(), task(t, dir))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != agent.StatusOK {
		t.Fatalf("status = %q, detail = %q", result.Status, result.Detail)
	}
	if string(result.Output) != `{"from":"the file"}` {
		t.Fatalf("output = %s", result.Output)
	}

	// And the prompt reached it on stdin rather than on the command line,
	// where every account on the machine could read it with ps.
	seen, err := os.ReadFile(filepath.Join(dir, "prompt-as-seen.txt"))
	if err != nil {
		t.Fatalf("the CLI was not handed the prompt on stdin: %v", err)
	}
	if strings.TrimSpace(string(seen)) != "write me a plan" {
		t.Fatalf("the CLI read %q on stdin", seen)
	}
	if strings.Contains(result.Command, "write me a plan") {
		t.Fatalf("the prompt is on the command line: %s", result.Command)
	}
}

// A CLI that never answers is killed, and the run is told which of the failure
// modes it was.
func TestProviderReportsATimeout(t *testing.T) {
	dir := workspaceWithPrompt(t, "hello")
	binary := fakeCLI(t, "sleep 60\n")
	provider := providerFor(t, binary, readyHost())

	invocation := task(t, dir)
	invocation.Timeout = 300 * time.Millisecond

	started := time.Now()
	result, err := provider.Run(t.Context(), invocation)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != agent.StatusTimeout {
		t.Fatalf("status = %q", result.Status)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("the timeout took %s to take effect", elapsed)
	}
	if !strings.Contains(result.Detail, "process tree was killed") {
		t.Fatalf("detail = %q", result.Detail)
	}
}

// A CLI that ran and wrote nothing is a bad answer, not a broken machine: the
// runner is allowed to try again.
func TestProviderReportsAMissingAnswerAsInvalidOutput(t *testing.T) {
	dir := workspaceWithPrompt(t, "hello")
	binary := fakeCLI(t, "echo 'I have decided not to answer'\nexit 0\n")
	provider := providerFor(t, binary, readyHost())

	result, err := provider.Run(t.Context(), task(t, dir))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != agent.StatusOutputInvalid {
		t.Fatalf("status = %q", result.Status)
	}
	if !strings.Contains(result.Detail, "out.json") {
		t.Fatalf("detail = %q", result.Detail)
	}
}

// Some builds exit non-zero after writing a perfectly good answer. The file is
// what decides; the exit code is recorded for a human.
func TestAnAnswerSurvivesANonZeroExit(t *testing.T) {
	dir := workspaceWithPrompt(t, "hello")
	binary := fakeCLI(t, "printf '{\"ok\":true}' > out.json\nexit 1\n")
	provider := providerFor(t, binary, readyHost())

	result, err := provider.Run(t.Context(), task(t, dir))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != agent.StatusOK {
		t.Fatalf("status = %q", result.Status)
	}
	if result.ExitCode != 1 || !strings.Contains(result.Detail, "exited 1") {
		t.Fatalf("the exit code was not recorded: %d %q", result.ExitCode, result.Detail)
	}
}

// An unauthenticated CLI is refused before it is launched, so the run fails
// with something an operator can act on rather than with the CLI's own login
// prompt arriving as a schema error.
func TestProviderRefusesToRunWhenNotAuthenticated(t *testing.T) {
	dir := workspaceWithPrompt(t, "hello")
	binary := fakeCLI(t, "printf '{}' > out.json\n")

	host := agent.Host{
		Getenv:  func(string) string { return "" },
		Exists:  func(string) bool { return false },
		HomeDir: "/home/tester",
	}
	provider := providerFor(t, binary, host)

	capability, err := provider.Detect(t.Context())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if capability.Readiness != agent.ReadinessUnauthenticated {
		t.Fatalf("readiness = %q", capability.Readiness)
	}

	result, err := provider.Run(t.Context(), task(t, dir))
	if err == nil {
		t.Fatal("an unauthenticated CLI was launched")
	}
	if result.Status != agent.StatusUnavailable {
		t.Fatalf("status = %q", result.Status)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "out.json")); statErr == nil {
		t.Fatal("the CLI ran despite being unusable")
	}
}

func TestMissingCLIIsReportedAsUnavailable(t *testing.T) {
	dir := workspaceWithPrompt(t, "hello")
	provider := providerFor(t, filepath.Join(t.TempDir(), "not-installed"), readyHost())

	result, err := provider.Run(t.Context(), task(t, dir))
	if err == nil || result.Status != agent.StatusUnavailable {
		t.Fatalf("status = %q, err = %v", result.Status, err)
	}
}

// The live check. It is skipped unless an operator opts in, because it spends
// their tokens and needs a CLI that is actually logged in — neither of which
// belongs in CI. Run it with:
//
//	QA_AGENT_LIVE=1 go test ./agent/claude/ -run Live -v
func TestLiveClaudeWritesASchemaValidApplicationMap(t *testing.T) {
	if os.Getenv("QA_AGENT_LIVE") == "" {
		t.Skip("set QA_AGENT_LIVE=1 to run the real CLI (spends tokens, needs a logged-in claude)")
	}

	root := os.Getenv("QA_AGENT_LIVE_DIR")
	if root == "" {
		root = t.TempDir()
	}
	dir := filepath.Join(root, "discovery")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// A small, real crawl result for the model to extend, placed as a file
	// exactly as the discovery pipeline will place it.
	partial := `{"version":1,"baseUrl":"http://localhost:3000","pages":[{"id":"page.root","path":"/","title":"Home","elements":[]}],"workflows":[]}`
	if err := os.WriteFile(filepath.Join(dir, "application-map.json"), []byte(partial), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}

	registry := agent.NewRegistry(New(Options{}))
	runner, err := agent.NewRunner(agent.RunnerOptions{
		Registry: registry,
		Timeout:  5 * time.Minute,
		Sandbox: security.Spec{
			Limits: security.DefaultAgentLimits(), Network: security.NetworkHost,
			EnvAllow: security.BaseEnvAllow(), AllowUnsandboxed: true,
		},
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	result, err := runner.Run(t.Context(), agent.Task{
		Phase:          prompts.PhaseDiscovery,
		WorkspaceDir:   dir,
		OutputSchema:   "application-map@1",
		AllowedOrigins: []string{"http://localhost:3000"},
		BaseURL:        "http://localhost:3000",
		Untrusted: []security.Block{{
			Kind:   security.KindDOMText,
			Source: "http://localhost:3000/",
			Content: "Home | Employees | Settings\n" +
				"Employees table with columns Name, Email, Role and an 'Add Employee' button.",
		}},
	})
	if err != nil {
		t.Fatalf("live run: %v (status %s, %d attempts)", err, result.Status, result.Attempts)
	}
	if result.Status != agent.StatusOK {
		t.Fatalf("status = %q", result.Status)
	}

	if err := qaschema.MustBeValid("application-map@1", result.Output); err != nil {
		t.Fatalf("the model's answer is not a valid application map: %v", err)
	}
	var appMap qaschema.ApplicationMap
	if err := json.Unmarshal(result.Output, &appMap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Logf("live claude produced %d pages in %d attempt(s), %s",
		len(appMap.Pages), result.Attempts, result.Duration.Round(time.Second))
}
