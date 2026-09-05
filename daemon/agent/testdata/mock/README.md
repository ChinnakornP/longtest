Canned answers for `agent.MockProvider`.

One file per phase, named after the `prompts.Phase`. Each is a real document
from `packages/qa-schema/fixtures`, so a test that uses them is asserting
against the same contracts every other component is held to — a fixture that
drifts from the schema fails the fixture suite, not just the test that reads it.

Add `{phase}.attempt-{n}.json` to script a different answer per attempt, and
`{phase}.attempt-{n}.status` holding a status word (`agent_timeout`,
`agent_unavailable`) to script a failure that did not really happen.

`analysis.json` is the one answer that cannot be a free-standing fixture. The
analysis phase is gated by `analysis.Context` (daemon/analysis/review.go), which
checks a finding's citations against the artifacts the execution really
produced — so this file names the case the mock plan produces (`TC-900`) and an
artifact id the test that drives it arranges to exist. A canned analysis that
cites evidence no run has is rejected three times and fails the phase, which is
the gate working, not the fixture being stale.
