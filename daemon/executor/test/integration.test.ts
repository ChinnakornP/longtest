/**
 * Integration tests for qa-executor.
 *
 * These spin up the fixture web app on a free port, drive a real Chromium
 * through Playwright, and assert two things:
 *
 * 1. The same test case, run three times on a freshly-restarted fixture
 *    app, produces the same outcome every time (deterministic).
 * 2. The execution-result JSON validates against `execution-result@1` and
 *    registers the four mandatory artifacts (screenshot, trace, network,
 *    console).
 *
 * These tests are slow because they own a browser and an HTTP server per
 * test. They are skipped automatically when the host has no Chromium
 * (e.g. CI without `playwright install chromium`).
 */

import { describe, expect, it, afterAll } from 'vitest';
import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { validate } from '@qa/schema';
import type { ApplicationMap, TestCase } from '@qa/schema';
import { runTestCase } from '../src/runner.ts';
import { Session } from '../src/session.ts';

const REPO_ROOT = join(__dirname, '..', '..', '..');
const FX_TSCONFIG = join(REPO_ROOT, 'e2e', 'fixture-app', 'tsconfig.json');
const FX_ENTRY = join(REPO_ROOT, 'e2e', 'fixture-app', 'src', 'server.ts');
const TSX_BIN = join(REPO_ROOT, 'daemon', 'executor', 'node_modules', '.bin', 'tsx');

/** True when the playwright chromium binary is on disk. */
function chromiumAvailable(): boolean {
  // Playwright reports its cache via the env var or ~/.cache/ms-playwright
  // — we check the latter because that is what `playwright install` writes.
  const cache = join(process.env['HOME'] ?? tmpdir(), '.cache', 'ms-playwright');
  return existsSync(cache) && existsSync(TSX_BIN);
}

class FixtureApp {
  private proc: ChildProcessWithoutNullStreams | undefined;
  private port = 0;
  readonly baseUrl: string;

  constructor() {
    this.baseUrl = '';
  }

  async start(): Promise<void> {
    if (!chromiumAvailable()) {
      throw new Error('chromium-not-installed');
    }
    const proc = spawn(TSX_BIN, [FX_ENTRY], {
      cwd: join(REPO_ROOT, 'e2e', 'fixture-app'),
      env: { ...process.env, FIXTURE_USER: 'admin@example.test', FIXTURE_PASSWORD: 'letmein' },
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
      proc.on('error', (err) => { clearTimeout(timer); reject(err); });
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
}

const appMap: ApplicationMap = {
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

const loginAndCreateEmployee = (uniqueSuffix: string): TestCase => ({
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

describe.skipIf(!chromiumAvailable())('integration: login + create employee (deterministic)', () => {
  const fixtures: FixtureApp[] = [];
  const traceDirs: string[] = [];

  afterAll(async () => {
    for (const f of fixtures) await f.stop();
    for (const dir of traceDirs) await rm(dir, { recursive: true, force: true }).catch(() => undefined);
  });

  async function freshRun(): Promise<{ result: import('@qa/schema').ExecutionResult; artifacts: string[] }> {
    const fixture = new FixtureApp();
    await fixture.start();
    fixtures.push(fixture);
    const localMap: ApplicationMap = { ...appMap, baseUrl: fixture.baseUrl };

    const traceDir = join(tmpdir(), `qe-int-trace-${randomUUID()}`);
    await mkdir(traceDir, { recursive: true });
    traceDirs.push(traceDir);

    const session = new Session({ baseUrl: fixture.baseUrl, traceDir });
    await session.open({ baseUrl: fixture.baseUrl });

    const artifactDir = join(tmpdir(), `qe-int-art-${randomUUID()}`);
    await mkdir(artifactDir, { recursive: true });

    try {
      const result = await runTestCase(session, {
        testCase: loginAndCreateEmployee(randomUUID().slice(0, 8)),
        appMap: localMap,
        artifactDir,
        storageKeyPrefix: 'orgs/test/runs/2026-09-04/int-run',
        fixtureCredentials: { logged_in_as_admin: { username: 'admin@example.test', password: 'letmein' } },
      });
      const artifacts = result.artifacts.map((a) => a.key);
      return { result, artifacts };
    } finally {
      await session.close();
      await rm(artifactDir, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  it('runs the same case three times and gets the same outcome', async () => {
    const a = await freshRun();
    const b = await freshRun();
    const c = await freshRun();
    expect(a.result.result).toBe(b.result.result);
    expect(b.result.result).toBe(c.result.result);
    expect(a.result.result).toBe('pass');
  }, 120_000);

  it('produces a result that validates against execution-result@1', async () => {
    const { result } = await freshRun();
    const validation = validate('execution-result@1', result);
    expect(validation.valid).toBe(true);
    if (!validation.valid) {
      for (const e of validation.errors) {
        process.stderr.write(`  ${e.instancePath}: ${e.message} [${e.keyword}]\n`);
      }
    }
  }, 120_000);

  it('registers the four mandatory artifact kinds', async () => {
    const { result } = await freshRun();
    const kinds = new Set(result.artifacts.map((a) => a.kind));
    expect(kinds.has('screenshot')).toBe(true);
    expect(kinds.has('trace')).toBe(true);
    expect(kinds.has('network')).toBe(true);
    expect(kinds.has('console')).toBe(true);
  }, 120_000);

  it('writes the artifact files to disk under the artifactDir', async () => {
    const fixture = new FixtureApp();
    await fixture.start();
    fixtures.push(fixture);
    const localMap: ApplicationMap = { ...appMap, baseUrl: fixture.baseUrl };

    const traceDir = join(tmpdir(), `qe-int-trace-${randomUUID()}`);
    await mkdir(traceDir, { recursive: true });
    traceDirs.push(traceDir);

    const session = new Session({ baseUrl: fixture.baseUrl, traceDir });
    await session.open({ baseUrl: fixture.baseUrl });

    const artifactDir = join(tmpdir(), `qe-int-art-${randomUUID()}`);
    await mkdir(artifactDir, { recursive: true });

    try {
      const result = await runTestCase(session, {
        testCase: loginAndCreateEmployee(randomUUID().slice(0, 8)),
        appMap: localMap,
        artifactDir,
        storageKeyPrefix: 'orgs/test/runs/2026-09-04/int-run',
        fixtureCredentials: { logged_in_as_admin: { username: 'admin@example.test', password: 'letmein' } },
      });
      // Read the screenshot artifact back and confirm it is a real PNG.
      const shot = result.artifacts.find((a) => a.kind === 'screenshot');
      expect(shot).toBeDefined();
      const localFilename = shot!.key.split('/').pop()!;
      const buf = await readFile(join(artifactDir, localFilename));
      // PNG signature: 89 50 4E 47 0D 0A 1A 0A
      expect(buf[0]).toBe(0x89);
      expect(buf[1]).toBe(0x50);
      expect(buf[2]).toBe(0x4e);
      expect(buf[3]).toBe(0x47);

      // Network log should be readable JSON.
      const net = result.artifacts.find((a) => a.kind === 'network');
      expect(net).toBeDefined();
      const netBuf = await readFile(join(artifactDir, net!.key.split('/').pop()!), 'utf8');
      expect(JSON.parse(netBuf)).toBeInstanceOf(Array);

      // Console log should be readable JSON.
      const con = result.artifacts.find((a) => a.kind === 'console');
      expect(con).toBeDefined();
      const conBuf = await readFile(join(artifactDir, con!.key.split('/').pop()!), 'utf8');
      expect(JSON.parse(conBuf)).toBeInstanceOf(Array);

      // Trace.zip exists in the session trace dir.
      const trace = result.artifacts.find((a) => a.kind === 'trace');
      expect(trace).toBeDefined();
      const traceLocal = join(traceDir, 'trace.zip');
      expect(existsSync(traceLocal)).toBe(true);
    } finally {
      await session.close();
      await rm(artifactDir, { recursive: true, force: true }).catch(() => undefined);
    }
  }, 120_000);
});

void writeFile; // ensure import is used
void FX_TSCONFIG; // ensure import is used
