# daemon/agent — driving an AI CLI through files

The platform holds no LLM API key. It drives whichever coding CLI the operator
already installed and authenticated, and it talks to that CLI the only way that
survives a vendor changing its output format overnight (ADR-003):

```
{phase}/                       the run's phase directory — the agent's blast radius
  contract/test-plan-v1.json   the schema the answer must match, placed as a file
  application-map.json         phase inputs, placed as files, never inlined
  prompt.md                    written by the runner, read by the CLI on stdin
  out.json                     written by the CLI, read back by the runner
  agent/
    attempt-1/{prompt.md,out.json,stdout.log,stderr.log,meta.json}
    attempt-2/…
```

Nothing parses stdout. `stdout.log` is kept because it is the first thing a
human wants when a run goes wrong, not because anything reads it.

## The three pieces

| | |
|---|---|
| `Detect` / `DetectAll` | what this machine has: **missing**, **unauthenticated**, **ready** |
| `Provider` | one CLI's launch recipe — `claude`, `opencode`, `antigravity`, `MockProvider` |
| `Runner` | the phase loop: render → launch → validate → retry → record |

A provider owns *only* how its CLI is invoked. Prompt rendering belongs to
`agent/prompts`, which is the one place page-derived bytes may enter a prompt;
launching goes through a `security.Spec`, which is what confines the child to
the workspace. A provider that built its own prompt string or its own
`exec.Cmd` would have removed both.

## Retries

An answer that does not match its contract is retried at most twice
(`DefaultMaxAttempts` = 3). The validator's report goes back to the model as a
framed `kind="agent_output"` block, never as instructions: it quotes the
document the model wrote, and on a hijacked first attempt that is page content
in the model's own voice.

A timeout, a missing CLI or a launch failure is **not** retried. The second
attempt would fail the same way and spend the same wall clock.

The output is never repaired by hand. Three prompts and three answers in the
workspace and an honest `agent_output_invalid` is worth more than a document we
edited into shape, which is no longer the document the model wrote.

## Adding a provider

1. Implement `agent.Provider` in `daemon/agent/<cli>/`.
2. Add the binary, install hint, login hint and auth probe to `Known` in
   `detect.go`.
3. Extend the sandbox in the provider's own `Run`: its credential directory
   read-only, its own environment variables by name. Never a wildcard — the
   daemon's environment holds the runtime pairing token.
4. Register it in `newAgentRunner` in `cmd/qa-daemon/start.go`.

## Testing without a model

`MockProvider` reads canned answers from `testdata/mock/` and goes through the
same file exchange a real CLI does, so every integration test downstream of
this package runs with no LLM, no cost and a deterministic answer:

```go
runner, _ := agent.NewRunner(agent.RunnerOptions{
    Registry: agent.NewRegistry(agent.NewMockProvider(agent.MockOptions{Dir: "…/testdata/mock"})),
})
deps.Agent = runtime.NewAgentRunner(runner)
```

Script a different answer per attempt with `{phase}.attempt-{n}.json`, and a
failure that did not really happen with `{phase}.attempt-{n}.status`.

## Checks

```sh
cd daemon && go test ./agent/...            # everything below, no model involved
cd daemon && go test ./runtime/ -run Mock   # a full run driven by MockProvider

# The live check. Spends tokens and needs a logged-in claude, so it is opt-in
# and never runs in CI. QA_AGENT_LIVE_DIR keeps the workspace for inspection.
QA_AGENT_LIVE=1 go test ./agent/claude/ -run Live -v -timeout 20m
```
