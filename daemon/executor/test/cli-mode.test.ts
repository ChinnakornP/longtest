/**
 * CLI mode: end-to-end test of `qa-executor run`.
 *
 * Spins up the fixture app, runs the CLI against a real test case and
 * application map, and asserts the result file + artifacts come out as
 * expected. This exercises the same code path the daemon uses (without
 * the JSON-RPC framing), so a regression here is a regression for both.
 */

import { describe, expect, it, afterAll } from 'vitest';
import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import type { ApplicationMap, TestCase } from '@qa/schema';
import { validate } from '@qa/schema';

const REPO_ROOT = join(__dirname, '..', '..', '..');
const TSX_BIN = join(REPO_ROOT, 'daemon', 'executor', 'node_modules', '.bin', 'tsx');
const EXECUTOR_ENTRY = join(REPO_ROOT, 'daemon', 'executor', 'src', 'main.ts');
const FX_ENTRY = join(REPO_ROOT, 'e2e', 'fixture-app', 'src', 'server.ts');

function chromiumAvailable(): boolean {
  const cache = join(process.env['HOME'] ?? tmpdir(), '.cache', 'ms-playwright');
  return existsSync(cache) && existsSync(TSX_BIN);
}

class FixtureApp {
  private proc: ChildProcessWithoutNullStreams | undefined;
  private port = 0;
  readonly baseUrl: string;
  constructor() { this.baseUrl = ''; }
  async start(): Promise<void> {
    if (!chromiumAvailable()) throw new Error('chromium-not-installed');
    const proc = spawn(TSX_BIN, [FX_ENTRY], {
      cwd: join(REPO_ROOT, 'e2e', 'fixture-app'),
      env: { ...process.env, FIXTURE_USER: 'admin@example.test', FIXTURE_PASSWORD: 'letmein' },
    });
    this.proc = proc;
    const port = await new Promise<number>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('fixture did not start')), 10_000);
      proc.stdout.on('data', (chunk: Buffer) => {
        const m = /^FIXTURE_PORT=(\d+)/m.exec(chunk.toString('utf8'));
        if (m !== null) { clearTimeout(timer); resolve(Number(m[1])); }
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
}

const appMap: ApplicationMap = {
  version: 1,
  baseUrl: 'http://127.0.0.1:0',
  pages: [
    { id: 'page.login', path: '/login', title: 'Sign in',
      elements: [
        { ref: 'login.input.email', type: 'input', locators: [{ kind: 'testId', value: 'login-email' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'login.input.password', type: 'input', locators: [{ kind: 'testId', value: 'login-password' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'login.btn.submit', type: 'button', locators: [{ kind: 'testId', value: 'login-submit' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
      ] },
    { id: 'page.employees', path: '/employees', title: 'Employees',
      elements: [
        { ref: 'emp.btn.add', type: 'button', locators: [{ kind: 'testId', value: 'add-emp' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.input.first', type: 'input', locators: [{ kind: 'testId', value: 'employee-first-name' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.input.last', type: 'input', locators: [{ kind: 'testId', value: 'employee-last-name' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.input.email', type: 'input', locators: [{ kind: 'testId', value: 'employee-email' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.btn.save', type: 'button', locators: [{ kind: 'testId', value: 'employee-save' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.table', type: 'table', locators: [{ kind: 'testId', value: 'employee-table' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
        { ref: 'emp.detail.email', type: 'text', locators: [{ kind: 'testId', value: 'employee-email' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
      ] },
  ],
  workflows: [],
};

const testCase = (suffix: string): TestCase => ({
  version: 1,
  id: 'TC-CLI-001',
  name: 'cli login and create employee',
  priority: 'critical',
  category: 'functional',
  preconditions: ['fixture:logged_in_as_admin'],
  steps: [
    { action: 'navigate', url: '/employees' },
    { action: 'click', target: { ref: 'emp.btn.add' } },
    { action: 'fill', target: { ref: 'emp.input.first' }, value: `Cli-${suffix}` },
    { action: 'fill', target: { ref: 'emp.input.last' }, value: 'Doe' },
    { action: 'fill', target: { ref: 'emp.input.email' }, value: `cli-${suffix}@example.test` },
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

describe.skipIf(!chromiumAvailable())('CLI mode: qa-executor run', () => {
  const apps: FixtureApp[] = [];
  const dirs: string[] = [];

  afterAll(async () => {
    for (const a of apps) await a.stop();
    for (const d of dirs) await rm(d, { recursive: true, force: true }).catch(() => undefined);
  });

  it('runs the test case end-to-end and writes a schema-valid result.json', async () => {
    const app = new FixtureApp();
    await app.start();
    apps.push(app);
    const localMap: ApplicationMap = { ...appMap, baseUrl: app.baseUrl };

    const workDir = join(tmpdir(), `qe-cli-${randomUUID()}`);
    await mkdir(workDir, { recursive: true });
    dirs.push(workDir);
    const tcPath = join(workDir, 'test-case.json');
    const amPath = join(workDir, 'app-map.json');
    const outDir = join(workDir, 'out');
    await writeFile(tcPath, JSON.stringify(testCase(randomUUID().slice(0, 6)), null, 2));
    await writeFile(amPath, JSON.stringify(localMap, null, 2));

    let capturedStderr = '';
    const proc = spawn(TSX_BIN, [
      EXECUTOR_ENTRY,
      'run',
      tcPath,
      '--app-map', amPath,
      '--out', outDir,
      '--credential', 'logged_in_as_admin=admin@example.test:letmein',
    ], { cwd: REPO_ROOT });
    const exitCode = await new Promise<number>((resolve) => {
      proc.stderr.on('data', (chunk: Buffer) => { capturedStderr += chunk.toString('utf8'); });
      proc.stdout.on('data', () => undefined);
      proc.on('close', (code) => resolve(code ?? 0));
      setTimeout(() => { proc.kill('SIGTERM'); resolve(124); }, 60_000);
    });
    if (exitCode !== 0) {
      console.error('cli-mode test stderr:', capturedStderr);
    }
    expect(exitCode).toBe(0);

    const resultText = await readFile(join(outDir, 'result.json'), 'utf8');
    const result = JSON.parse(resultText);
    const v = validate('execution-result@1', result);
    expect(v.valid).toBe(true);
    if (!v.valid) {
      for (const e of v.errors) {
        console.error(`  ${e.instancePath}: ${e.message} [${e.keyword}]`);
      }
    }
    expect(result.result).toBe('pass');

    // Artifacts must be on disk under outDir/artifacts.
    const artifacts = result.artifacts as { id: string; kind: string; key: string }[];
    for (const a of artifacts) {
      const filename = a.key.split('/').pop()!;
      const path = join(outDir, 'artifacts', filename);
      if (!existsSync(path)) {
        console.error(`missing artifact: ${path} (key=${a.key})`);
      }
      expect(existsSync(path)).toBe(true);
    }
  }, 90_000);

  it('exits with code 2 when the test case file is not valid JSON', async () => {
    const workDir = join(tmpdir(), `qe-cli-${randomUUID()}`);
    await mkdir(workDir, { recursive: true });
    dirs.push(workDir);
    const tcPath = join(workDir, 'bad.json');
    const amPath = join(workDir, 'app-map.json');
    await writeFile(tcPath, '{ not valid');
    await writeFile(amPath, JSON.stringify(appMap, null, 2));

    const proc = spawn(TSX_BIN, [
      EXECUTOR_ENTRY,
      'run',
      tcPath,
      '--app-map', amPath,
      '--out', join(workDir, 'out'),
    ], { cwd: REPO_ROOT });
    const exitCode = await new Promise<number>((resolve) => {
      proc.on('close', (code) => resolve(code ?? 0));
      setTimeout(() => { proc.kill('SIGTERM'); resolve(124); }, 15_000);
    });
    expect(exitCode).toBe(2);
  }, 30_000);
});
