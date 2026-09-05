/**
 * @fileoverview Shared harness for the tests that need the real fixture app.
 *
 * It lives outside any one test file because two suites now drive the same
 * application: the determinism/artifact integration tests, and the planner
 * executability benchmark. A copy in each would drift, and the first symptom
 * would be two suites disagreeing about what "the fixture app" is.
 */

import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { ApplicationMap, TestCase } from '@qa/schema';

export const REPO_ROOT = join(__dirname, '..', '..', '..');
export const FX_ENTRY = join(REPO_ROOT, 'e2e', 'fixture-app', 'src', 'server.ts');
export const TSX_BIN = join(REPO_ROOT, 'daemon', 'executor', 'node_modules', '.bin', 'tsx');

/**
 * The fixture app's login, which is a public value in its own README and
 * exists only in a process this test starts and kills. It is passed in as a
 * fixture credential — the same channel a real run uses — rather than written
 * into any test case, because a test case that carries a password is exactly
 * what the planner contract forbids.
 */
export const FIXTURE_USER = 'admin@example.test';
export const FIXTURE_PASSWORD = 'letmein';

/** True when the fixture app can be started at all: tsx is on disk. */
export function fixtureAvailable(): boolean {
  return existsSync(TSX_BIN);
}

/** True when the playwright chromium binary is on disk. */
export function chromiumAvailable(): boolean {
  // Playwright reports its cache via the env var or ~/.cache/ms-playwright
  // — we check the latter because that is what `playwright install` writes.
  const cache = join(process.env['HOME'] ?? tmpdir(), '.cache', 'ms-playwright');
  return existsSync(cache) && fixtureAvailable();
}

export interface FixtureOptions {
  /**
   * Defects to inject, as FIXTURE_BUGS names. Empty is the honest app.
   *
   * A test that asks for one is asserting on a failure whose true cause it
   * already knows, which is the only way a failure classifier can be scored.
   */
  bugs?: readonly string[];
  /**
   * Set false for a test that drives the app over HTTP rather than through a
   * browser. Those need tsx and nothing else, and skipping them wherever
   * chromium is absent hides them on every machine that has not run
   * `playwright install`.
   */
  requiresBrowser?: boolean;
}

export class FixtureApp {
  private proc: ChildProcessWithoutNullStreams | undefined;
  private port = 0;
  private readonly options: FixtureOptions;
  readonly baseUrl: string;

  constructor(options: FixtureOptions = {}) {
    this.options = options;
    this.baseUrl = '';
  }

  async start(): Promise<void> {
    if (this.options.requiresBrowser !== false && !chromiumAvailable()) {
      throw new Error('chromium-not-installed');
    }
    if (!fixtureAvailable()) {
      throw new Error('tsx-not-installed');
    }
    const proc = spawn(TSX_BIN, [FX_ENTRY], {
      cwd: join(REPO_ROOT, 'e2e', 'fixture-app'),
      env: {
        ...process.env,
        FIXTURE_USER,
        FIXTURE_PASSWORD,
        FIXTURE_BUGS: (this.options.bugs ?? []).join(','),
      },
    });
    this.proc = proc;
    const port = await new Promise<number>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('fixture did not start within 10s')), 10_000);
      proc.stdout.on('data', (chunk: Buffer) => {
        const text = chunk.toString('utf8');
        const match = /^FIXTURE_PORT=(\d+)/m.exec(text);
        if (match !== null) {
          clearTimeout(timer);
          resolve(Number(match[1]));
        }
      });
      proc.on('error', (err) => {
        clearTimeout(timer);
        reject(err);
      });
      proc.stderr.on('data', (chunk: Buffer) => {
        // Surface any startup error
        if (chunk.length > 0) process.stderr.write(`fixture-app: ${chunk}`);
      });
    });
    this.port = port;
    (this as { baseUrl: string }).baseUrl = `http://127.0.0.1:${port}`;
  }

  async stop(): Promise<void> {
    if (this.proc === undefined) return;
    this.proc.kill('SIGTERM');
    await new Promise<void>((resolve) => {
      this.proc?.on('exit', () => resolve());
      setTimeout(resolve, 1500);
    });
    this.proc = undefined;
  }

  url(path: string): string {
    return `${this.baseUrl}${path}`;
  }

  /** The port the app is listening on; 0 before start(). */
  get listeningPort(): number {
    return this.port;
  }
}

/**
 * The application map of the fixture app, as a discovery run would produce it.
 *
 * Shared rather than copied per suite for the reason this file exists: three
 * suites now drive the same application, and the first symptom of a copy
 * drifting is two of them disagreeing about what "the fixture app" is.
 */
export const employeesAppMap: ApplicationMap = {
  version: 1,
  baseUrl: 'http://127.0.0.1:0', // overwritten per run
  pages: [
    {
      id: 'page.login',
      path: '/login',
      title: 'Sign in',
      elements: [
        { ref: 'login.input.email', type: 'input', label: 'Email', locators: [{ kind: 'testId', value: 'login-email' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'login.input.password', type: 'input', label: 'Password', locators: [{ kind: 'testId', value: 'login-password' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'login.btn.submit', type: 'button', label: 'Sign in', locators: [{ kind: 'testId', value: 'login-submit' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
      ],
    },
    {
      id: 'page.employees',
      path: '/employees',
      title: 'Employees',
      elements: [
        { ref: 'emp.btn.add', type: 'button', label: 'Add Employee', locators: [{ kind: 'testId', value: 'add-emp' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.input.first', type: 'input', label: 'First name', locators: [{ kind: 'testId', value: 'employee-first-name' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.input.last', type: 'input', label: 'Last name', locators: [{ kind: 'testId', value: 'employee-last-name' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.input.email', type: 'input', label: 'Email', locators: [{ kind: 'testId', value: 'employee-email' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.btn.save', type: 'button', label: 'Save', locators: [{ kind: 'testId', value: 'employee-save' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.table', type: 'table', label: 'Employees', locators: [{ kind: 'testId', value: 'employee-table' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.detail.email', type: 'text', label: 'Employee email', locators: [{ kind: 'testId', value: 'employee-email' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.search', type: 'input', label: 'Search', locators: [{ kind: 'testId', value: 'employee-search' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
      ],
    },
  ],
  workflows: [],
};

/**
 * Sign in, create an employee, and check it is listed.
 *
 * Passes against the honest app and fails against `create-500`, which is what
 * makes it usable as both a determinism case and an analysis fixture.
 */
export const loginAndCreateEmployee = (uniqueSuffix: string): TestCase => ({
  version: 1,
  id: 'TC-INT-001',
  name: 'login and create employee',
  priority: 'critical',
  category: 'functional',
  preconditions: ['fixture:logged_in_as_admin'],
  steps: [
    { action: 'navigate', url: '/employees' },
    { action: 'click', target: { ref: 'emp.btn.add' } },
    { action: 'fill', target: { ref: 'emp.input.first' }, value: `John-${uniqueSuffix}` },
    { action: 'fill', target: { ref: 'emp.input.last' }, value: 'Doe' },
    { action: 'fill', target: { ref: 'emp.input.email' }, value: `john-${uniqueSuffix}@example.test` },
    { action: 'click', target: { ref: 'emp.btn.save' } },
    { action: 'waitFor', target: { ref: 'emp.detail.email' }, state: 'visible', timeoutMs: 10_000 },
    { action: 'navigate', url: '/employees' },
    { action: 'waitFor', target: { ref: 'emp.table' }, state: 'visible', timeoutMs: 10_000 },
  ],
  assertions: [
    { type: 'visible', target: { ref: 'emp.table' } },
    { type: 'urlMatches', value: '^/employees' },
    { type: 'noConsoleError', ignorePatterns: ['favicon'] },
  ],
});
