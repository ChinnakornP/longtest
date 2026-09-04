'use client';

import { useEffect } from 'react';

import { useOrgStore } from '@/lib/stores/org-store';

import { useMe } from './use-me';

/**
 * Resolves which org is "active": the one persisted in useOrgStore if it's
 * still in the user's org list, otherwise the first org. Self-heals the
 * store (e.g. after switching accounts, or losing membership) so callers
 * never have to null-check a stale id.
 */
export function useActiveOrg() {
  const { data: me, isLoading } = useMe();
  const activeOrgId = useOrgStore((s) => s.activeOrgId);
  const setActiveOrgId = useOrgStore((s) => s.setActiveOrgId);

  const orgs = me?.orgs ?? [];
  const activeOrg = orgs.find((org) => org.id === activeOrgId) ?? orgs[0] ?? null;

  useEffect(() => {
    if (activeOrg && activeOrg.id !== activeOrgId) {
      setActiveOrgId(activeOrg.id);
    }
  }, [activeOrg, activeOrgId, setActiveOrgId]);

  return { activeOrg, orgs, isLoading };
}
