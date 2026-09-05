/**
 * @fileoverview The planner executability benchmark (LONG-16).
 *
 * The acceptance criterion this file measures: a plan the Test Planner
 * produced, stored after passing the backend's review, must actually RUN. Not
 * "validates against the contract" — run, in a real browser, against a real
 * application, without the executor reporting that it could not find what the
 * plan pointed at.
 *
 * TARGET_NOT_FOUND is the number that matters, and it is the one this file
 * asserts on. A plan whose steps are individually well-formed and whose refs
 * name elements that are not there is the failure mode the whole ref-checking
 * gate exists to prevent, and it is invisible until something drives a
 * browser. Whether a case then PASSES is a different question — it depends on
 * what the application does, which is what a QA tool is for — so a failed
 * assertion is not counted against the planner here.
 *
 * The plan and the application map are the same golden pair the backend's
 * tests use. That is deliberate: this file is the evidence that the map those
 * tests validate refs against describes the application the executor drives.
 *
 * Skipped without Chromium, like the rest of the integration suite. CI runs it
 * in the `e2e` job, which installs one.
 */

import { describe, expect, it, afterAll } from 'vitest';
import { mkdir, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { validate } from '@qa/schema';
import type { ApplicationMap, ExecutionResult, TestCase, TestPlan } from '@qa/schema';
import { runTestCase } from '../src/runner.ts';
import { Session } from '../src/session.ts';
import {
  FixtureApp,
  REPO_ROOT,
  chromiumAvailable,
  FIXTURE_USER,
  FIXTURE_PASSWORD,
} from './fixture-app.ts';

/**
 * The golden pair, read from the backend's testdata rather than copied.
 *
 * A copy here would let the two drift, and the first thing to break would be
 * the property this file exists to prove: that a plan the backend accepted is
 * a plan the executor can run.
 */
const GOLDEN_DIR = join(REPO_ROOT, 'server', 'internal', 'testcase', 'testdata');

/** The share of a plan's cases that must run without a missing target. */
const MINIMUM_EXECUTABLE = 0.8;

async function readGolden<T>(name: string, schemaId: string): Promise<T> {
  const raw = await readFile(join(GOLDEN_DIR, name), 'utf8');
  const parsed: unknown = JSON.parse(raw);
  const result = validate(schemaId, parsed);
  if (!result.valid) {
    const detail = result.errors.map((e) => `${e.instancePath}: ${e.message}`).join('\n  ');
    throw new Error(`${name} is not a valid ${schemaId}:\n  ${detail}`);
  }
  return parsed as T;
}

/** One case's outcome, reduced to what the benchmark measures. */
interface Attempt {
  id: string;
  category: string;
  result: ExecutionResult['result'];
  /** The refs the executor could not resolve, if any. */
  missingTargets: string[];
}

/**
 * targetProblems pulls the missing-target failures out of a result.
 *
 * The executor classifies a step it could not resolve as TARGET_NOT_FOUND and
 * carries the message through into the step result, so the count comes from
 * the same field a human reading the report would.
 */
function targetProblems(result: ExecutionResult): string[] {
  const problems: string[] = [];
  for (const step of result.steps) {
    if (step.status !== 'fail' && step.status !== 'error') continue;
    if ((step.message ?? '').includes('target not found')) {
      problems.push(`step ${step.index} (${step.action}): ${step.message}`);
    }
  }
  for (const assertion of result.assertions ?? []) {
    if (assertion.status !== 'fail' && assertion.status !== 'error') continue;
    if ((assertion.message ?? '').includes('target not found')) {
      problems.push(`assertion ${assertion.index} (${assertion.type}): ${assertion.message}`);
    }
  }
  if (result.failureClass === 'TEST_BUG' && (result.message ?? '').includes('target not found')) {
    problems.push(result.message ?? 'target not found');
  }
  return problems;
}

describe.skipIf(!chromiumAvailable())('planner executability: the golden plan runs', () => {
  const apps: FixtureApp[] = [];
  const dirs: string[] = [];

  afterAll(async () => {
    for (const app of apps) await app.stop();
    for (const dir of dirs) await rm(dir, { recursive: true, force: true }).catch(() => undefined);
  });

  async function runPlan(): Promise<Attempt[]> {
    const plan = await readGolden<TestPlan>('fixture-app-plan.json', 'test-plan@1');
    const map = await readGolden<ApplicationMap>('fixture-app-appmap.json', 'application-map@1');

    const app = new FixtureApp();
    await app.start();
    apps.push(app);

    // The map's baseUrl is a placeholder: the fixture app takes a free port.
    const localMap: ApplicationMap = { ...map, baseUrl: app.baseUrl };

    const traceDir = join(tmpdir(), `qe-plan-trace-${randomUUID()}`);
    const artifactDir = join(tmpdir(), `qe-plan-art-${randomUUID()}`);
    await mkdir(traceDir, { recursive: true });
    await mkdir(artifactDir, { recursive: true });
    dirs.push(traceDir, artifactDir);

    const attempts: Attempt[] = [];
    for (const testCase of plan.testCases as TestCase[]) {
      // A session per case, because a case's preconditions are about the state
      // it starts in: carrying a signed-in session into the case that asserts
      // a signed-out visitor is redirected would test nothing.
      const session = new Session({ baseUrl: app.baseUrl, traceDir });
      await session.open({ baseUrl: app.baseUrl });
      try {
        const result = await runTestCase(session, {
          testCase,
          appMap: localMap,
          artifactDir,
          storageKeyPrefix: 'orgs/test/runs/2026-09-05/plan-bench',
          fixtureCredentials: {
            logged_in_as_admin: { username: FIXTURE_USER, password: FIXTURE_PASSWORD },
          },
        });
        attempts.push({
          id: testCase.id,
          category: testCase.category,
          result: result.result,
          missingTargets: targetProblems(result),
        });
      } finally {
        await session.close();
      }
    }
    return attempts;
  }

  it(`runs at least ${MINIMUM_EXECUTABLE * 100}% of its cases without a missing target`, async () => {
    const attempts = await runPlan();
    expect(attempts.length).toBeGreaterThan(0);

    const broken = attempts.filter((a) => a.missingTargets.length > 0);
    const executable = (attempts.length - broken.length) / attempts.length;

    // Printed whatever the outcome: the threshold is a floor, and a suite that
    // quietly slid from 100% to 81% is still passing and still a regression.
    process.stdout.write(
      `planner executability: ${attempts.length - broken.length}/${attempts.length} ` +
        `cases ran with every target resolved (${Math.round(executable * 100)}%)\n`,
    );
    // The number on its own is not actionable; the refs are.
    for (const attempt of broken) {
      process.stderr.write(`${attempt.id}: ${attempt.missingTargets.join('; ')}\n`);
    }
    expect(executable).toBeGreaterThanOrEqual(MINIMUM_EXECUTABLE);
  }, 300_000);

  it('exercises every one of the five contract categories', async () => {
    const plan = await readGolden<TestPlan>('fixture-app-plan.json', 'test-plan@1');
    const categories = new Set((plan.testCases as TestCase[]).map((c) => c.category));
    for (const expected of [
      'functional',
      'validation',
      'navigation',
      'ui_behavior',
      'error_handling',
    ]) {
      expect(categories.has(expected as TestCase['category'])).toBe(true);
    }
  });

  it('carries no credential in any case', async () => {
    // The contract's rule, checked against the plan the executor is about to
    // run rather than against the schema: a fixture reference is the only way
    // a case may ask to be signed in.
    const plan = await readGolden<TestPlan>('fixture-app-plan.json', 'test-plan@1');
    for (const testCase of plan.testCases as TestCase[]) {
      for (const precondition of testCase.preconditions ?? []) {
        expect(precondition.startsWith('fixture:')).toBe(true);
      }
      const body = JSON.stringify(testCase);
      expect(body).not.toContain(FIXTURE_PASSWORD);
    }
  });
});
