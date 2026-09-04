# Security

This document states what the platform defends against, what it deliberately
does not, and how to report a vulnerability. It describes the controls that
exist in the repository today; anything not built yet is listed under
[Known gaps](#known-gaps) rather than described as if it were.

Last reviewed: 2026-09-04 (LONG-14).

## Reporting a vulnerability

Open a **private security advisory** on the repository:
<https://github.com/ChinnakornP/longtest/security/advisories/new>.

Please do not open a public issue or a pull request for a vulnerability — a
public report is a disclosure, and this repository is public.

Include what you did, what happened, and what you expected. A proof of concept
against a local `make up` stack is ideal; do not test against anyone else's
deployment.

We will acknowledge within 5 working days and aim to have a fix or a stated
decision within 30 days. There is no bounty programme.

## What this product is

An AI agent that points a browser at a web application, works out what the
application does, writes test cases, runs them, and explains the failures.

That means the data flow is:

```
untrusted website  ->  browser  ->  daemon  ->  AI CLI  ->  model provider
                                      |
                                      +->  backend  ->  database, object store
```

The AI CLI is a program on the customer's own machine, already authenticated
to a model provider, that can normally read files and run commands. The
website is one nobody has vetted. **Everything below exists because of that
sentence.**

## Trust boundaries

| # | Boundary | Crossing | Trust of what crosses |
| - | -------- | -------- | --------------------- |
| 1 | Application under test → browser | HTML, text, a11y tree, console, network, downloads, filenames | **Untrusted.** Assume the page is trying to hijack the agent. |
| 2 | Browser → daemon | Page-derived bytes, evidence | Untrusted; framed and bounded before it goes further. |
| 3 | Daemon → AI CLI | A prompt and workspace files | Instructions are ours; observations are untrusted and framed as such. |
| 4 | AI CLI → daemon | `out.json` | **Untrusted output.** Schema-validated, then gated. |
| 5 | Daemon → backend | WebSocket frames, artifact keys | Authenticated by runtime token; scrubbed. |
| 6 | Daemon → object store | Artifact bodies over presigned PUT | Scoped to one run's key prefix (ADR-002). |
| 7 | Browser/user → backend | Session cookie, `X-Org-ID` | Authenticated and org-scoped (ADR-006, ADR-007). |

The one that matters most is #1, and the design position on it is stated in
[ADR-003](adr/0003-ai-cli-file-contract.md): the workspace is the agent's
blast radius, and page text copied into it is data, never instructions.

## The controls

All of these live in `daemon/security` unless noted. Each has tests; the test
names are given so a reviewer can check the claim rather than take it.

### 1. Prompt-injection boundary

Page-derived bytes reach a model only inside a frame produced by
`security.Wrap`:

```
<<<UNTRUSTED_PAGE_CONTENT id="<run nonce>" kind="dom_text" source="https://..." bytes=812 truncated=false>>>
...content...
<<<END_UNTRUSTED_PAGE_CONTENT id="<run nonce>">>>
```

- The `id` is a 128-bit per-run nonce the page cannot observe, so a page that
  guesses the delimiter still cannot forge a frame. The system prompt tells the
  model that a block whose ids do not match the task's is not a real frame.
- Both markers are stripped from the payload case-insensitively, along with the
  attribute tail up to a nearby `>>>`.
- The `source` and `kind` attributes are attacker-controlled, so they are
  emitted JSON-quoted with `>` escaped — a crafted source cannot terminate the
  header or start a new line.
- ANSI escape sequences and C0/C1 controls are removed: a CLI that echoes a
  prompt renders it to the operator's terminal.
- Invisible Unicode is removed — bidi overrides, zero-width characters,
  variation selectors, and the tag-character block (U+E0000–U+E007F), which is
  a complete invisible ASCII alphabet.
- Each block is capped at 16 KiB, truncated on a rune boundary, with the
  original size and the fact of truncation stated inside the frame.

`daemon/agent/prompts` is the only way to build a prompt, and its templates
have no hole for raw content. Rendering is deterministic, which is what makes
the corpus assertion below possible.

The same framing is implemented in TypeScript for the executor
(`daemon/executor/src/untrusted.ts`) and held to byte equality with the Go
implementation by 27 shared vectors — `daemon/security/parity_test.go` and
`daemon/executor/test/untrusted.test.ts` read the same file. Two framings that
differ teach a model that the framing is negotiable.

### 2. The injection corpus

`e2e/injection-corpus/corpus.json` holds 18 cases across 9 channels: visible
text, HTML comments, `aria-label`, image `alt`, page title, console output,
JSON API responses, downloaded file bodies, and attacker-chosen filenames —
plus delimiter forgery, mixed-case delimiter forgery, invisible Unicode, ANSI
escapes, a Markdown-image exfiltration channel, a fabricated fixture name, a
subdomain pivot, a raw-locator smuggle, and a volume attack.

`daemon/security/injection_corpus_test.go` runs every case in CI and asserts:

1. **Page content cannot reach the instruction region of a prompt.** The
   prompt is rendered and everything outside the frames is compared byte for
   byte against a prompt built from benign content
   (`TestCorpusCannotReachTheInstructionRegion`).
2. **The plan the injection wanted does not run.** Each case carries the plan a
   fully compromised planner would emit; the gate must reject it for the stated
   rule (`TestCorpusHijackedPlansAreRejected`).

Plus frame forgery, sanitisation and size bounds.

**What the corpus does not prove.** It does not run a model, so it is not
evidence that any particular model ignored any particular injection. That
depends on a model version nobody here controls, and a green CI run built on a
lucky sample would read as a guarantee it is not. The live half —
`e2e/injection-corpus/src/server.ts`, a real browser, a real CLI — is opt-in
and is not a merge gate.

### 3. The plan gate

`security.PlanGate` validates model output *before* anything is executed, on
the explicit assumption that the boundary above can fail. A control whose
failure mode is "the model was persuaded" is not a control.

Every rule is a property of the JSON, checkable without knowing what the test
is meant to do:

| Rule | Refuses |
| --- | --- |
| `egress_not_allowed` | a `navigate` target off the allowlist, after resolving relative and protocol-relative URLs |
| `credential_in_plan` | a literal run credential anywhere in the plan |
| `unknown_fixture` | a precondition naming a fixture this run cannot establish |
| `precondition_not_a_fixture_ref` | a prose precondition |
| `unstable_locator_not_flagged` | a raw locator not marked `unstable: true` |
| `plan_too_large` | a plan that would hammer the customer's own staging environment |
| `unknown_action` | an action outside the frozen v1 vocabulary |
| `value_looks_like_an_instruction` | a step value that reads as a command (heuristic; the only rule here that can produce a false positive) |
| `wrong_contract_version` | a plan claiming a different contract |

It reports every violation rather than the first, because a plan is fixed by
asking the model again and a retry loop that fixes one problem per round is
expensive.

### 4. Filesystem confinement

Two layers, because the daemon and the CLI need different mechanisms.

- **`security.Workspace`** wraps `os.Root`. Path resolution is done by the
  kernel with `openat2` semantics, so `..`, an absolute path, and a symlink the
  agent planted all fail at the syscall — and keep failing if a directory is
  swapped for a symlink between the check and the open. A path-prefix string
  comparison cannot make that last guarantee.
  Tests: `TestWorkspaceRefusesToEscape`, `TestWorkspaceRefusesASymlinkedDirectory`.
- **Landlock** (`daemon/security/landlock_linux.go`, raw syscalls — the daemon
  module carries no dependencies) confines child processes. The run's workspace
  is read-write; a small system list is read-execute; everything else is
  unreachable, including `~/.ssh`, `~/.aws`, the daemon's own config and any
  other run's workspace.
  Tests: `TestSandboxConfinesReadsToTheWorkspace`,
  `TestSandboxConfinesWritesToTheWorkspace`,
  `TestSandboxRefusesToFollowASymlinkOut`.

The provider's own credential directory (`~/.claude` and equivalents) is
granted **read-only**: the CLI needs the token it authenticated with, and must
not be able to rewrite the operator's config.

### 5. Process sandbox

`security.Spec` builds every child process. Restrictions are applied in the
child, after `fork` and before `exec`, so there is no window in which the
process exists unrestricted.

- `no_new_privs`, so a confined agent cannot escape through `sudo` or a setuid
  helper.
- `RLIMIT_CPU`, `RLIMIT_AS`, `RLIMIT_FSIZE`, `RLIMIT_NOFILE`, and a wall-clock
  deadline. `RLIMIT_CORE` is 0: a core dump of the AI CLI contains the model
  credentials it authenticated with and whatever page content was in memory,
  and `core_pattern` writes it somewhere the run cannot scrub.
- `RLIMIT_NPROC` as *headroom above current usage*. The kernel charges it per
  task across the whole uid, so an absolute ceiling makes `fork` fail on any
  real workstation. See [Known gaps](#known-gaps).
- Own process group plus `Pdeathsig`, so a wall-clock kill takes the whole tree
  and an orphaned Chromium does not outlive the daemon.
- **The daemon's environment is not inherited.** Only an explicit allowlist
  crosses (`PATH`, `LANG`, `TZ`, TLS bundle paths, plus whatever a provider
  names). The daemon's own environment holds the runtime pairing token and the
  artifact-store credentials.
  Test: `TestSandboxDoesNotInheritTheDaemonEnvironment`.
- `$HOME` and `$TMPDIR` are the run's workspace, so anything the CLI caches
  lands where it is allowed to write and is deleted with the run.

### 6. Egress control

`security.EgressPolicy` is deny-by-default. An empty allowlist denies
everything, which is the correct behaviour for a misconfigured run.

- The allowlist is **exact by default**: `demo.example.test` does not imply
  `uploads.demo.example.test`. A sibling subdomain serving user content is the
  normal case, not the exotic one, and it is exactly where an injected
  instruction gets somewhere useful.
- `file:`, `javascript:`, `data:` and everything else non-HTTP are refused: a
  page that can choose the scheme picks one of those.
- URLs with embedded credentials (`https://user:pass@host/`) are refused.
- Private, loopback and link-local addresses require an explicit opt-in, so a
  policy does not silently permit `169.254.169.254`. The opt-in exists because
  testing an app on the customer's own LAN is the product's pitch.

### 7. Credential handling

Target-application credentials are referenced by name — `fixture:logged_in_as_admin`
— everywhere a model, a prompt, a workspace file, an event or an artifact can
see them. The planner is given `FixtureStore.Names()` and nothing else.

- At rest they are AES-256-GCM sealed under a key from `QA_FIXTURE_KEY`, which
  the daemon holds and the backend does not. A database dump of the ciphertext
  is not a set of customer logins.
- `security.Scrubber` removes them from everything on the way out, in every
  encoding they travel in: raw, URL-escaped, JSON-escaped, and base64 at all
  three phase alignments (which is how a credential survives inside a Basic
  auth header). The replacement carries a truncated SHA-256 so two occurrences
  are recognisably the same credential in a log without disclosing it.
- `security.RunGuard` is the single choke point: prompt, workspace file, run
  log, `run.event`, artifact body. Prompts are scrubbed by
  `prompts.Build` itself, because the prompt string is handed to the CLI
  directly — scrubbing on the way to disk would leave the copy the model reads
  intact.
- `RunGuard.Verify` re-scans the workspace as a backstop and fails the run if
  it finds anything, catching a future call site that wrote a file around the
  guard.

Tests: `TestNothingLeaksACredentialAcrossAWholeRun` seeds a fixture with a fake
credential, drives a prompt, a workspace JSON file, a chunked run log, an event
and two artifacts, then scans all of them plus the whole workspace.
`TestVerifyDetectsAFileWrittenAroundTheGuard` proves the scan is not vacuous.

### 8. Secrets in the repository and CI

- No secret is committed. `.env.example` ships placeholders; a real `.env` is
  git-ignored and CI fails if one is tracked.
- Gitleaks runs on every push and pull request (`security` job) with `--redact`
  so a hit is not printed into a public log.
- Third-party GitHub Actions are pinned to a commit SHA, not a tag.
- `permissions: contents: read` at workflow level.
- pnpm refuses to resolve a package version published in the last 24 hours
  (`minimumReleaseAge`), which is the window in which compromised-maintainer
  npm attacks are caught and yanked.

### 9. Session cookies

The session cookie is `__Host-qa_session` whenever its preconditions hold
(Secure, `Path=/`, no `Domain`), and plain `qa_session` otherwise. The name is
**derived** from those attributes in one place (`auth.SessionCookieName`), not
configured alongside them: a `__Host-` cookie that is not Secure is silently
discarded by the browser, and "login does nothing, nothing in any log" is the
worst failure mode to debug. The prefix is the only thing that stops a
subdomain of the deployment from overwriting the main origin's session cookie.

## What this system does NOT guarantee

Stated plainly, because a security document that only lists wins is marketing.

1. **It does not guarantee a model ignores an injection.** The framing lowers
   the odds; the plan gate is what bounds the damage when it fails. If your
   threat model requires that a model never be persuaded, this product does not
   meet it and neither does any other.
2. **It does not defend the application under test.** The agent fills forms and
   clicks buttons. Point it at production and it will create, edit and delete
   real data. Human-in-the-loop for irreversible actions is a **known gap**
   (see below) — today the protection is that you chose the target.
3. **It does not sandbox the browser itself.** Chromium runs with its own
   sandbox under the daemon's user. A Chromium renderer escape is a compromise
   of the daemon's account.
4. **It does not scrub binary artifacts.** Screenshots and `trace.zip` are
   pixels and a zip; substring replacement cannot scrub them. A screenshot of a
   password field with the value revealed will contain the password.
5. **It does not protect against a hostile operator.** Anyone who can configure
   a run can point it at anything the daemon's network reaches, within the
   egress allowlist they also configure.
6. **It does not protect the model provider's context.** Page content, framed
   and bounded, still reaches a third-party model. Do not point this at an
   application whose *content* is confidential.
7. **It is not audited.** Nothing here has had an external review or a
   penetration test. The controls are described so you can judge them, not so
   you can skip judging them.
8. **Non-Linux runtimes are not sandboxed.** See below.

## Known gaps

Tracked, not hidden. Each is a follow-up rather than something quietly absent.

| Gap | Impact | Status |
| --- | --- | --- |
| No human-in-the-loop approval for irreversible actions (payment, delete, permission change, e-mail send) | The agent can perform a destructive action on the target app without asking | The plan gate has no `requires_approval` rule yet; needs a UI to approve into. Follow-up issue. |
| Egress is enforced in-process, not by a proxy | `NetworkProxy` sets `HTTP(S)_PROXY`, which a well-behaved client honours and a compromised one ignores. Only `NetworkNone` is kernel-enforced. | Needs a deny-by-default egress proxy container and a network namespace that routes only to it. Follow-up issue. |
| `RLIMIT_NPROC` is approximate | Two concurrent runs raise each other's baseline, so the fork ceiling is softer than stated | A precise limit needs a cgroup with `pids.max` and cgroup delegation the daemon cannot assume on a customer laptop. |
| No sandbox on macOS or Windows | A run on such a runtime has no filesystem confinement and no rlimits | `Spec.AllowUnsandboxed` must be set explicitly and is reported in `hello`, so the platform can refuse to schedule on it. `sandbox_init` is deprecated and undocumented; a container is the likely answer. |
| Binary artifacts are not scrubbed | Credentials can survive in a screenshot or a trace | Needs redaction at capture time (mask the field before the screenshot), not after. |
| No rate limit on the pairing-code redeem endpoint | Denial of service against runtime pairing | Noted during the T05 review. Not a code-guessing risk: 60 bits, 15-minute TTL, stored hashed. |
| The daemon has no supply-chain attestation | A tampered daemon binary is undetectable to the backend | Needs signed releases and a checked signature at pairing time. |

## Reviewing a change to this area

If a change touches any of the following, it needs a second reader:

- `daemon/security/**`, `daemon/agent/prompts/**`
- `e2e/injection-corpus/corpus.json` — removing a case is a reduction in
  coverage and should say why
- the `security` job in `.github/workflows/ci.yml`
- anything that constructs an `exec.Cmd` in the daemon, or writes a file
  outside `security.Workspace`

Adding an entry to `EgressPolicy`'s allowlist, widening `Spec.ReadOnlyPaths`,
or adding a variable to `Spec.EnvAllow` are all quiet ways to remove a control.
