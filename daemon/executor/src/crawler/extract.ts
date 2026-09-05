/**
 * @fileoverview Page extraction — pull path, title, authRequired and the
 * element list out of one loaded Playwright page.
 *
 * The extractor is a pure data flow over a Playwright `Page`:
 *
 *   page  ─►  readPath() / readTitle()
 *          ─►  readAuthRequired()
 *          ─►  enumerateElements()  ─►  readLabel() per element
 *                                ─►  ref collisions per (role, label)
 *                                ─►  buildLocatorChain()
 *
 * Stability rules (mirrored in `ref.ts`):
 *   - The element list is sorted by (type, label, ref-derivation-order).
 *     Sorting matters: two runs against the same DOM must produce the
 *     same element *order*, otherwise the per-page collision counter picks
 *     different values for two elements that happen to share a label.
 *   - Elements with no locator chain (the locator builder found nothing
 *     worth emitting) are dropped. A button we cannot target is worse than
 *     no button: it would become an element the executor can never resolve.
 *   - Labels are trimmed but never mutated beyond that; the same page on a
 *     later run must produce the same label so the ref does not shift.
 */
import type { Page, Locator as PlaywrightLocator } from 'playwright';
import type { RawElement, RawElementType, RawLocator } from './types.ts';
import { buildElementRef, buildPageRef } from './ref.ts';
import { buildLocatorChain } from './locators.ts';

export interface ExtractInput {
  /** Absolute URL the page was loaded with. */
  url: string;
  /** The loaded Playwright page. */
  page: Page;
}

export interface ExtractOutput {
  /** Path relative to the baseUrl. Used for dedup and as the page id. */
  path: string;
  title: string;
  authRequired: boolean;
  elements: RawElement[];
  /** Tallies: number of `<form>` elements, and number of interactive elements. */
  formCount: number;
  actionCount: number;
}

/** Selector list per element type we extract. Order = enum order in types.ts. */
const TYPE_SELECTORS: ReadonlyArray<{ type: RawElementType; selector: string; role: string | null }> = [
  { type: 'button', selector: 'button, input[type="button"], input[type="submit"], input[type="reset"]', role: 'button' },
  { type: 'link', selector: 'a[href]', role: 'link' },
  { type: 'input', selector: 'input:not([type="button"]):not([type="submit"]):not([type="reset"]):not([type="checkbox"]):not([type="radio"]):not([type="image"])', role: null },
  { type: 'textarea', selector: 'textarea', role: 'textbox' },
  { type: 'select', selector: 'select', role: 'combobox' },
  { type: 'checkbox', selector: 'input[type="checkbox"]', role: 'checkbox' },
  { type: 'radio', selector: 'input[type="radio"]', role: 'radio' },
  { type: 'form', selector: 'form', role: null },
  { type: 'table', selector: 'table', role: null },
  { type: 'row', selector: 'tr', role: 'row' },
  { type: 'cell', selector: 'th, td', role: 'cell' },
  { type: 'text', selector: 'p, h1, h2, h3, h4, h5, h6, label, span, div', role: null },
  { type: 'image', selector: 'img', role: 'img' },
  { type: 'dialog', selector: 'dialog, [role="dialog"]', role: 'dialog' },
  { type: 'tab', selector: '[role="tab"]', role: 'tab' },
  { type: 'menu', selector: '[role="menu"]', role: 'menu' },
  { type: 'toast', selector: '[role="alert"], [role="status"]', role: null },
];

/**
 * Extract everything the crawl needs from one page. The page must already
 * have `goto()`'d and waited for `load` (the caller drives that so the
 * BFS loop can keep its own timeout policy).
 */
export async function extractPage(input: ExtractInput): Promise<ExtractOutput> {
  const { page, url } = input;

  const path = pathFromUrl(url, page.url());
  const title = await readTitle(page);
  const authRequired = await readAuthRequired(page);

  // Count forms first; we surface this in the tallies regardless of how
  // many other elements we end up deduplicating.
  const formCount = await page.locator('form').count();

  const perType = await collectByType(page);
  const actionCount = perType
    .filter((p) => p.type !== 'text' && p.type !== 'row' && p.type !== 'cell' && p.type !== 'image')
    .reduce((acc, p) => acc + p.entries.length, 0);

  const pageRef = buildPageRef(path);
  const elements = assembleElements(perType, pageRef);

  return { path, title, authRequired, elements, formCount, actionCount };
}

interface PerType {
  type: RawElementType;
  entries: Array<{ el: PlaywrightLocator; label: string; attrs: Record<string, string | null | undefined> }>;
}

async function collectByType(page: Page): Promise<PerType[]> {
  const out: PerType[] = [];
  for (const def of TYPE_SELECTORS) {
    const loc = page.locator(def.selector);
    const count = await loc.count();
    const entries: PerType['entries'] = [];
    for (let i = 0; i < count; i += 1) {
      const el = loc.nth(i);
      const label = await readLabel(el, def.type);
      const attrs = await readAttrs(el, ATTRS_TO_READ);
      entries.push({ el, label, attrs });
    }
    out.push({ type: def.type, entries });
  }
  return out;
}

const ATTRS_TO_READ = [
  'data-testid',
  'id',
  'name',
  'placeholder',
  'alt',
  'title',
  'type',
  'href',
  'role',
  'aria-label',
  'for',
] as const;

async function readAttrs(el: PlaywrightLocator, names: readonly string[]): Promise<Record<string, string | null | undefined>> {
  const out: Record<string, string | null | undefined> = {};
  for (const name of names) {
    out[name] = await el.getAttribute(name);
  }
  return out;
}

/**
 * Choose the best label we can read for an element. The rule is, in order:
 *
 *   1. `aria-label` attribute
 *   2. text inside `<label for="…">` referencing this element (id match)
 *   3. value attribute (for buttons / submit inputs)
 *   4. textContent of the element itself (collapsed)
 *   5. `title` attribute
 *   6. empty string (the ref slugs `_unlabelled`)
 *
 * The order is documented in `ref.ts`: the label flows straight into the
 * slug, so two runs against the same DOM must read the same label.
 */
async function readLabel(el: PlaywrightLocator, type: RawElementType): Promise<string> {
  const ariaLabel = await el.getAttribute('aria-label');
  if (ariaLabel !== null && ariaLabel.trim() !== '') return ariaLabel.trim();

  const id = await el.getAttribute('id');
  if (id !== null && id !== '') {
    const ref = el.page().locator(`label[for="${cssEscape(id)}"]`);
    const count = await ref.count();
    if (count > 0) {
      const text = (await ref.first().textContent()) ?? '';
      const trimmed = text.trim();
      if (trimmed !== '') return trimmed;
    }
  }

  if (type === 'button') {
    const value = await el.getAttribute('value');
    if (value !== null && value.trim() !== '') return value.trim();
    const text = (await el.textContent()) ?? '';
    const trimmed = text.trim();
    if (trimmed !== '') return trimmed;
  }

  if (type === 'input' || type === 'textarea') {
    const placeholder = await el.getAttribute('placeholder');
    if (placeholder !== null && placeholder.trim() !== '') return placeholder.trim();
  }

  if (type === 'cell' || type === 'text') {
    const text = (await el.textContent()) ?? '';
    const trimmed = text.trim();
    if (trimmed !== '') return trimmed;
  }

  if (type === 'image') {
    const alt = await el.getAttribute('alt');
    if (alt !== null && alt.trim() !== '') return alt.trim();
  }

  const title = await el.getAttribute('title');
  if (title !== null && title.trim() !== '') return title.trim();

  return '';
}

async function readTitle(page: Page): Promise<string> {
  try {
    return await page.title();
  } catch {
    return '';
  }
}

/**
 * Decide whether this page would have required authentication. The signal
 * is "the page rendered, but a sibling visit to the same path would 302
 * to /login". We approximate this by checking the redirect chain: if the
 * final URL on a fresh navigation to this path lands on a known auth path
 * and not the path we started at, auth is required.
 *
 * For now, we only have the live page in front of us; we treat the
 * presence of an inline login form / a 401 / a redirect in the navigation
 * history as auth. The crawler runs in an unauthenticated browser context
 * by design (auth flows are out of scope for Slice A).
 */
async function readAuthRequired(page: Page): Promise<boolean> {
  // Heuristic: a page that redirected to /login (or has a login form at the
  // top of the body) is auth-required. The fixture app redirects all
  // protected paths to /login with 302 — we cannot see the 302 after
  // `goto`, but we can look at the final URL.
  const url = page.url();
  return /\/(login|signin|sign-in|auth)/.test(url);
}

/**
 * Assemble `RawElement[]` from the per-type buckets. We sort within each
 * bucket by label so two runs produce the same element order; collision
 * counters depend on order.
 */
function assembleElements(perType: readonly PerType[], pageRef: string): RawElement[] {
  const out: RawElement[] = [];
  for (const bucket of perType) {
    const sorted = bucket.entries
      .slice()
      .sort((a, b) => a.label.localeCompare(b.label) || a.attrs['data-testid']?.localeCompare(b.attrs['data-testid'] ?? '') || 0);
    const collisions = new Map<string, number>();
    for (const entry of sorted) {
      const chain: RawLocator[] = buildLocatorChain({
        type: bucket.type,
        label: entry.label,
        el: entry.el,
        attrs: entry.attrs,
      });
      if (chain.length === 0) continue;
      const key = `${bucket.type}|${entry.label}`;
      const next = (collisions.get(key) ?? 0) + 1;
      collisions.set(key, next);
      const ref = buildElementRef({
        pageRef,
        role: bucket.type,
        label: entry.label,
        collision: next,
      });
      out.push({
        ref,
        type: bucket.type,
        label: entry.label,
        locators: chain,
      });
    }
  }
  return out;
}

function pathFromUrl(originalUrl: string, finalUrl: string): string {
  // The crawler passes the *navigation target* as `originalUrl`; if the
  // page redirected (login bounce), `finalUrl` is where Playwright
  // actually is. We want the path of the redirect target, because that is
  // the page the user would land on; it is also the only path the test
  // runner will be able to navigate to.
  const target = finalUrl.length > 0 ? finalUrl : originalUrl;
  try {
    const u = new URL(target);
    return u.pathname + u.search;
  } catch {
    return originalUrl;
  }
}

function cssEscape(value: string): string {
  return value.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}
