import { beforeEach, describe, expect, it } from 'vitest';

import { useOrgStore } from './org-store';

describe('useOrgStore', () => {
  beforeEach(() => {
    localStorage.clear();
    useOrgStore.getState().setActiveOrgId(null);
  });

  it('updates the active org id', () => {
    useOrgStore.getState().setActiveOrgId('org-1');
    expect(useOrgStore.getState().activeOrgId).toBe('org-1');
  });

  it('persists the selection so it survives a reload', () => {
    useOrgStore.getState().setActiveOrgId('org-2');
    const persisted = JSON.parse(localStorage.getItem('qa-active-org') ?? '{}') as {
      state?: { activeOrgId?: string };
    };
    expect(persisted.state?.activeOrgId).toBe('org-2');
  });
});
