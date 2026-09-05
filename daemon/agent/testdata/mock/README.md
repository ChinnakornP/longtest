Canned answers for `agent.MockProvider`.

One file per phase, named after the `prompts.Phase`. Each is a real document
from `packages/qa-schema/fixtures`, so a test that uses them is asserting
against the same contracts every other component is held to — a fixture that
drifts from the schema fails the fixture suite, not just the test that reads it.

Add `{phase}.attempt-{n}.json` to script a different answer per attempt, and
`{phase}.attempt-{n}.status` holding a status word (`agent_timeout`,
`agent_unavailable`) to script a failure that did not really happen.
