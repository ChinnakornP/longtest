/**
 * @fileoverview Action runtime: the executor's deterministic verbs.
 *
 * Every enum member in `STEP_ACTION_VALUES` must have an implementation here.
 * The runner calls into this module by `StepAction`, so the contract check
 * ("action not in enum → error") lives in the runner before we ever get
 * here. Inside this file the rule is the inverse: if you add an action to
 * the schema, you must add it here, and you must add a test in
 * `test/actions.test.ts` that proves the new code path is exercised.
 */

import type {
  CheckStep,
  ClickStep,
  FillStep,
  HoverStep,
  NavigateStep,
  PressStep,
  ScreenshotStep,
  SelectStep,
  WaitForStep,
} from '@qa/schema';
import type { Page, Locator as PlaywrightLocator } from 'playwright';
import { resolveTarget, type ResolvedTarget, type UnresolvedTarget } from './locator.ts';

export interface ActionContext {
  page: Page;
  appMap: { byRef: Map<string, { element: { ref: string; locators: readonly import('@qa/schema').Locator[] }; pagePath: string }> };
  baseUrl: string;
  stepTimeoutMs: number;
  defaultStepTimeoutMs: number;
}

export type ActionResult =
  | { kind: 'ok'; resolved: ResolvedTarget | null; unstable: boolean; extra?: Record<string, unknown> }
  | { kind: 'unresolved'; unresolved: UnresolvedTarget };

/**
 * Run one step. Returns either `ok` (with the resolved locator so the runner
 * can attach it to StepResult) or `unresolved` (so the runner can turn it
 * into a `TARGET_NOT_FOUND` error response).
 */
export async function runStep(step: import('@qa/schema').Step, ctx: ActionContext): Promise<ActionResult> {
  const timeout = step.timeoutMs ?? ctx.stepTimeoutMs ?? ctx.defaultStepTimeoutMs;

  switch (step.action) {
    case 'navigate':
      return runNavigate(step, ctx);
    case 'click':
      return runClick(step, ctx, timeout);
    case 'fill':
      return runFill(step, ctx, timeout);
    case 'select':
      return runSelect(step, ctx, timeout);
    case 'check':
      return runCheck(step, ctx, timeout);
    case 'hover':
      return runHover(step, ctx, timeout);
    case 'press':
      return runPress(step, ctx, timeout);
    case 'waitFor':
      return runWaitFor(step, ctx, timeout);
    case 'screenshot':
      return runScreenshot(step, ctx);
  }
}

function resolveOrFail(
  ctx: ActionContext,
  target: import('@qa/schema').Target,
): Promise<{ ok: ResolvedTarget } | { fail: UnresolvedTarget }> {
  return resolveTarget(ctx.page, ctx.appMap, target).then((r) =>
    'locator' in r ? { ok: r } : { fail: r },
  );
}

async function runNavigate(step: NavigateStep, ctx: ActionContext): Promise<ActionResult> {
  const url = resolveUrl(step.url, ctx.baseUrl);
  const waitUntil = step.waitUntil ?? 'load';
  const timeout = step.timeoutMs ?? ctx.defaultStepTimeoutMs;
  await ctx.page.goto(url, { waitUntil, timeout });
  return { kind: 'ok', resolved: null, unstable: false };
}

async function runClick(step: ClickStep, ctx: ActionContext, timeout: number): Promise<ActionResult> {
  const resolved = await resolveOrFail(ctx, step.target);
  if ('fail' in resolved) return { kind: 'unresolved', unresolved: resolved.fail };
  const r = resolved.ok;
  const opts: Parameters<typeof r.locator.click>[0] = { timeout };
  if (step.button !== undefined) opts.button = step.button;
  if (step.clickCount !== undefined) opts.clickCount = step.clickCount;
  await r.locator.click(opts);
  return { kind: 'ok', resolved: r, unstable: r.unstable };
}

async function runFill(step: FillStep, ctx: ActionContext, timeout: number): Promise<ActionResult> {
  const resolved = await resolveOrFail(ctx, step.target);
  if ('fail' in resolved) return { kind: 'unresolved', unresolved: resolved.fail };
  const r = resolved.ok;
  await r.locator.fill(step.value, { timeout });
  return { kind: 'ok', resolved: r, unstable: r.unstable };
}

async function runSelect(step: SelectStep, ctx: ActionContext, timeout: number): Promise<ActionResult> {
  const resolved = await resolveOrFail(ctx, step.target);
  if ('fail' in resolved) return { kind: 'unresolved', unresolved: resolved.fail };
  const r = resolved.ok;
  const by = step.by ?? 'value';
  const value = step.value;
  await r.locator.selectOption(by === 'value' ? { value } : by === 'label' ? { label: value } : { index: Number(value) }, { timeout });
  return { kind: 'ok', resolved: r, unstable: r.unstable };
}

async function runCheck(step: CheckStep, ctx: ActionContext, timeout: number): Promise<ActionResult> {
  const resolved = await resolveOrFail(ctx, step.target);
  if ('fail' in resolved) return { kind: 'unresolved', unresolved: resolved.fail };
  const r = resolved.ok;
  const desired = step.checked ?? true;
  const isChecked = await r.locator.isChecked({ timeout });
  if (isChecked !== desired) {
    await r.locator.click({ timeout });
  }
  return { kind: 'ok', resolved: r, unstable: r.unstable };
}

async function runHover(step: HoverStep, ctx: ActionContext, timeout: number): Promise<ActionResult> {
  const resolved = await resolveOrFail(ctx, step.target);
  if ('fail' in resolved) return { kind: 'unresolved', unresolved: resolved.fail };
  const r = resolved.ok;
  await r.locator.hover({ timeout });
  return { kind: 'ok', resolved: r, unstable: r.unstable };
}

async function runPress(step: PressStep, ctx: ActionContext, timeout: number): Promise<ActionResult> {
  let locator: PlaywrightLocator | undefined;
  let unstable = false;
  if (step.target) {
    const resolved = await resolveOrFail(ctx, step.target);
    if ('fail' in resolved) return { kind: 'unresolved', unresolved: resolved.fail };
    locator = resolved.ok.locator;
    unstable = resolved.ok.unstable;
  }
  if (locator) {
    await locator.press(step.key, { timeout });
  } else {
    await ctx.page.keyboard.press(step.key);
  }
  return {
    kind: 'ok',
    resolved: locator
      ? { locator, description: 'press:page-level', unstable }
      : null,
    unstable,
  };
}

async function runWaitFor(step: WaitForStep, ctx: ActionContext, timeout: number): Promise<ActionResult> {
  const resolved = await resolveOrFail(ctx, step.target);
  if ('fail' in resolved) {
    // Waiting for something to go away is satisfied by it never having been
    // there. Same reasoning as the `hidden` assertion: reporting
    // TARGET_NOT_FOUND would make "wait until the dialog is gone" impossible
    // to express for a dialog that closed before the step ran.
    if (resolved.fail.reason === 'no_match' && (step.state === 'hidden' || step.state === 'detached')) {
      return { kind: 'ok', resolved: null, unstable: false };
    }
    return { kind: 'unresolved', unresolved: resolved.fail };
  }
  const r = resolved.ok;
  await r.locator.waitFor({ state: step.state, timeout });
  return { kind: 'ok', resolved: r, unstable: r.unstable };
}

async function runScreenshot(_step: ScreenshotStep, _ctx: ActionContext): Promise<ActionResult> {
  // The runner captures screenshots on demand via the evidence module, so
  // explicit `screenshot` steps are a no-op from the executor's side. The
  // schema reserves the action for symmetry with the planner, and we want
  // it to be a no-op rather than a duplicate of evidence so the runner
  // never captures the same image twice.
  return { kind: 'ok', resolved: null, unstable: false };
}

export function resolveUrl(url: string, baseUrl: string): string {
  if (/^https?:\/\//.test(url)) return url;
  // Strip trailing slash from baseUrl so `/x` does not become `//x`.
  return `${baseUrl.replace(/\/+$/, '')}${url}`;
}
