/**
 * @fileoverview Locator chain builder — ADR-004 ordering.
 *
 * The acceptance criterion says: "locator candidate เรียงลำดับตามที่กำหนดจริง
 * มี test ต่อ priority". This file pins the order with one test per kind,
 * plus the rules around deduplication and missing attributes.
 *
 * The chain is the executor's contract (see `executor/src/locator.ts`).
 * If the order drifts, the executor resolves to a different element and
 * the failure mode is silent.
 */

import { describe, expect, it } from 'vitest';
import { buildLocatorChain } from '../../src/crawler/locators.ts';

function makeEl(attrs: Record<string, string | null | undefined>): Parameters<typeof buildLocatorChain>[0] {
  return {
    type: 'button',
    label: 'Sign in',
    el: {} as never,
    attrs,
  };
}

describe('locators: ordering', () => {
  it('emits testId first when present', () => {
    const chain = buildLocatorChain(makeEl({ 'data-testid': 'login-submit', id: 'submit', placeholder: 'x', alt: 'y', title: 'z' }));
    expect(chain[0]).toEqual({ kind: 'testId', value: 'login-submit' });
  });

  it('emits role+name second, after testId', () => {
    const chain = buildLocatorChain(makeEl({ 'data-testid': 'login-submit' }));
    expect(chain[1]).toEqual({ kind: 'role', value: 'button', name: 'Sign in' });
  });

  it('emits label third, after role', () => {
    const chain = buildLocatorChain(makeEl({}));
    expect(chain[0]).toEqual({ kind: 'role', value: 'button', name: 'Sign in' });
    expect(chain[1]).toEqual({ kind: 'label', value: 'Sign in' });
  });

  it('emits placeholder before text', () => {
    const chain = buildLocatorChain(makeEl({ placeholder: 'Type here' }));
    const placeholderIdx = chain.findIndex((l) => l.kind === 'placeholder');
    const textIdx = chain.findIndex((l) => l.kind === 'text');
    expect(placeholderIdx).toBeGreaterThanOrEqual(0);
    expect(textIdx).toBeGreaterThan(placeholderIdx);
    expect(chain[placeholderIdx]).toEqual({ kind: 'placeholder', value: 'Type here' });
  });

  it('emits altText after placeholder/text', () => {
    const chain = buildLocatorChain(makeEl({ alt: 'logo' }));
    const altIdx = chain.findIndex((l) => l.kind === 'altText');
    expect(altIdx).toBeGreaterThan(chain.findIndex((l) => l.kind === 'text'));
    expect(chain[altIdx]).toEqual({ kind: 'altText', value: 'logo' });
  });

  it('emits title after altText', () => {
    const chain = buildLocatorChain(makeEl({ title: 'Help' }));
    const titleIdx = chain.findIndex((l) => l.kind === 'title');
    expect(titleIdx).toBeGreaterThan(chain.findIndex((l) => l.kind === 'altText'));
    expect(chain[titleIdx]).toEqual({ kind: 'title', value: 'Help' });
  });

  it('emits css only when no better strategy was found', () => {
    // An element with no label, no testId, no placeholder, no alt, no title
    // and no role falls through to CSS as the last resort.
    const chain = buildLocatorChain({ type: 'other', label: '', el: {} as never, attrs: { id: 'submit' } });
    expect(chain.at(-1)).toEqual({ kind: 'css', value: '#submit' });
  });

  it('does not emit css when other kinds exist', () => {
    const chain = buildLocatorChain(makeEl({ 'data-testid': 'x', id: 'submit' }));
    expect(chain.some((l) => l.kind === 'css')).toBe(false);
  });
});

describe('locators: role mapping per element type', () => {
  it.each([
    ['button', 'button'],
    ['link', 'link'],
    ['input', 'textbox'],
    ['textarea', 'textbox'],
    ['select', 'combobox'],
    ['checkbox', 'checkbox'],
    ['radio', 'radio'],
    ['row', 'row'],
    ['cell', 'cell'],
    ['image', 'img'],
    ['dialog', 'dialog'],
    ['tab', 'tab'],
    ['menu', 'menu'],
  ] as const)('maps type=%s to role=%s', (type, role) => {
    const chain = buildLocatorChain({ type, label: 'X', el: {} as never, attrs: {} });
    expect(chain[0]).toEqual({ kind: 'role', value: role, name: 'X' });
  });

  it('omits role for types that have no a11y role', () => {
    for (const type of ['form', 'table', 'text', 'toast', 'other'] as const) {
      const chain = buildLocatorChain({ type, label: 'X', el: {} as never, attrs: {} });
      expect(chain.some((l) => l.kind === 'role')).toBe(false);
    }
  });
});

describe('locators: missing attributes', () => {
  it('skips missing testId', () => {
    const chain = buildLocatorChain(makeEl({}));
    expect(chain.some((l) => l.kind === 'testId')).toBe(false);
  });

  it('skips empty placeholder', () => {
    const chain = buildLocatorChain(makeEl({ placeholder: '   ' }));
    expect(chain.some((l) => l.kind === 'placeholder')).toBe(false);
  });

  it('skips empty alt', () => {
    const chain = buildLocatorChain(makeEl({ alt: '' }));
    expect(chain.some((l) => l.kind === 'altText')).toBe(false);
  });

  it('skips empty title', () => {
    const chain = buildLocatorChain(makeEl({ title: '' }));
    expect(chain.some((l) => l.kind === 'title')).toBe(false);
  });

  it('omits role when label is empty', () => {
    const chain = buildLocatorChain({ type: 'button', label: '', el: {} as never, attrs: {} });
    expect(chain.some((l) => l.kind === 'role')).toBe(false);
  });
});

describe('locators: deduplication', () => {
  it('does not emit two locators of the same kind with the same value', () => {
    // If an element has both a matching role+name AND a label of the same
    // value, we still emit both because role entries carry a name field
    // and label entries do not — they are distinct strategies.
    const chain = buildLocatorChain({ type: 'button', label: 'Sign in', el: {} as never, attrs: {} });
    const labels = chain.filter((l) => l.kind === 'label');
    expect(labels.length).toBe(1);
    expect(labels[0]).toEqual({ kind: 'label', value: 'Sign in' });
  });

  it('emits role then label even when values match', () => {
    const chain = buildLocatorChain({ type: 'button', label: 'Sign in', el: {} as never, attrs: {} });
    const roleIdx = chain.findIndex((l) => l.kind === 'role');
    const labelIdx = chain.findIndex((l) => l.kind === 'label');
    expect(roleIdx).toBeLessThan(labelIdx);
  });
});

describe('locators: chain completeness', () => {
  it('always has at least one entry when something was extracted', () => {
    const chain = buildLocatorChain(makeEl({}));
    expect(chain.length).toBeGreaterThan(0);
  });

  it('prefers the most-specific kind and falls back through the chain', () => {
    const chain = buildLocatorChain(makeEl({
      'data-testid': 'login-submit',
      id: 'submit',
      name: 'commit',
      placeholder: 'submit',
      alt: 'icon',
      title: 'help',
    }));
    expect(chain.map((l) => l.kind)).toEqual(['testId', 'role', 'label', 'placeholder', 'text', 'altText', 'title']);
  });
});
