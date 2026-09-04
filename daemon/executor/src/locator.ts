/**
 * @fileoverview Locator resolution: the executor's only view of "where".
 *
 * A test step targets an element via `ref` (an id owned by the Application
 * Map) or `locator` (an escape hatch the planner invented). The executor
 * resolves a `ref` by walking the element's locator fallback chain and
 * returning the first chain member that matches **exactly one** element
 * (ADR-004). A raw locator runs but marks the step `unstable: true` and
 * surfaces the count in the report.
 */

import type { Locator, Page } from '@qa/schema';
import type { Locator as PlaywrightLocator } from 'playwright';

/** Outcome of resolving a target on the live page. */
export interface ResolvedTarget {
  /** Playwright locator for the element. Guaranteed to match exactly one element. */
  locator: PlaywrightLocator;
  /**
   * The locator description that actually matched — the one we put in
   * `StepResult.resolvedLocator` so the Failure Analyst knows which strategy
   * the executor fell back to.
   */
  description: string;
  /** `true` for raw-locator targets the planner invented. Always `false` for refs. */
  unstable: boolean;
}

/** Result when no element in the chain matches exactly one element. */
export interface UnresolvedTarget {
  reason: 'no_match' | 'ambiguous';
  /** Every strategy tried, in order. Lets the Failure Analyst explain the failure. */
  tried: string[];
  unstable: boolean;
}

interface PageElement {
  ref: string;
  locators: readonly Locator[];
}

/**
 * Build a single ref → element lookup table from the Application Map.
 *
 * Refs are unique across the whole map (Discovery guarantees this), so a flat
 * Map is enough; pages only matter for the message we surface on miss.
 */
export function indexApplicationMap(
  appMap: Pick<Page, never> & {
    pages: readonly Page[];
  },
): { byRef: Map<string, { element: PageElement; pagePath: string }>; pageByPath: Map<string, Page> } {
  const byRef = new Map<string, { element: PageElement; pagePath: string }>();
  const pageByPath = new Map<string, Page>();
  for (const page of appMap.pages) {
    pageByPath.set(page.path, page);
    for (const element of page.elements) {
      byRef.set(element.ref, { element: { ref: element.ref, locators: element.locators }, pagePath: page.path });
    }
  }
  return { byRef, pageByPath };
}

/**
 * Translate one chain entry into a Playwright Locator.
 *
 * The mapping is deliberately close to ADR-004's wording:
 *   testId → role+name → label → text → css
 * `placeholder`, `altText` and `title` are kept as additional fallback kinds
 * because the Application Map allows them and Playwright supports all three
 * natively — they are not a separate strategy, they are alternative forms of
 * "the page already tells us who this element is".
 */
export function locatorToPlaywright(
  root: PlaywrightLocator,
  entry: Locator,
): PlaywrightLocator {
  switch (entry.kind) {
    case 'testId':
      return root.getByTestId(entry.value);
    case 'role': {
      const name = entry.name ?? entry.value;
      return root.getByRole(entry.value as Parameters<typeof root.getByRole>[0], { name, exact: true });
    }
    case 'label':
      return root.getByLabel(entry.value, { exact: true });
    case 'placeholder':
      return root.getByPlaceholder(entry.value, { exact: true });
    case 'text':
      return root.getByText(entry.value, { exact: true });
    case 'altText':
      return root.getByAltText(entry.value, { exact: true });
    case 'title':
      return root.locator(`[title="${cssEscape(entry.value)}"]`);
    case 'css':
      return root.locator(entry.value);
  }
}

function cssEscape(value: string): string {
  return value.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

/**
 * Resolve a target on the live page.
 *
 * The Playwright `Locator` returned by `getByTestId(...)` etc. is *lazy*: it
 * does not query the page yet. The strict-count check needs to actually run
 * against the DOM, so we use `count()` here and `resolve()` on the first
 * chain member that reports exactly one match.
 */
export async function resolveTarget(
  page: { locator: (selector: string) => PlaywrightLocator },
  appMap: { byRef: Map<string, { element: PageElement; pagePath: string }> },
  target:
    | { ref: string }
    | { locator: string; unstable: true },
): Promise<ResolvedTarget | UnresolvedTarget> {
  if ('locator' in target) {
    const locator = page.locator(target.locator);
    return { locator, description: `raw(${target.locator})`, unstable: true };
  }

  const found = appMap.byRef.get(target.ref);
  if (found === undefined) {
    return {
      reason: 'no_match',
      tried: [`ref:${target.ref}`],
      unstable: false,
    };
  }

  const tried: string[] = [];
  // page.locator('') is the document root in Playwright; we anchor every
  // strategy to the page, not to whatever the previous step left in focus.
  const root = page.locator('html');
  for (const entry of found.element.locators) {
    const description = describeEntry(entry);
    tried.push(description);
    const candidate = locatorToPlaywright(root, entry);
    let count: number;
    try {
      count = await candidate.count();
    } catch (error) {
      // A malformed selector should not take down the test case. We surface
      // it as a no-match for this strategy and keep walking the chain.
      tried[tried.length - 1] = `${description} (selector error: ${describeError(error)})`;
      continue;
    }
    if (count === 1) {
      return { locator: candidate, description, unstable: false };
    }
    if (count > 1) {
      return { reason: 'ambiguous', tried, unstable: false };
    }
  }
  return { reason: 'no_match', tried, unstable: false };
}

function describeEntry(entry: Locator): string {
  if (entry.kind === 'role') return `role:${entry.value}:${entry.name ?? '?'}`;
  return `${entry.kind}:${entry.value}`;
}

function describeError(error: unknown): string {
  if (error instanceof Error) return error.message;
  return String(error);
}
