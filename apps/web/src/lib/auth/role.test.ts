import { describe, expect, it } from 'vitest';

import { canManage, canWrite } from './role';

describe('canWrite', () => {
  it('is false for viewer and undefined', () => {
    expect(canWrite('viewer')).toBe(false);
    expect(canWrite(undefined)).toBe(false);
  });

  it('is true for member, admin and owner', () => {
    expect(canWrite('member')).toBe(true);
    expect(canWrite('admin')).toBe(true);
    expect(canWrite('owner')).toBe(true);
  });
});

describe('canManage', () => {
  it('is false for viewer and member', () => {
    expect(canManage('viewer')).toBe(false);
    expect(canManage('member')).toBe(false);
  });

  it('is true for admin and owner only', () => {
    expect(canManage('admin')).toBe(true);
    expect(canManage('owner')).toBe(true);
  });
});
