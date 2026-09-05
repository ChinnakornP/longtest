/**
 * @fileoverview Path template dedup — collapse `/employees/1`,
 * `/employees/2` into `/employees/:id`.
 *
 * The acceptance criterion says: "test พิสูจน์ template dedup ยุบ
 * `/employees/:id` ได้". This file is that test.
 */

import { describe, expect, it } from 'vitest';
import { collapsePaths, sameShape } from '../../src/crawler/dedup.ts';

describe('dedup: same-shape collapse', () => {
  it('collapses /employees/1, /employees/2 into /employees/:id', () => {
    const out = collapsePaths(['/employees/1', '/employees/2']);
    expect(out).toEqual([{ path: '/employees/:id', pattern: '/employees/:id' }]);
  });

  it('collapses /employees/1/edit, /employees/2/edit into /employees/:id/edit', () => {
    const out = collapsePaths(['/employees/1/edit', '/employees/2/edit']);
    expect(out).toEqual([{ path: '/employees/:id/edit', pattern: '/employees/:id/edit' }]);
  });

  it('collapses three concrete paths to one template', () => {
    const out = collapsePaths(['/orders/a-1', '/orders/a-2', '/orders/a-3']);
    expect(out).toEqual([{ path: '/orders/:id', pattern: '/orders/:id' }]);
  });

  it('keeps a single path as-is', () => {
    expect(collapsePaths(['/employees/1'])).toEqual([{ path: '/employees/1', pattern: '/employees/1' }]);
  });

  it('returns empty for empty input', () => {
    expect(collapsePaths([])).toEqual([]);
  });
});

describe('dedup: when not to collapse', () => {
  it('does not collapse when segment counts differ', () => {
    const out = collapsePaths(['/employees', '/employees/1']);
    expect(out).toEqual([
      { path: '/employees', pattern: '/employees' },
      { path: '/employees/1', pattern: '/employees/1' },
    ]);
  });

  it('does not collapse when one path is a substring of another', () => {
    // Different segment counts — `/a` has 1, `/a/1` has 2.
    const out = collapsePaths(['/employees', '/employees/1']);
    expect(out).toEqual([
      { path: '/employees', pattern: '/employees' },
      { path: '/employees/1', pattern: '/employees/1' },
    ]);
  });

  it('collapses two paths whose every position differs', () => {
    // `/a/1` and `/b/2` share shape (2 segments each); both positions vary,
    // so both become `:id`. This is the most general collapse.
    const out = collapsePaths(['/a/1', '/b/2']);
    expect(out).toEqual([{ path: '/:id/:id', pattern: '/:id/:id' }]);
  });

  it('collapses only the differing position, leaves matching static segments', () => {
    const out = collapsePaths(['/employees/1/details', '/employees/2/details']);
    expect(out).toEqual([{ path: '/employees/:id/details', pattern: '/employees/:id/details' }]);
  });
});

describe('dedup: sameShape', () => {
  it('returns true for paths that share structure', () => {
    expect(sameShape('/employees/1', '/employees/2')).toBe(true);
    expect(sameShape('/employees/1/edit', '/employees/2/edit')).toBe(true);
  });

  it('returns false for paths with different segment counts', () => {
    expect(sameShape('/employees', '/employees/1')).toBe(false);
    expect(sameShape('/employees/1', '/employees/1/edit')).toBe(false);
  });
});
