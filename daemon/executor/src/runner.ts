/**
 * @fileoverview Run one test case end-to-end.
 *
 * The orchestrator. It owns the retry policy, the evidence capture schedule,
 * the fixture establishment, and the per-step / per-assertion dispatch. It
 * returns an `ExecutionResult` that has already been validated against the
 * schema, or it throws an `ExecutorError` whose `code` is the wire-stable
 * error code the daemon needs.
 */

import { mkdir } from 'node:fs/promises';
import type {
  ApplicationMap,
  ExecutionResult,
  StepAction,
  TestCase,
  Step,
  Assertion,
} from '@qa/schema';
import type { Session } from './session.ts';
import { indexApplicationMap } from './locator.ts';
import { runStep } from './actions.ts';
import { runAssertion } from './assertions.ts';
import { ResultBuilder, emptyStepResult, emptyAssertionResult, knownArtifactIds } from './result.ts';
import { EvidenceCollector } from './evidence.ts';
import { establishFixtures, FixtureUnavailableError } from './fixtures.ts';

/** Wire-stable error code; mirrors `RpcErrorCode` in `protocol.ts`. */
export type RunnerErrorCode =
  | 'TARGET_NOT_FOUND'
  | 'UNKNOWN_ACTION'
  | 'UNKNOWN_ASSERTION_TYPE'
  | 'FIXTURE_UNAVAILABLE'
  | 'BROWSER_LAUNCH_FAILED'
  | 'TIMEOUT'
  | 'NETWORK_ERROR'
  | 'CANCELLED'
  | 'INTERNAL';

export class ExecutorError extends Error {
  readonly code: RunnerErrorCode;
  readonly stepIndex?: number;
  readonly data?: Record<string, unknown>;
  constructor(code: RunnerErrorCode, message: string, opts: { stepIndex?: number; data?: Record<string, unknown> } = {}) {
    super(message);
    this.code = code;
    if (opts.stepIndex !== undefined) this.stepIndex = opts.stepIndex;
    if (opts.data !== undefined) this.data = opts.data;
  }
}

export interface RunOptions {
  testCase: TestCase;
  appMap: ApplicationMap;
  artifactDir: string;
  storageKeyPrefix: string;
  runId?: string;
  stepTimeoutMs?: number;
  assertionTimeoutMs?: number;
  fixtureCredentials: Record<string, { username: string; password: string }>;
}

const KNOWN_STEP_ACTIONS: readonly StepAction[] = [
  'navigate', 'click', 'fill', 'select', 'check', 'hover', 'press', 'waitFor', 'screenshot',
];

export async function runTestCase(session: Session, opts: RunOptions): Promise<ExecutionResult> {
  const startedAt = new Date();
  const builder = new ResultBuilder({
    testCaseId: opts.testCase.id,
    ...(opts.runId !== undefined ? { runId: opts.runId } : {}),
    startedAt,
  });

  // Pre-flight: reject unknown actions before we spend any time in the
  // browser. The schema validator should have caught this already, but a
  // schema-valid test case with a typo in a single step still has to fail
  // here rather than silently skip the bad step.
  for (let i = 0; i < opts.testCase.steps.length; i += 1) {
    const step = opts.testCase.steps[i];
    if (step === undefined) continue;
    if (!KNOWN_STEP_ACTIONS.includes(step.action)) {
      throw new ExecutorError('UNKNOWN_ACTION', `unknown action "${step.action}" at step ${i}`, {
        stepIndex: i,
        data: { action: step.action, allowed: KNOWN_STEP_ACTIONS },
      });
    }
  }

  await mkdir(opts.artifactDir, { recursive: true });
  const evidence = new EvidenceCollector({
    artifactDir: opts.artifactDir,
    storageKeyPrefix: opts.storageKeyPrefix,
    testCaseId: opts.testCase.id,
    tracePath: session.traceDir,
  });
  await evidence.init();

  const appMapIndex = indexApplicationMap(opts.appMap);
  const page = session.getPage();

  // Wire evidence capture for everything that happens after this point.
  // We attach before fixture establishment so the login navigation's
  // evidence belongs to the test case itself.
  const statusByRequest = new WeakMap<import('playwright').Request, number | undefined>();
  page.on('request', (req) => {
    void evidence.recordNetwork(req).then(() => {
      // recordNetwork awaits response(); copy the resolved status into a
      // side-table so the assertion runtime can check synchronously.
      const last = evidence.networkSnapshot().at(-1);
      if (last !== undefined) statusByRequest.set(req, last.status);
    });
  });
  page.on('console', (msg) => evidence.recordConsole(msg));

  // Establish preconditions. A fixture that the executor does not know
  // about is `FIXTURE_UNAVAILABLE`: skip the whole case (the planner
  // asked us to run something we cannot establish) and record `result:
  // skipped` with `failureClass: ENVIRONMENT_ERROR`.
  try {
    await establishFixtures(opts.testCase.preconditions, page, {
      baseUrl: session.baseUrl,
      credentials: opts.fixtureCredentials,
    });
  } catch (error) {
    if (error instanceof FixtureUnavailableError) {
      const endedAt = new Date();
      builder.end(endedAt);
      return builder.finalise({
        message: error.message,
        failureClass: 'ENVIRONMENT_ERROR',
      });
    }
    throw error;
  }

  // Per-step retry policy. Default is `{ retries: 1 }` per the contract.
  const maxRetries = 1;
  const retryMinMs = 100;
  const retryMaxMs = 1000;

  let lastError: ExecutorError | undefined;
  for (let attempt = 1; attempt <= maxRetries + 1; attempt += 1) {
    try {
      await executeSteps(opts.testCase.steps, opts, session, builder, evidence, appMapIndex, statusByRequest);
      lastError = undefined;
      break;
    } catch (error) {
      if (!(error instanceof ExecutorError)) throw error;
      lastError = error;
      if (error.code === 'TARGET_NOT_FOUND' && attempt <= maxRetries) {
        await sleep(backoff(attempt, retryMinMs, retryMaxMs));
        continue;
      }
      break;
    }
  }

  // Capture final evidence, in priority order. Final screenshot first so a
  // pass still produces one (the contract requires at least one screenshot
  // for every case). Failure screenshot if we bailed on a step or
  // assertion.
  try {
    await evidence.captureScreenshot(page, { name: 'final' });
  } catch {
    // best-effort
  }
  if (lastError !== undefined) {
    try {
      const failureShotId = await evidence.captureScreenshot(page, { name: 'failure' });
      const last = builder.snapshot().steps.at(-1);
      if (last !== undefined) {
        last.artifactIds = [...(last.artifactIds ?? []), failureShotId];
      }
    } catch {
      // best-effort
    }
  }
  // Stop Playwright tracing so trace.zip is on disk before we register it.
  const tracePath = await session.finalizeTrace();
  evidence.setTracePath(tracePath);
  try {
    await evidence.writeNetworkArtifact();
    await evidence.writeConsoleArtifact();
  } catch {
    // best-effort
  }
  const traceId = await evidence.registerTraceArtifact();
  for (const artifact of evidence.artifactsList()) {
    builder.addArtifact(artifact);
  }
  void traceId;

  const endedAt = new Date();
  builder.end(endedAt);

  let message: string | undefined;
  let failureClass: ExecutionResult['failureClass'];
  if (lastError !== undefined) {
    message = lastError.message;
    if (lastError.code === 'TARGET_NOT_FOUND') failureClass = 'TEST_BUG';
    else if (lastError.code === 'TIMEOUT') failureClass = 'TIMEOUT';
    else if (lastError.code === 'NETWORK_ERROR') failureClass = 'NETWORK_ERROR';
    else if (lastError.code === 'FIXTURE_UNAVAILABLE') failureClass = 'ENVIRONMENT_ERROR';
    else if (lastError.code === 'UNKNOWN_ACTION' || lastError.code === 'UNKNOWN_ASSERTION_TYPE') failureClass = 'TEST_BUG';
    else failureClass = 'ENVIRONMENT_ERROR';
  }

  // Validate every step/assertion references a real artifact id before
  // finalising. This is the post-condition the schema cannot enforce: the
  // ArtifactId regex only guarantees shape, not referential integrity.
  const known = knownArtifactIds(builder.snapshot().artifacts);
  for (const step of builder.snapshot().steps) {
    for (const id of step.artifactIds ?? []) {
      if (!known.has(id)) {
        throw new ExecutorError('INTERNAL', `step ${step.index} references unknown artifact id "${id}"`, {
          stepIndex: step.index,
        });
      }
    }
  }
  for (const assertion of builder.snapshot().assertions) {
    for (const id of assertion.artifactIds ?? []) {
      if (!known.has(id)) {
        throw new ExecutorError('INTERNAL', `assertion ${assertion.index} references unknown artifact id "${id}"`, {
          stepIndex: assertion.index,
        });
      }
    }
  }

  return builder.finalise({
    ...(failureClass !== undefined ? { failureClass } : {}),
    ...(message !== undefined ? { message } : {}),
  });
}

async function executeSteps(
  steps: readonly Step[],
  opts: RunOptions,
  session: Session,
  builder: ResultBuilder,
  evidence: EvidenceCollector,
  appMapIndex: ReturnType<typeof indexApplicationMap>,
  statusByRequest: WeakMap<import('playwright').Request, number | undefined>,
): Promise<void> {
  const page = session.getPage();
  for (let i = 0; i < steps.length; i += 1) {
    const step = steps[i];
    if (step === undefined) continue;
    const stepStartedAt = new Date();
    const result = await runStep(step, {
      page,
      appMap: appMapIndex,
      baseUrl: session.baseUrl,
      stepTimeoutMs: opts.stepTimeoutMs ?? 15_000,
      defaultStepTimeoutMs: 15_000,
    });
    const stepEndedAt = new Date();

    if (result.kind === 'unresolved') {
      let shotId: string | undefined;
      try {
        shotId = await evidence.captureScreenshot(page, { name: `step-${i}-failure` });
      } catch {
        shotId = undefined;
      }
      const stepResult = emptyStepResult(i, step.action, 'error');
      stepResult.message = `target not found for step ${i}; tried: ${result.unresolved.tried.join(', ')}`;
      stepResult.startedAt = stepStartedAt.toISOString();
      stepResult.endedAt = stepEndedAt.toISOString();
      stepResult.durationMs = Math.max(0, stepEndedAt.getTime() - stepStartedAt.getTime());
      stepResult.unstableTarget = result.unresolved.unstable;
      if (shotId !== undefined) stepResult.artifactIds = [shotId];
      builder.addStep(stepResult);
      throw new ExecutorError('TARGET_NOT_FOUND', stepResult.message ?? 'target not found', {
        stepIndex: i,
        data: { tried: result.unresolved.tried, unstable: result.unresolved.unstable },
      });
    }

    const stepResult = emptyStepResult(i, step.action, 'pass');
    stepResult.startedAt = stepStartedAt.toISOString();
    stepResult.endedAt = stepEndedAt.toISOString();
    stepResult.durationMs = Math.max(0, stepEndedAt.getTime() - stepStartedAt.getTime());
    stepResult.unstableTarget = result.unstable;
    if (result.resolved !== null) {
      stepResult.resolvedLocator = result.resolved.description;
    }
    builder.addStep(stepResult);
  }

  // Assertions run once, after every step has succeeded.
  for (let i = 0; i < opts.testCase.assertions.length; i += 1) {
    const assertion: Assertion | undefined = opts.testCase.assertions[i];
    if (assertion === undefined) continue;
    let outcome;
    try {
      outcome = await runAssertion(assertion, {
        page,
        appMap: appMapIndex,
        baseUrl: session.baseUrl,
        requests: session.getRequests(),
        cachedStatus: (req) => statusByRequest.get(req),
        consoleMessages: session.getConsoleMessages(),
        currentUrl: session.currentUrl(),
        assertionTimeoutMs: opts.assertionTimeoutMs ?? 15_000,
        defaultAssertionTimeoutMs: 15_000,
      });
    } catch (error) {
      const assertionResult = emptyAssertionResult(i, assertion.type, 'error');
      assertionResult.message = describeError(error);
      builder.addAssertion(assertionResult);
      throw new ExecutorError('INTERNAL', describeError(error), { stepIndex: i });
    }
    if (outcome.kind === 'unresolved') {
      const assertionResult = emptyAssertionResult(i, assertion.type, 'error');
      assertionResult.message = `target not found for assertion ${i}; tried: ${outcome.unresolved.tried.join(', ')}`;
      assertionResult.actual = '';
      builder.addAssertion(assertionResult);
      throw new ExecutorError('TARGET_NOT_FOUND', assertionResult.message ?? 'target not found', {
        stepIndex: i,
      });
    }
    if (outcome.kind === 'fail') {
      let shotId: string | undefined;
      try {
        shotId = await evidence.captureScreenshot(page, { name: `assertion-${i}-failure` });
      } catch {
        shotId = undefined;
      }
      const assertionResult = emptyAssertionResult(i, assertion.type, 'fail');
      assertionResult.expected = outcome.expected;
      assertionResult.actual = outcome.actual;
      assertionResult.message = outcome.message;
      if (shotId !== undefined) assertionResult.artifactIds = [shotId];
      builder.addAssertion(assertionResult);
      throw new ExecutorError('INTERNAL', outcome.message, { stepIndex: i });
    }
    const assertionResult = emptyAssertionResult(i, assertion.type, 'pass');
    builder.addAssertion(assertionResult);
  }
}

function backoff(attempt: number, minMs: number, maxMs: number): number {
  // Exponential with full jitter, bounded by [minMs, maxMs].
  const exponential = minMs * 2 ** (attempt - 1);
  return Math.min(maxMs, Math.round(minMs + Math.random() * (Math.min(maxMs, exponential) - minMs)));
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function describeError(error: unknown): string {
  if (error instanceof Error) return error.message;
  return String(error);
}
