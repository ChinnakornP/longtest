/**
 * @fileoverview Assertion runtime: the executor's deterministic opinions.
 *
 * Every enum member in `ASSERTION_TYPE_VALUES` must have an implementation
 * here. The runner dispatches by `AssertionType`, so the "unknown type →
 * error" check lives in the runner before we ever get here.
 *
 * Assertions return a verdict (`pass`/`fail`) plus a structured reason the
 * Failure Analyst can quote. Observed text is untrusted by definition, so
 * it is reported in the result but never echoed back into prompts.
 */

import type {
  Assertion,
  AssertionResult,
  AssertionType,
} from '@qa/schema';
import type { Page, Request as PlaywrightRequest } from 'playwright';
import { resolveTarget, type ResolvedTarget, type UnresolvedTarget } from './locator.ts';

export interface AssertionContext {
  page: Page;
  appMap: { byRef: Map<string, { element: { ref: string; locators: readonly import('@qa/schema').Locator[] }; pagePath: string }> };
  baseUrl: string;
  /** Network requests captured during the run, in arrival order. */
  requests: readonly PlaywrightRequest[];
  /** Lookup a request's already-resolved status (the runner awaits it). */
  cachedStatus: (req: PlaywrightRequest) => number | undefined;
  /** Console messages captured during the run. */
  consoleMessages: readonly { level: 'log' | 'warn' | 'error' | 'info' | 'debug'; text: string }[];
  /** Current URL the runner most recently observed. */
  currentUrl: string;
  assertionTimeoutMs: number;
  defaultAssertionTimeoutMs: number;
}

export type AssertionOutcome =
  | { kind: 'pass' }
  | { kind: 'fail'; expected: string; actual: string; message: string }
  | { kind: 'unresolved'; unresolved: UnresolvedTarget };

export async function runAssertion(
  assertion: Assertion,
  ctx: AssertionContext,
): Promise<AssertionOutcome> {
  const timeout = 'timeoutMs' in assertion && typeof assertion.timeoutMs === 'number'
    ? assertion.timeoutMs
    : ctx.assertionTimeoutMs ?? ctx.defaultAssertionTimeoutMs;

  switch (assertion.type) {
    case 'visible':
      return runVisible(assertion, ctx, timeout);
    case 'hidden':
      return runHidden(assertion, ctx, timeout);
    case 'textEquals':
      return runTextEquals(assertion, ctx, timeout);
    case 'textContains':
      return runTextContains(assertion, ctx, timeout);
    case 'urlMatches':
      return runUrlMatches(assertion, ctx);
    case 'elementCount':
      return runElementCount(assertion, ctx, timeout);
    case 'httpStatusNot':
      return runHttpStatusNot(assertion, ctx);
    case 'noConsoleError':
      return runNoConsoleError(assertion, ctx);
  }
}

async function resolveOrFail(
  ctx: AssertionContext,
  target: import('@qa/schema').Target,
): Promise<{ ok: ResolvedTarget } | { fail: UnresolvedTarget }> {
  return resolveTarget(ctx.page, ctx.appMap, target).then((r) =>
    'locator' in r ? { ok: r } : { fail: r },
  );
}

async function runVisible(
  assertion: import('@qa/schema').TargetAssertion,
  ctx: AssertionContext,
  timeout: number,
): Promise<AssertionOutcome> {
  const resolved = await resolveOrFail(ctx, assertion.target);
  if ('fail' in resolved) return { kind: 'unresolved', unresolved: resolved.fail };
  try {
    await resolved.ok.locator.waitFor({ state: 'visible', timeout });
    return { kind: 'pass' };
  } catch (error) {
    return {
      kind: 'fail',
      expected: 'visible',
      actual: 'not visible within timeout',
      message: assertion.message ?? `expected target to be visible: ${describeError(error)}`,
    };
  }
}

/**
 * `hidden` is the one assertion an unresolvable target satisfies.
 *
 * "No element matches" is exactly what this assertion is claiming, so
 * reporting it as TARGET_NOT_FOUND would mean the only way to pass is for the
 * element to exist and be invisible — and the common case, an element that is
 * absent from the page altogether, could never be asserted at all. It is the
 * difference between "the employee table is not shown to a signed-out visitor"
 * being expressible and not.
 *
 * `ambiguous` is still a failure: several elements matched, so the thing is
 * there, several times over.
 */
async function runHidden(
  assertion: import('@qa/schema').TargetAssertion,
  ctx: AssertionContext,
  timeout: number,
): Promise<AssertionOutcome> {
  const resolved = await resolveOrFail(ctx, assertion.target);
  if ('fail' in resolved) {
    if (resolved.fail.reason === 'no_match') return { kind: 'pass' };
    return {
      kind: 'fail',
      expected: 'hidden',
      actual: `${resolved.fail.reason}: ${resolved.fail.tried.join(', ')}`,
      message: assertion.message ?? 'expected target to be hidden, but several elements matched',
    };
  }
  try {
    await resolved.ok.locator.waitFor({ state: 'hidden', timeout });
    return { kind: 'pass' };
  } catch (error) {
    return {
      kind: 'fail',
      expected: 'hidden',
      actual: 'still visible at end of timeout',
      message: assertion.message ?? `expected target to be hidden: ${describeError(error)}`,
    };
  }
}

async function runTextEquals(
  assertion: import('@qa/schema').TextAssertion,
  ctx: AssertionContext,
  timeout: number,
): Promise<AssertionOutcome> {
  const resolved = await resolveOrFail(ctx, assertion.target);
  if ('fail' in resolved) return { kind: 'unresolved', unresolved: resolved.fail };
  try {
    await resolved.ok.locator.waitFor({ state: 'visible', timeout });
  } catch {
    // fall through; the actual text will reveal why
  }
  const actual = await safeTextContent(resolved.ok.locator, timeout);
  if (actual === assertion.value) return { kind: 'pass' };
  return {
    kind: 'fail',
    expected: assertion.value,
    actual: actual ?? '(empty)',
    message: assertion.message ?? 'text did not equal expected',
  };
}

async function runTextContains(
  assertion: import('@qa/schema').TextAssertion,
  ctx: AssertionContext,
  timeout: number,
): Promise<AssertionOutcome> {
  const resolved = await resolveOrFail(ctx, assertion.target);
  if ('fail' in resolved) return { kind: 'unresolved', unresolved: resolved.fail };
  try {
    await resolved.ok.locator.waitFor({ state: 'visible', timeout });
  } catch {
    // fall through
  }
  const actual = await safeTextContent(resolved.ok.locator, timeout);
  if (actual !== undefined && actual.includes(assertion.value)) return { kind: 'pass' };
  return {
    kind: 'fail',
    expected: `contains ${JSON.stringify(assertion.value)}`,
    actual: actual ?? '(empty)',
    message: assertion.message ?? 'text did not contain expected',
  };
}

function runUrlMatches(assertion: import('@qa/schema').UrlMatchesAssertion, ctx: AssertionContext): AssertionOutcome {
  const url = assertion.value;
  // The contract pattern (`^/employees/[0-9]+$`) is path-shaped, so we
  // match it against the path component of the URL. Falling back to the
  // full URL means the planner can also write a regex that includes the
  // host (`^https://staging...`); a regex that matches either wins.
  let pathname: string;
  try {
    pathname = new URL(ctx.currentUrl).pathname;
  } catch {
    pathname = ctx.currentUrl;
  }
  const re = new RegExp(url);
  if (re.test(pathname) || re.test(ctx.currentUrl)) return { kind: 'pass' };
  return {
    kind: 'fail',
    expected: `url matches ${url}`,
    actual: ctx.currentUrl,
    message: assertion.message ?? `current url ${ctx.currentUrl} did not match ${url}`,
  };
}

async function runElementCount(
  assertion: import('@qa/schema').ElementCountAssertion,
  ctx: AssertionContext,
  timeout: number,
): Promise<AssertionOutcome> {
  const resolved = await resolveOrFail(ctx, assertion.target);
  if ('fail' in resolved) return { kind: 'unresolved', unresolved: resolved.fail };
  // Auto-wait for at least one match before counting; the alternative is a
  // count of zero for a not-yet-mounted element followed by a confusing
  // "expected 3, got 0" failure.
  try {
    await resolved.ok.locator.first().waitFor({ state: 'attached', timeout });
  } catch {
    // ignore — count will be 0 and the assertion will fail loudly
  }
  const count = await resolved.ok.locator.count();
  const op = assertion.operator ?? 'eq';
  const ok =
    op === 'eq' ? count === assertion.value :
    op === 'gte' ? count >= assertion.value :
    count <= assertion.value;
  if (ok) return { kind: 'pass' };
  return {
    kind: 'fail',
    expected: `count ${op} ${assertion.value}`,
    actual: String(count),
    message: assertion.message ?? `element count ${count} does not satisfy ${op} ${assertion.value}`,
  };
}

function runHttpStatusNot(assertion: import('@qa/schema').HttpStatusNotAssertion, ctx: AssertionContext): AssertionOutcome {
  // Synchronous check against the captured status (recordNetwork resolves
  // response() before it appends to the log).
  const offending = ctx.requests.find((r) => {
    const cached = ctx.cachedStatus(r);
    return cached === assertion.value;
  });
  if (!offending) return { kind: 'pass' };
  return {
    kind: 'fail',
    expected: `no HTTP response with status ${assertion.value}`,
    actual: `${offending.method()} ${offending.url()} → ${assertion.value}`,
    message: assertion.message ?? `saw HTTP ${assertion.value} for ${offending.url()}`,
  };
}

function runNoConsoleError(assertion: import('@qa/schema').NoConsoleErrorAssertion, ctx: AssertionContext): AssertionOutcome {
  const ignorePatterns = (assertion.ignorePatterns ?? []).map((p) => new RegExp(p));
  const offenders = ctx.consoleMessages.filter(
    (m) => m.level === 'error' && !ignorePatterns.some((re) => re.test(m.text)),
  );
  if (offenders.length === 0) return { kind: 'pass' };
  const first = offenders[0];
  if (!first) return { kind: 'pass' };
  return {
    kind: 'fail',
    expected: 'no console errors',
    actual: first.text,
    message: assertion.message ?? `console emitted an error: ${first.text}`,
  };
}

async function safeTextContent(locator: import('playwright').Locator, timeout: number): Promise<string | undefined> {
  try {
    return (await locator.textContent({ timeout })) ?? undefined;
  } catch {
    return undefined;
  }
}

function describeError(error: unknown): string {
  if (error instanceof Error) return error.message;
  return String(error);
}

/** Convenience for tests: assert a type is one of the known enum members. */
export function isAssertionType(value: string): value is AssertionType {
  return (
    value === 'visible' ||
    value === 'hidden' ||
    value === 'textEquals' ||
    value === 'textContains' ||
    value === 'urlMatches' ||
    value === 'elementCount' ||
    value === 'httpStatusNot' ||
    value === 'noConsoleError'
  );
}

export function emptyAssertionResult(index: number, type: AssertionType, status: AssertionResult['status']): AssertionResult {
  return { index, type, status };
}
