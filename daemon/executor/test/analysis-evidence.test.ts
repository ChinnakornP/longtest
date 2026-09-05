/**
 * @fileoverview The evidence a failed run leaves for the Failure Analyst.
 *
 * The analyst (T15) does not see the browser. It sees an execution result and
 * the artifact files this executor wrote, and it classifies from those. So the
 * question this suite asks is not "did the analyst get it right" — that needs a
 * model — but the one underneath it: when the application under test has a
 * known defect, is the evidence of that defect in what the analyst is handed?
 *
 * A classifier given a network log with no 500 in it cannot report a 500, and
 * would be blamed for a gap that happened two components earlier.
 */

import { mkdir, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';

import { afterEach, describe, expect, it } from 'vitest';

import { Session } from '../src/session.ts';
import { runTestCase } from '../src/runner.ts';
import {
  FixtureApp,
  chromiumAvailable,
  employeesAppMap,
  loginAndCreateEmployee,
  FIXTURE_USER,
  FIXTURE_PASSWORD,
} from './fixture-app.ts';

interface NetworkEntry {
  method: string;
  url: string;
  status?: number;
}

describe.skipIf(!chromiumAvailable())('analysis evidence: a known defect leaves a trace', () => {
  const cleanup: Array<() => Promise<void>> = [];

  afterEach(async () => {
    for (const fn of cleanup.splice(0)) await fn().catch(() => undefined);
  });

  async function runAgainst(bugs: readonly string[]) {
    const fixture = new FixtureApp({ bugs });
    await fixture.start();
    cleanup.push(() => fixture.stop());

    const traceDir = join(tmpdir(), `qe-ev-trace-${randomUUID()}`);
    const artifactDir = join(tmpdir(), `qe-ev-art-${randomUUID()}`);
    await mkdir(traceDir, { recursive: true });
    await mkdir(artifactDir, { recursive: true });
    cleanup.push(() => rm(traceDir, { recursive: true, force: true }));
    cleanup.push(() => rm(artifactDir, { recursive: true, force: true }));

    const session = new Session({ baseUrl: fixture.baseUrl, traceDir });
    await session.open({ baseUrl: fixture.baseUrl });
    cleanup.push(() => session.close());

    const result = await runTestCase(session, {
      testCase: loginAndCreateEmployee(randomUUID().slice(0, 8)),
      appMap: { ...employeesAppMap, baseUrl: fixture.baseUrl },
      artifactDir,
      storageKeyPrefix: 'orgs/test/runs/2026-09-04/ev-run',
      fixtureCredentials: { logged_in_as_admin: { username: FIXTURE_USER, password: FIXTURE_PASSWORD } },
    });
    return { result, artifactDir };
  }

  /** The network log as the analyst's evidence collector reads it. */
  async function networkLog(
    result: import('@qa/schema').ExecutionResult,
    artifactDir: string,
  ): Promise<NetworkEntry[]> {
    const artifact = result.artifacts.find((a) => a.kind === 'network');
    expect(artifact, 'the execution registered no network artifact').toBeDefined();
    const name = (artifact as { key: string }).key.split('/').pop() as string;
    return JSON.parse(await readFile(join(artifactDir, name), 'utf8')) as NetworkEntry[];
  }

  it('leaves the 500 in the network log when create-500 is injected', async () => {
    const { result, artifactDir } = await runAgainst(['create-500']);

    // The case fails, which is the point of injecting the defect.
    expect(result.result).not.toBe('pass');
    // Every failed execution must have a step the analyst can blame.
    expect(result.steps.some((step) => step.status !== 'pass')).toBe(true);

    const entries = await networkLog(result, artifactDir);
    const failed = entries.filter((entry) => entry.status !== undefined && entry.status >= 400);
    expect(failed.length, `no failing request in ${JSON.stringify(entries)}`).toBeGreaterThan(0);
    expect(failed.some((entry) => entry.status === 500 && entry.url.includes('/employees'))).toBe(true);
  }, 120_000);

  it('leaves a screenshot to cite, so the finding is not evidence-free', async () => {
    // finding@1 requires at least one artifact id, and a finding with none is
    // rejected before it is stored. A failed execution that produced nothing
    // citable is therefore a gap in the report, not just a thin one.
    const { result } = await runAgainst(['create-500']);
    expect(result.artifacts.length).toBeGreaterThan(0);
    expect(result.artifacts.some((a) => a.kind === 'screenshot')).toBe(true);
  }, 120_000);

  it('leaves no failing request when edit-not-synced is the defect', async () => {
    // The hard case, stated as a property: this defect is invisible to
    // anything that classifies by status code. The rule pass must not claim
    // it, and the model has to read the assertion to get it right.
    const { result, artifactDir } = await runAgainst(['edit-not-synced']);

    const entries = await networkLog(result, artifactDir);
    const failed = entries.filter((entry) => entry.status === undefined || entry.status >= 400);
    expect(failed, `a status-code classifier could shortcut this: ${JSON.stringify(failed)}`).toHaveLength(0);
  }, 120_000);

  it('passes against the honest app, so a failure means the defect', async () => {
    const { result } = await runAgainst([]);
    expect(result.result).toBe('pass');
  }, 120_000);
});
