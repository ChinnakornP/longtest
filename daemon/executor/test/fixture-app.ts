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

/** True when the playwright chromium binary is on disk. */
export function chromiumAvailable(): boolean {
  // Playwright reports its cache via the env var or ~/.cache/ms-playwright
  // — we check the latter because that is what `playwright install` writes.
  const cache = join(process.env['HOME'] ?? tmpdir(), '.cache', 'ms-playwright');
  return existsSync(cache) && existsSync(TSX_BIN);
}

export class FixtureApp {
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
      env: { ...process.env, FIXTURE_USER, FIXTURE_PASSWORD },
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
