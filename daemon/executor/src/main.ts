/**
 * qa-executor entrypoint.
 *
 * Two modes:
 *   - `qa-executor run <test-case.json> --app-map ... --out ...` for humans.
 *   - `qa-executor` (no args, or args the CLI does not understand) starts
 *     the stdio JSON-RPC loop the daemon talks to. Anything sent to stdin
 *     before the loop decides it is not a CLI invocation gets ignored.
 *
 * The version flag is special-cased to `--version` / `-V` so the daemon
 * can ask for the wire version before it sends a `session.open`.
 */

import {
  UNTRUSTED_CLOSE,
  UNTRUSTED_END,
  UNTRUSTED_START,
  wrapBlock,
  wrapUntrusted,
} from './untrusted.ts';
import { runCli, printCliHelp, type CliOptions } from './cli-mode.ts';
import { runStdioLoop } from './stdio-loop.ts';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { mkdir, rm } from 'node:fs/promises';

export { UNTRUSTED_CLOSE, UNTRUSTED_END, UNTRUSTED_START, wrapBlock, wrapUntrusted };
export type { UntrustedBlock, UntrustedKind } from './untrusted.ts';

const VERSION = '0.0.0';

async function main(argv: string[]): Promise<number> {
  if (argv[0] === '--version' || argv[0] === '-V') {
    process.stdout.write(`qa-executor ${VERSION}\n`);
    return 0;
  }
  if (argv[0] === '-h' || argv[0] === '--help') {
    printCliHelp(process.stdout);
    return 0;
  }

  if (argv[0] === 'run') {
    return runCliFromArgs(argv.slice(1));
  }

  // No CLI verb → stdio JSON-RPC loop. We never reach this branch from a
  // shell invocation because the CLI verbs short-circuit above; this is the
  // path the daemon takes when it `exec`s us.
  return runStdioFromArgs(argv);
}

async function runCliFromArgs(argv: string[]): Promise<number> {
  let testCasePath: string | undefined;
  let appMapPath: string | undefined;
  let outDir: string | undefined;
  let runId: string | undefined;
  let traceDir: string | undefined;
  const credentials: Record<string, { username: string; password: string }> = {};
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === undefined) continue;
    switch (arg) {
      case '--app-map':
        appMapPath = argv[++i];
        break;
      case '--out':
        outDir = argv[++i];
        break;
      case '--run-id':
        runId = argv[++i];
        break;
      case '--trace-dir':
        traceDir = argv[++i];
        break;
      case '--credential': {
        const next = argv[++i];
        if (next === undefined) {
          process.stderr.write('qa-executor: --credential needs name=username:password\n');
          return 2;
        }
        const eq = next.indexOf('=');
        const colon = next.indexOf(':');
        if (eq === -1 || colon === -1 || colon < eq) {
          process.stderr.write(`qa-executor: --credential "${next}" is not name=username:password\n`);
          return 2;
        }
        const name = next.slice(0, eq);
        const username = next.slice(eq + 1, colon);
        const password = next.slice(colon + 1);
        credentials[name] = { username, password };
        break;
      }
      default:
        if (arg.startsWith('--')) {
          process.stderr.write(`qa-executor: unknown flag "${arg}"\n`);
          printCliHelp(process.stderr);
          return 2;
        }
        if (testCasePath === undefined) testCasePath = arg;
        break;
    }
  }
  if (testCasePath === undefined || appMapPath === undefined || outDir === undefined) {
    process.stderr.write('qa-executor: missing required argument\n');
    printCliHelp(process.stderr);
    return 2;
  }
  const opts: CliOptions = {
    testCasePath,
    appMapPath,
    outDir,
    ...(runId !== undefined ? { runId } : {}),
    ...(traceDir !== undefined ? { traceDir } : {}),
    fixtureCredentials: credentials,
  };
  return runCli(opts);
}

async function runStdioFromArgs(_argv: string[]): Promise<number> {
  // The trace directory is per-process. The daemon owns the lifecycle, so
  // we drop it when the process exits — Chromium cleans up after itself.
  const traceDir = join(tmpdir(), `qa-executor-trace-${process.pid}`);
  await mkdir(traceDir, { recursive: true });
  try {
    await runStdioLoop({
      traceDir,
      defaultFixtureCredentials: {},
    });
    return 0;
  } finally {
    await rm(traceDir, { recursive: true, force: true }).catch(() => undefined);
  }
}

if (process.argv[1] !== undefined && process.argv[1].endsWith('main.ts')) {
  main(process.argv.slice(2))
    .then((code) => {
      process.exitCode = code;
    })
    .catch((error) => {
      process.stderr.write(`qa-executor: fatal: ${error instanceof Error ? error.message : String(error)}\n`);
      process.exitCode = 2;
    });
}
