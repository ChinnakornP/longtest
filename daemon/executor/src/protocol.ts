/**
 * @fileoverview JSON-RPC protocol between the Go daemon and this executor.
 *
 * The wire format is line-delimited JSON, one message per line. Request frames
 * carry an integer `id`, response frames echo it, and event frames carry no
 * id (the daemon's own envelope, contract D, wraps and sequences them). The
 * protocol is intentionally minimal — the executor is the only thing the
 * daemon talks to in this process, so the methods are the methods.
 *
 * Unknown methods are an error, not a silent success. A method the daemon
 * thinks exists but the executor does not is a version skew and the daemon
 * must hear about it before it sends more work.
 */

import type { ApplicationMap, ExecutionResult, TestCase } from '@qa/schema';

export const PROTOCOL_VERSION = 1 as const;

export type RpcId = number;

export interface RpcRequest<P = unknown> {
  id: RpcId;
  method: string;
  params?: P;
}

export interface RpcSuccess<R = unknown> {
  id: RpcId;
  result: R;
}

export interface RpcError {
  id: RpcId;
  error: RpcErrorBody;
}

export interface RpcErrorBody {
  /** Stable, machine-readable error code the daemon can switch on. */
  code: RpcErrorCode;
  message: string;
  /**
   * Extra context the daemon can surface to the UI. Must be JSON-serialisable
   * and must not contain page content; only executor-controlled fields.
   */
  data?: Record<string, unknown>;
}

export type RpcErrorCode =
  | 'TARGET_NOT_FOUND'
  | 'UNKNOWN_ACTION'
  | 'UNKNOWN_ASSERTION_TYPE'
  | 'INVALID_PARAMS'
  | 'INVALID_METHOD'
  | 'SESSION_NOT_OPEN'
  | 'BROWSER_LAUNCH_FAILED'
  | 'TIMEOUT'
  | 'NETWORK_ERROR'
  | 'FIXTURE_UNAVAILABLE'
  | 'CANCELLED'
  | 'INTERNAL';

export interface RpcEvent {
  event: 'step' | 'assertion' | 'evidence' | 'progress';
  data: Record<string, unknown>;
}

export type RpcFrame = RpcRequest | RpcSuccess | RpcError | RpcEvent;

export interface Viewport {
  width: number;
  height: number;
}

export interface SessionOpenParams {
  baseUrl: string;
  viewport?: Viewport;
  /**
   * Playwright storage state blob from a previous session. When present the
   * executor rehydrates cookies/localStorage so the test starts already
   * authenticated.
   */
  storageState?: unknown;
  /**
   * Locale / time zone the executor asks Chromium for. Default "en-US" / UTC.
   * Kept as a free-form string so the daemon can pass through whatever the
   * project pinned.
   */
  locale?: string;
  timezoneId?: string;
  /** Per-step timeout in milliseconds. Default 15 000. */
  defaultStepTimeoutMs?: number;
  /** Per-assertion timeout in milliseconds. Default 15 000. */
  defaultAssertionTimeoutMs?: number;
  /** Per-step retry policy. Default { retries: 1, minTimeoutMs: 100, maxTimeoutMs: 1000 }. */
  retry?: RetryPolicy;
}

export interface RetryPolicy {
  /** Number of retries after the first attempt. Default 1. */
  retries: number;
  /** Minimum backoff between retries, in milliseconds. Default 100. */
  minTimeoutMs: number;
  /** Maximum backoff between retries, in milliseconds. Default 1000. */
  maxTimeoutMs: number;
}

export interface SessionOpenResult {
  /** Browser context id. The executor supports a single session at a time. */
  sessionId: string;
  /** Echo of baseUrl so the daemon can confirm what the executor is pointing at. */
  baseUrl: string;
  /** Playwright storage state the executor captured after this session. */
  storageState: unknown;
  /** Protocol version the executor implements. */
  protocolVersion: typeof PROTOCOL_VERSION;
}

export interface TestcaseRunParams {
  testCase: TestCase;
  appMap: ApplicationMap;
  artifactDir: string;
  /**
   * Run-level storage key prefix. Used to build the artifact `key` values in
   * the returned ExecutionResult so the daemon's presigned-URL upload maps
   * them to the correct run prefix.
   *
   * Format: `orgs/{orgId}/runs/{YYYY-MM-DD}/{runId}/`. The daemon derives this
   * from `RunAssignPayload.artifactUpload.keyPrefix`; the executor never sees
   * the presigned URL or the org id directly.
   */
  storageKeyPrefix: string;
  /** Optional run id echoed in ExecutionResult.runId for debugging. */
  runId?: string;
  /** Overrides for session-level defaults. */
  stepTimeoutMs?: number;
  assertionTimeoutMs?: number;
}

export type TestcaseRunResult = ExecutionResult;
