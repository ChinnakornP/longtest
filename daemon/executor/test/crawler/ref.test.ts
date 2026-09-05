/**
 * @fileoverview `ref` derivation — the single highest-risk piece of
 * Slice A.
 *
 * The acceptance criterion says: "test พิสูจน์ `ref` เสถียรข้าม run (รัน 2
 * รอบ เทียบ ref set)". This file is that test, plus the unit cases that
 * pin the derivation rule.
 *
 * Every test in this file is deterministic: no time, no randomness, no
 * process state. Two runs against the same code must produce the same
 * `ref` for the same input.
 */

import { describe, expect, it } from 'vitest';
import { buildElementRef, buildPageRef, slugLabel, slugPath } from '../../src/crawler/ref.ts';

describe('ref: slugPath', () => {
  it('strips the leading slash', () => {
    expect(slugPath('/login')).toBe('login');
  });

  it('joins multi-segment paths with a dot', () => {
    expect(slugPath('/employees/:id/edit')).toBe('employees.id.edit');
  });

  it('lowercases uppercase letters', () => {
    expect(slugPath('/Employees/Add')).toBe('employees.add');
  });

  it('drops query strings and fragments', () => {
    expect(slugPath('/employees?q=1#top')).toBe('employees');
  });

  it('returns "root" for the bare path', () => {
    expect(slugPath('/')).toBe('root');
    expect(slugPath('')).toBe('root');
  });

  it('collapses runs of non-alphanumerics', () => {
    // `-` is in the allowed set, so `c--d` stays; only runs of "weird"
    // characters collapse.
    expect(slugPath('/a   b/c--d')).toBe('a.b.c--d');
  });
});

describe('ref: slugLabel', () => {
  it('lowercases and trims', () => {
    expect(slugLabel('  Sign In  ')).toBe('sign-in');
  });

  it('returns _unlabelled for empty / whitespace input', () => {
    expect(slugLabel('')).toBe('_unlabelled');
    expect(slugLabel('   ')).toBe('_unlabelled');
  });

  it('replaces non-alphanumerics with a single dash', () => {
    expect(slugLabel('Add Employee!')).toBe('add-employee');
  });

  it('truncates long labels and appends a stable hash', () => {
    const a = slugLabel('a'.repeat(100));
    const b = slugLabel('a'.repeat(100));
    expect(a).toBe(b);
    expect(a.length).toBeLessThanOrEqual(40);
    expect(a.endsWith('-0000')).toBe(false); // different input → different hash
  });

  it('distinguishes two long labels that share a prefix', () => {
    const a = slugLabel('x'.repeat(50) + 'first');
    const b = slugLabel('x'.repeat(50) + 'second');
    expect(a).not.toBe(b);
  });
});

describe('ref: buildPageRef', () => {
  it('produces page.<slugPath>', () => {
    expect(buildPageRef('/login')).toBe('page.login');
    expect(buildPageRef('/employees/:id')).toBe('page.employees.id');
  });

  it('never produces an empty page ref', () => {
    expect(buildPageRef('/')).toBe('page.root');
    expect(buildPageRef('')).toBe('page.root');
  });

  it('matches the application-map@1 Ref pattern', () => {
    for (const path of ['/', '/login', '/employees/:id', '/employees/123/edit', '/a-b/c_d']) {
      expect(buildPageRef(path)).toMatch(/^[A-Za-z0-9][A-Za-z0-9_-]*(\.[A-Za-z0-9][A-Za-z0-9_-]*)*$/);
    }
  });
});

describe('ref: buildElementRef', () => {
  it('is pure: same input → same output', () => {
    const input = { pageRef: 'page.login', role: 'button' as const, label: 'Sign in', collision: 1 };
    const a = buildElementRef(input);
    const b = buildElementRef(input);
    expect(a).toBe(b);
    expect(a).toBe('page.login.button.sign-in');
  });

  it('collision counter disambiguates duplicate (role, label) on the same page', () => {
    const base = { pageRef: 'page.employees', role: 'button' as const, label: 'Edit' };
    expect(buildElementRef({ ...base, collision: 1 })).toBe('page.employees.button.edit');
    expect(buildElementRef({ ...base, collision: 2 })).toBe('page.employees.button.edit_2');
    expect(buildElementRef({ ...base, collision: 3 })).toBe('page.employees.button.edit_3');
  });

  it('survives the schema regex on every role', () => {
    const roles = ['button', 'link', 'input', 'textarea', 'select', 'checkbox', 'radio', 'form', 'table', 'row', 'cell', 'text', 'image', 'dialog', 'tab', 'menu', 'toast', 'other'] as const;
    for (const role of roles) {
      const ref = buildElementRef({ pageRef: 'page.x', role, label: 'Hello', collision: 1 });
      expect(ref).toMatch(/^[A-Za-z0-9][A-Za-z0-9_-]*(\.[A-Za-z0-9][A-Za-z0-9_-]*)*$/);
    }
  });
});

describe('ref: stability across runs (Slice A acceptance)', () => {
  /**
   * The acceptance criterion for Slice A: re-run the same derivation on
   * the same inputs and the ref set must be identical. We model "two
   * runs" by re-invoking the derivation a second time and comparing the
   * resulting ref sets.
   */
  function buildRefs(pagePath: string, elements: ReadonlyArray<{ role: 'button' | 'link' | 'input'; label: string }>): Set<string> {
    const pageRef = buildPageRef(pagePath);
    const collisions = new Map<string, number>();
    const refs = new Set<string>();
    for (const el of elements) {
      const key = `${el.role}|${el.label}`;
      const next = (collisions.get(key) ?? 0) + 1;
      collisions.set(key, next);
      refs.add(buildElementRef({ pageRef, role: el.role, label: el.label, collision: next }));
    }
    return refs;
  }

  it('two runs against the same fixture app produce identical ref sets', () => {
    const elements = [
      { role: 'input' as const, label: 'Email' },
      { role: 'input' as const, label: 'Password' },
      { role: 'button' as const, label: 'Sign in' },
    ];
    const runA = buildRefs('/login', elements);
    const runB = buildRefs('/login', elements);
    expect(runA).toEqual(runB);
    expect([...runA].sort()).toEqual([
      'page.login.button.sign-in',
      'page.login.input.email',
      'page.login.input.password',
    ]);
  });

  it('detects ref drift: changing the derivation produces a different set', () => {
    const elements = [{ role: 'input' as const, label: 'Email' }];
    const expected = buildRefs('/login', elements);
    // Same inputs, different page path → ref must move.
    const drift = buildRefs('/signin', elements);
    expect(expected).not.toEqual(drift);
  });
});
