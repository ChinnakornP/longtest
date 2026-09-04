/**
 * @fileoverview `qa-executor run` — drive one test case from the command line.
 *
 * The same executor the daemon talks to over stdio is also a CLI for humans.
 * The CLI takes a `test-case@1` JSON file and an `application-map@1` JSON
 * file, writes `result.json` and the artifact directory next to them, and
 * exits 0 on pass / 1 on fail / 2 on harness error — so CI can gate on it
 * the same way it gates on any test runner.
 */

import { mkdir, writeFile, copyFile, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { join, resolve as pathResolve } from 'node:path';
import { randomUUID } from 'node:crypto';
import { tmpdir } from 'node:os';
import { Session } from './session.ts';
import { runTestCase, ExecutorError } from './runner.ts';
import { validate } from '@qa/schema';
import type { ApplicationMap, TestCase } from '@qa/schema';

export interface CliOptions {
  testCasePath: string;
  appMapPath: string;
  outDir: string;
  /** Optional run-id; default is a fresh uuid so runs do not collide. */
  runId?: string;
  /** Trace directory; default is a fresh `os.tmpdir()/qa-executor-trace-*`. */
  traceDir?: string;
  /** Fixture credentials passed to `fixture:logged_in_as_admin`. */
  fixtureCredentials?: Record<string, { username: string; password: string }>;
}

export async function runCli(opts: CliOptions): Promise<number> {
  const testCase = await readJsonFile<TestCase>(opts.testCasePath);
  const appMap = await readJsonFile<ApplicationMap>(opts.appMapPath);

  // Validate before we even spin up Chromium. A schema-invalid case is a
  // CLI misuse, not a test failure — the right exit code is 2.
  const tcValid = validate('test-case@1', testCase);
  if (!tcValid.valid) {
    for (const error of tcValid.errors) {
      process.stderr.write(`${opts.testCasePath}:${error.instancePath || '/'}: ${error.message} [${error.keyword}]\n`);
    }
    return 2;
  }
  const amValid = validate('application-map@1', appMap);
  if (!amValid.valid) {
    for (const error of amValid.errors) {
      process.stderr.write(`${opts.appMapPath}:${error.instancePath || '/'}: ${error.message} [${error.keyword}]\n`);
    }
    return 2;
  }

  const outDir = pathResolve(opts.outDir);
  const artifactDir = join(outDir, 'artifacts');
  await mkdir(artifactDir, { recursive: true });

  const traceDir = opts.traceDir ?? join(tmpdir(), `qa-executor-trace-${randomUUID()}`);
  await mkdir(traceDir, { recursive: true });

  const session = new Session({ baseUrl: appMap.baseUrl, traceDir });
  try {
    await session.open({ baseUrl: appMap.baseUrl });
  } catch (error) {
    process.stderr.write(`qa-executor: failed to open browser: ${describeError(error)}\n`);
    await rm(traceDir, { recursive: true, force: true }).catch(() => undefined);
    return 2;
  }

  try {
    const result = await runTestCase(session, {
      testCase,
      appMap,
      artifactDir,
      storageKeyPrefix: buildStorageKeyPrefix(opts.runId ?? randomUUID()),
      ...(opts.runId !== undefined ? { runId: opts.runId } : {}),
      fixtureCredentials: opts.fixtureCredentials ?? {},
    });
    // Copy trace.zip from the trace directory into the artifact directory
    // so all four artifacts live side by side. The daemon-side flow does
    // not need this — the daemon uploads artifacts directly from wherever
    // the executor writes them — but the CLI promises "result.json and
    // artifacts in <out-dir>" and trace.zip belongs to that set.
    const traceFile = join(traceDir, 'trace.zip');
    if (existsSync(traceFile)) {
      await copyFile(traceFile, join(artifactDir, 'trace.zip')).catch(() => undefined);
    }
    await writeFile(join(outDir, 'result.json'), `${JSON.stringify(result, null, 2)}\n`, 'utf8');
    if (result.result === 'pass') return 0;
    if (result.result === 'fail' || result.result === 'skipped') return 1;
    return 2;
  } catch (error) {
    if (error instanceof ExecutorError) {
      process.stderr.write(`qa-executor: ${error.code}: ${error.message}\n`);
    } else {
      process.stderr.write(`qa-executor: harness error: ${describeError(error)}\n`);
    }
    return 2;
  } finally {
    await session.close();
    if (!opts.traceDir) {
      await rm(traceDir, { recursive: true, force: true }).catch(() => undefined);
    }
  }
}

async function readJsonFile<T>(path: string): Promise<T> {
  if (!existsSync(path)) {
    process.stderr.write(`qa-executor: file not found: ${path}\n`);
    process.exit(2);
  }
  const { readFile } = await import('node:fs/promises');
  const text = await readFile(path, 'utf8');
  try {
    return JSON.parse(text) as T;
  } catch (cause) {
    process.stderr.write(`qa-executor: ${path} is not valid JSON: ${describeError(cause)}\n`);
    process.exit(2);
  }
}

function describeError(error: unknown): string {
  if (error instanceof Error) return error.message;
  return String(error);
}

/** Build a placeholder storage key prefix. Real callers (the daemon) pass
 * the real `orgs/.../runs/.../{runId}/` prefix from the RunAssignPayload. */
function buildStorageKeyPrefix(runId: string): string {
  // We do not know the org id here, but the contract only requires the
  // schema pattern, not the actual org id. The CLI is for debugging — the
  // artifacts it writes do not get uploaded anywhere — so we use a
  // deterministic placeholder. The pattern needs `runs/YYYY-MM-DD/`.
  const today = new Date().toISOString().slice(0, 10);
  return `orgs/cli-local/runs/${today}/${runId}`;
}

export function printCliHelp(stream: NodeJS.WritableStream): void {
  stream.write(`qa-executor run <test-case.json> --app-map <app-map.json> --out <dir>

  Run one structured test case against an application map and write
  result.json + an artifacts directory under <dir>.

  Exit codes:
    0  the test case passed
    1  the test case failed (or was skipped)
    2  harness / CLI misuse (bad input, browser launch failure, ...)

Options:
  --app-map <file>     application map (application-map@1) JSON file
  --out <dir>          output directory for result.json and artifacts
  --run-id <uuid>      run id to embed in artifact keys (default: fresh uuid)
  --trace-dir <dir>    directory for the Playwright trace; kept after run
  --credential name=alice@example.test:secret   fixture credential, repeatable
                        name maps to fixture:<name> in preconditions
`);
}
