# qa-daemon

The runtime agent. It runs on a machine inside the network that hosts the
application under test, dials the backend over **one outbound WebSocket**, and
drives the Playwright sidecar and an AI CLI on the operator's behalf.

```
backend  <--- one outbound wss ---  qa-daemon  ---> qa-executor (Node) ---> Chromium
                                        |
                                        +--------> claude / opencode / agy
                                        |
                                        +--------> S3 / MinIO (presigned PUT)
```

The daemon **opens no inbound port** (ADR-002). That is the product's whole
pitch: it can test `http://localhost:3000`, `http://192.168.1.20` and
`staging.internal` without anyone exposing them to the internet.

## Commands

```bash
qa-daemon pair --code K7Q2-9FMR-3XT8 --server https://qa.example.com [--name laptop]
qa-daemon start [--verbose] [--quiet]
qa-daemon status [--output json]
qa-daemon doctor [--output json]
qa-daemon version
```

- **pair** redeems a one-time code for this machine's runtime token and writes
  the config. The token is never printed: it is an organization-wide
  control-plane credential, and a terminal scrollback is not a vault.
- **start** connects and executes assigned runs until it is signalled.
- **status** reads the published state file — there is no local port to query —
  and reports a daemon whose process is gone as `stopped`, never as `online`.
- **doctor** answers "why can this machine not run a test?" and exits non-zero
  when something is broken, so a provisioning script can gate on it.

## Files

| Path | What |
| --- | --- |
| `$XDG_CONFIG_HOME/qa-daemon/config.json` | server URL, runtime id, runtime token. **Mode 0600, enforced**: a looser mode is refused, not repaired. |
| `$XDG_STATE_HOME/qa-daemon/state.json` | what `status` reads |
| `$XDG_STATE_HOME/qa-daemon/daemon.log` | structured JSON log |
| `$XDG_DATA_HOME/qa-daemon/workspaces/{projectId}/{runId}/{phase}/` | per-run workspace, `phase ∈ discovery, planning, execution, analysis` |

Environment overrides (for containers, where writing a token to a volume is
worse than passing one in): `QA_DAEMON_CONFIG`, `QA_DAEMON_SERVER`,
`QA_DAEMON_TOKEN`, `QA_DAEMON_RUNTIME_ID`, `QA_DAEMON_WORKSPACE_ROOT`,
`QA_DAEMON_EXECUTOR`, `QA_DAEMON_CHROMIUM`.

## What the daemon guarantees

- **Reconnect, don't restart.** A lost connection is a normal state. Runs keep
  going, the frames they produce queue in an outbox, and reconnection uses
  exponential backoff with full jitter capped at 15s — so a backend restart is
  not answered by every daemon in the organization at the same millisecond.
- **A run executes at most once.** `run.assign` is at-least-once. An assignment
  for a run already in flight is ignored; one for a run that already finished
  is answered by repeating the result. A per-run ledger on disk covers the
  remaining case — a daemon restarted mid-run — so a test case that already
  created an employee does not create a second one.
- **Sequence numbers never restart.** The backend deduplicates on
  `(runId, seq)`, so a counter that restarted at zero after a reconnect would
  have its new events silently discarded as duplicates.
- **Cancel kills the tree.** Children are started in their own process group
  and cancelled with SIGTERM then SIGKILL on the group, so a sidecar that
  forked Chromium does not leave it behind. The budget is five seconds; the
  end-to-end test measures it against a fixture that ignores SIGTERM.
- **Evidence bypasses the API.** Screenshots, traces, HAR and console logs are
  PUT straight to object storage with the presigned credentials that came down
  with the assignment, and only keys travel back in `run.result`. A key outside
  the run's own prefix is refused: that prefix is the tenant boundary.
- **Documents are forwarded, not rewritten.** An application map, a test plan
  or a finding is validated against its contract and passed on as the bytes
  that were validated.

## What is not here yet

- The AI CLI provider itself (T10). The daemon runs the phase, hands the
  provider a workspace directory and a schema id, and validates what comes
  back; with no provider configured an agent phase fails with
  `agent_not_available` rather than quietly doing nothing.
- Discovery, planning and failure-analysis prompts (T13–T15).
- Daemon auto-update.
