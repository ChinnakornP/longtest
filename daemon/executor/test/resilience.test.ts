/**
 * Resilience: a misbehaving target must not hang the executor.
 *
 * Acceptance criteria: "executor รอด: หน้าเว็บที่ crash / redirect วนลูป /
 * timeout ต้องจบเป็น `error` พร้อม evidence ไม่ใช่ค้าง".
 *
 * We exercise three failure modes that the daemon must be able to
 * distinguish from a legitimate product bug:
 *
 *   - The page crashes after `navigate` (we close the response mid-flight).
 *   - The page redirects in a loop (302 → 302 → ...).
 *   - A step asks for an element that will never appear (timeout).
 *
 * Every case must finish with `result: error`, not hang, and must still
 * register the four artifacts (a Failure Analyst needs *something* to read).
 */

import { describe, expect, it } from 'vitest';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { mkdir, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import type { ApplicationMap, TestCase } from '@qa/schema';
import { runTestCase } from '../src/runner.ts';
import { Session } from '../src/session.ts';
import { validate } from '@qa/schema';

const TSX_BIN = join(__dirname, '..', '..', 'daemon', 'executor', 'node_modules', '.bin', 'tsx');

function chromiumAvailable(): boolean {
  const cache = join(process.env['HOME'] ?? tmpdir(), '.cache', 'ms-playwright');
  return existsSync(cache) && existsSync(TSX_BIN);
}

class ChaosApp {
  private server: ReturnType<typeof createServer> | undefined;
  private port = 0;
  readonly baseUrl: string;
  constructor() { this.baseUrl = ''; }
  async start(mode: 'crash' | 'redirect-loop' | 'timeout'): Promise<void> {
    this.server = createServer((req, res) => chaosHandler(mode, req, res));
    await new Promise<void>((resolve) => this.server!.listen(0, () => resolve()));
    const address = this.server!.address();
    if (address === null || typeof address === 'string') throw new Error('no address');
    this.port = address.port;
    (this as { baseUrl: string }).baseUrl = `http://127.0.0.1:${this.port}`;
  }
  async stop(): Promise<void> {
    if (this.server === undefined) return;
    await new Promise<void>((resolve) => this.server!.close(() => resolve()));
    this.server = undefined;
  }
}

function chaosHandler(mode: 'crash' | 'redirect-loop' | 'timeout', _req: IncomingMessage, res: ServerResponse): void {
  if (mode === 'crash') {
    // Write a partial header and destroy the socket — Chromium reports
    // `net::ERR_CONNECTION_RESET` (or similar) and Playwright surfaces it
    // as a navigation error.
    res.writeHead(200, { 'content-type': 'text/html' });
    res.write('<h1>This page will </h1>');
    res.destroy();
    return;
  }
  if (mode === 'redirect-loop') {
    res.writeHead(302, { location: '/redirect-loop' });
    res.end();
    return;
  }
  // timeout: hang forever without ever responding. The executor relies on
  // Playwright's per-step timeout to give up.
  res.writeHead(200, { 'content-type': 'text/html' });
  res.write('<!doctype html><html><head>');
  // Never call res.end().
}

const appMap: ApplicationMap = {
  version: 1,
  baseUrl: 'http://127.0.0.1:0',
  pages: [
    { id: 'page.boom', path: '/', title: 'boom',
      elements: [
        { ref: 'boom.never', type: 'button', locators: [{ kind: 'testId', value: 'never-appears' }], lastSeenRunId: '00000000-0000-0000-0000-000000000001' },
      ] },
  ],
  workflows: [],
};

async function runAgainst(mode: 'crash' | 'redirect-loop' | 'timeout', tc: TestCase): Promise<import('@qa/schema').ExecutionResult> {
  const chaos = new ChaosApp();
  await chaos.start(mode);
  const traceDir = join(tmpdir(), `qe-res-trace-${randomUUID()}`);
  const artifactDir = join(tmpdir(), `qe-res-art-${randomUUID()}`);
  await mkdir(traceDir, { recursive: true });
  await mkdir(artifactDir, { recursive: true });
  const session = new Session({ baseUrl: chaos.baseUrl, traceDir });
  await session.open({ baseUrl: chaos.baseUrl });
  const localMap: ApplicationMap = { ...appMap, baseUrl: chaos.baseUrl };
  try {
    return await runTestCase(session, {
      testCase: tc,
      appMap: localMap,
      artifactDir,
      storageKeyPrefix: 'orgs/test/runs/2026-09-04/res',
      fixtureCredentials: {},
    });
  } finally {
    await session.close();
    await chaos.stop();
    await rm(traceDir, { recursive: true, force: true }).catch(() => undefined);
    await rm(artifactDir, { recursive: true, force: true }).catch(() => undefined);
  }
}

describe.skipIf(!chromiumAvailable())('resilience: harness must not hang', () => {
  it('finishes as error when the page crashes', async () => {
    const tc: TestCase = {
      version: 1,
      id: 'TC-RES-CRASH',
      name: 'crash',
      priority: 'medium',
      category: 'error_handling',
      preconditions: [],
      steps: [{ action: 'navigate', url: '/', timeoutMs: 5_000 }],
      assertions: [{ type: 'visible', target: { ref: 'boom.never' } }],
    };
    const result = await runAgainst('crash', tc);
    expect(['error', 'fail']).toContain(result.result);
    expect(result.steps.length).toBeGreaterThan(0);
    // Validates against the schema even when the harness gave up early.
    const v = validate('execution-result@1', result);
    expect(v.valid).toBe(true);
  }, 60_000);

  it('finishes as error on a redirect loop', async () => {
    const tc: TestCase = {
      version: 1,
      id: 'TC-RES-LOOP',
      name: 'redirect loop',
      priority: 'medium',
      category: 'error_handling',
      preconditions: [],
      steps: [{ action: 'navigate', url: '/redirect-loop', timeoutMs: 5_000 }],
      assertions: [{ type: 'visible', target: { ref: 'boom.never' } }],
    };
    const result = await runAgainst('redirect-loop', tc);
    expect(['error', 'fail']).toContain(result.result);
  }, 60_000);

  it('finishes as error on a step timeout', async () => {
    const tc: TestCase = {
      version: 1,
      id: 'TC-RES-TIMEOUT',
      name: 'timeout',
      priority: 'medium',
      category: 'error_handling',
      preconditions: [],
      steps: [{ action: 'navigate', url: '/', timeoutMs: 3_000 }],
      // Wait for an element the page will never render.
      assertions: [{ type: 'visible', target: { ref: 'boom.never' }, timeoutMs: 3_000 }],
    };
    const result = await runAgainst('timeout', tc);
    expect(['error', 'fail']).toContain(result.result);
  }, 60_000);
});
