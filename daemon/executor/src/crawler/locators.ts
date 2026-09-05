/**
 * @fileoverview Locator candidate builder — ordered fallback chain per
 * ADR-004.
 *
 * Every interactive element we discover becomes a chain in this order:
 *
 *   testId → role+name → label → placeholder → text → altText → title → css
 *
 * The order is the executor's contract (see `executor/src/locator.ts`),
 * not a stylistic choice: the executor walks the chain top to bottom and
 * stops at the first entry that resolves to exactly one element on the
 * live page. Re-ordering changes the resolver's decisions, which changes
 * which locator ends up in `StepResult.resolvedLocator`, which changes the
 * Failure Analyst's signal.
 *
 * The locator kind set matches `application-map@1`. We never emit a kind
 * outside that enum.
 */
import type { Locator as PlaywrightLocator } from 'playwright';
import type { RawElementType, RawLocator } from './types.ts';

const ROLE_BY_ELEMENT: Readonly<Record<RawElementType, string | null>> = {
  button: 'button',
  link: 'link',
  input: 'textbox',
  textarea: 'textbox',
  select: 'combobox',
  checkbox: 'checkbox',
  radio: 'radio',
  form: null,
  table: null,
  row: 'row',
  cell: 'cell',
  text: null,
  image: 'img',
  dialog: 'dialog',
  tab: 'tab',
  menu: 'menu',
  toast: null,
  other: null,
};

/** Inputs the locator builder reads off the DOM. */
export interface RawDiscoveredElement {
  type: RawElementType;
  label: string;
  /** DOM handle, used to read attributes that are not in `attrs`. */
  el: PlaywrightLocator;
  /** Snapshot of relevant attributes. Missing keys are treated as absent. */
  attrs: Readonly<Record<string, string | null | undefined>>;
}

/**
 * Build the ordered ADR-004 chain for one element. Returns the entries that
 * have a real value (we do not emit empty testIds or empty roles) and
 * de-duplicates by `kind` so an element that happens to expose a label and
 * a role does not end up with two `label` entries.
 *
 * The first entry that resolves to exactly one element on the page wins,
 * so the chain is ordered most-specific → least-specific.
 */
export function buildLocatorChain(d: RawDiscoveredElement): RawLocator[] {
  const out: RawLocator[] = [];
  const seen = new Set<string>();

  const push = (loc: RawLocator): void => {
    if (!seen.has(`${loc.kind}:${loc.value}${loc.name !== undefined ? `:${loc.name}` : ''}`)) {
      seen.add(`${loc.kind}:${loc.value}${loc.name !== undefined ? `:${loc.name}` : ''}`);
      out.push(loc);
    }
  };

  const testId = firstNonEmpty(d.attrs['data-testid']);
  if (testId !== null) push({ kind: 'testId', value: testId });

  const role = ROLE_BY_ELEMENT[d.type];
  if (role !== null && d.label !== '') {
    push({ kind: 'role', value: role, name: d.label });
  }

  if (d.label !== '') push({ kind: 'label', value: d.label });

  const placeholder = firstNonEmpty(d.attrs['placeholder']);
  if (placeholder !== null) push({ kind: 'placeholder', value: placeholder });

  if (d.label !== '') push({ kind: 'text', value: d.label });

  const alt = firstNonEmpty(d.attrs['alt']);
  if (alt !== null) push({ kind: 'altText', value: alt });

  const title = firstNonEmpty(d.attrs['title']);
  if (title !== null) push({ kind: 'title', value: title });

  // CSS last. We only emit it when nothing better was found — emitting it
  // higher up would push the executor onto a strategy that breaks on every
  // CSS refactor, which is exactly the regression ADR-004 exists to prevent.
  if (out.length === 0) {
    const css = synthesizeCssSelector(d);
    if (css !== null) push({ kind: 'css', value: css });
  }

  return out;
}

/**
 * Try to build a CSS selector that uniquely identifies the element. The
 * rule is the same as ADR-004: only as a last resort. We prefer tag + a
 * single attribute that is reasonably stable; if nothing stable is on the
 * element we return null and let the element end up without a locator (the
 * chain will still have at least the role/label from above).
 */
function synthesizeCssSelector(d: RawDiscoveredElement): string | null {
  const tag = d.type;
  const id = firstNonEmpty(d.attrs['id']);
  if (id !== null) return `#${cssEscape(id)}`;
  const testId = firstNonEmpty(d.attrs['data-testid']);
  if (testId !== null) return `${tag}[data-testid="${cssEscape(testId)}"]`;
  const name = firstNonEmpty(d.attrs['name']);
  if (name !== null) return `${tag}[name="${cssEscape(name)}"]`;
  return null;
}

function firstNonEmpty(value: string | null | undefined): string | null {
  if (value === null || value === undefined) return null;
  const trimmed = value.trim();
  return trimmed === '' ? null : trimmed;
}

function cssEscape(value: string): string {
  return value.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}
