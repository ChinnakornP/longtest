/**
 * @fileoverview The stdio JSON-RPC loop.
 *
 * One line per JSON object on stdin, one line per frame on stdout. Requests
 * carry an integer `id` and we respond with either `{id, result}` or
 * `{id, error}`. Events we emit on our own (step landed, screenshot saved)
 * carry no id; the daemon's envelope wraps them. Anything malformed on
 * stdin is rejected with a JSON-RPC "Parse error" — but the parse error
 * response goes to stderr in a way the daemon can ignore, because a parse
 * error means we cannot trust the request id either.
 */

import { Session } from './session.ts';
import { ExecutorError, runTestCase, type RunOptions } from './runner.ts';
import { mkdir } from 'node:fs/promises';
import {
  PROTOCOL_VERSION,
  type RpcErrorBody,
  type RpcFrame,
  type RpcRequest,
  type RpcSuccess,
  type RpcError,
  type SessionOpenParams,
  type SessionOpenResult,
  type TestcaseRunParams,
} from './protocol.ts';

interface LoopOptions {
  /** Trace directory the session will write to. */
  traceDir: string;
  /** Default fixture credentials injected by the daemon. */
  defaultFixtureCredentials: Record<string, { username: string; password: string }>;
}

export async function runStdioLoop(opts: LoopOptions): Promise<void> {
  const session = new Session({ baseUrl: '', traceDir: opts.traceDir });
  // We start with `baseUrl: ''` and re-create on session.open. The field
  // exists for type symmetry — callers can read it before they have any
  // session.
  let fixtureCredentials = opts.defaultFixtureCredentials;

  const rl = createLineReader(process.stdin);
  const nextId = 0;
  void nextId;

  for await (const line of rl) {
    const trimmed = line.trim();
    if (trimmed.length === 0) continue;
    let request: RpcRequest;
    try {
      const parsed = JSON.parse(trimmed);
      request = assertRequest(parsed);
    } catch (error) {
      // We cannot trust the id of a malformed request, so emit a top-level
      // JSON-RPC error frame. Per the spec, the parser error uses id: null.
      const errFrame: RpcFrame = {
        id: -1,
        error: { code: 'INVALID_PARAMS', message: describeError(error) },
      };
      process.stdout.write(`${JSON.stringify(errFrame)}\n`);
      continue;
    }

    try {
      const result = await dispatch(request, session, () => fixtureCredentials, (next) => { fixtureCredentials = next; });
      if (request.method !== 'session.close') {
        const success: RpcSuccess = { id: request.id, result };
        process.stdout.write(`${JSON.stringify(success)}\n`);
      } else {
        // session.close returns no payload; send a success with null result
        // so the daemon gets a deterministic "ack" shape.
        const success: RpcSuccess = { id: request.id, result: null };
        process.stdout.write(`${JSON.stringify(success)}\n`);
      }
    } catch (error) {
      const body = toErrorBody(error);
      const errFrame: RpcError = { id: request.id, error: body };
      process.stdout.write(`${JSON.stringify(errFrame)}\n`);
    }
  }
}

async function dispatch(
  request: RpcRequest,
  session: Session,
  getCreds: () => RunOptions['fixtureCredentials'],
  setCreds: (next: RunOptions['fixtureCredentials']) => void,
): Promise<unknown> {
  switch (request.method) {
    case 'session.open': {
      const params = request.params as SessionOpenParams | undefined;
      if (params === undefined) {
        throw rpcError('INVALID_PARAMS', 'session.open requires params');
      }
      if (session.isOpen()) {
        throw rpcError('INTERNAL', 'session already open');
      }
      const nextSession = new Session({ baseUrl: params.baseUrl, traceDir: session.traceDir });
      await nextSession.open(params);
      // Swap the new session in for the dispatcher. We do not keep a
      // pointer to the old one — close is idempotent and the old session
      // was never opened.
      Object.assign(session, nextSession);
      const result: SessionOpenResult = {
        sessionId: 'default',
        baseUrl: params.baseUrl,
        storageState: nextSession.storageState,
        protocolVersion: PROTOCOL_VERSION,
      };
      // Return the storage state to the daemon; the daemon will hand it
      // back on the next open to keep cookies across runs.
      return result;
    }
    case 'testcase.run': {
      const params = request.params as TestcaseRunParams | undefined;
      if (params === undefined) {
        throw rpcError('INVALID_PARAMS', 'testcase.run requires params');
      }
      if (!session.isOpen()) {
        throw rpcError('SESSION_NOT_OPEN', 'testcase.run requires an open session');
      }
      await mkdir(params.artifactDir, { recursive: true });
      const result = await runTestCase(session, {
        testCase: params.testCase,
        appMap: params.appMap,
        artifactDir: params.artifactDir,
        storageKeyPrefix: params.storageKeyPrefix,
        ...(params.runId !== undefined ? { runId: params.runId } : {}),
        ...(params.stepTimeoutMs !== undefined ? { stepTimeoutMs: params.stepTimeoutMs } : {}),
        ...(params.assertionTimeoutMs !== undefined ? { assertionTimeoutMs: params.assertionTimeoutMs } : {}),
        fixtureCredentials: getCreds(),
      });
      // The session may have grown credentials (e.g. logged_in_as_admin
      // succeeded and the daemon wants to persist them for the next case).
      // We refresh the captured storage state so the next session.open
      // call gets the latest cookies.
      await session.captureStorageState();
      return result;
    }
    case 'session.close': {
      if (!session.isOpen()) return null;
      await session.close();
      setCreds({});
      return null;
    }
    default:
      throw rpcError('INVALID_METHOD', `unknown method "${request.method}"`, { method: request.method });
  }
}

function assertRequest(value: unknown): RpcRequest {
  if (typeof value !== 'object' || value === null) {
    throw rpcError('INVALID_PARAMS', 'request must be an object');
  }
  const obj = value as Record<string, unknown>;
  if (typeof obj['id'] !== 'number') {
    throw rpcError('INVALID_PARAMS', 'request.id must be a number');
  }
  if (typeof obj['method'] !== 'string') {
    throw rpcError('INVALID_PARAMS', 'request.method must be a string');
  }
  return { id: obj['id'], method: obj['method'], ...(obj['params'] !== undefined ? { params: obj['params'] } : {}) };
}

function toErrorBody(error: unknown): RpcErrorBody {
  if (error instanceof ExecutorError) {
    return { code: error.code, message: error.message, ...(error.data !== undefined ? { data: error.data } : {}) };
  }
  if (error instanceof RpcCallError) return error.body;
  return { code: 'INTERNAL', message: describeError(error) };
}

/** Internal error type: lets the dispatcher raise an RpcErrorBody directly. */
class RpcCallError extends Error {
  readonly body: RpcErrorBody;
  constructor(body: RpcErrorBody) {
    super(body.message);
    this.body = body;
  }
}

function rpcError(code: RpcErrorBody['code'], message: string, data?: Record<string, unknown>): RpcCallError {
  return new RpcCallError({ code, message, ...(data !== undefined ? { data } : {}) });
}

function describeError(error: unknown): string {
  if (error instanceof Error) return error.message;
  return String(error);
}

/** Async iterator over non-empty stdin lines. */
async function* createLineReader(stream: NodeJS.ReadableStream): AsyncGenerator<string> {
  let buffer = '';
  for await (const chunk of stream) {
    buffer += typeof chunk === 'string' ? chunk : chunk.toString('utf8');
    let nl = buffer.indexOf('\n');
    while (nl !== -1) {
      const line = buffer.slice(0, nl);
      buffer = buffer.slice(nl + 1);
      yield line;
      nl = buffer.indexOf('\n');
    }
  }
  if (buffer.length > 0) yield buffer;
}
