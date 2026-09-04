import { create } from 'zustand';
import { persist } from 'zustand/middleware';

/**
 * UI-only selection state: which org the user is currently viewing. The org
 * list itself, names, roles etc. are server state and live in TanStack Query
 * (see useMe) — this store only ever holds the id.
 */
interface OrgState {
  activeOrgId: string | null;
  setActiveOrgId: (orgId: string | null) => void;
}

export const useOrgStore = create<OrgState>()(
  persist(
    (set) => ({
      activeOrgId: null,
      setActiveOrgId: (orgId) => set({ activeOrgId: orgId }),
    }),
    { name: 'qa-active-org' },
  ),
);
