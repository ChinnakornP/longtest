import { create } from 'zustand';
import { persist } from 'zustand/middleware';

import type { RunMode } from '@/lib/api/qa-types';

/**
 * UI-only nav aid: which runs the current browser started, per project.
 * NOT a substitute for a real run-history endpoint - the T08 contract has
 * no `GET /projects/{id}/runs` (see the T11 PR description). This only
 * remembers what this browser has seen locally so the project page has
 * something to link to; it is not authoritative run state, which always
 * comes from useRun/useRunStream (TanStack Query / the live stream).
 */
export interface RecentRun {
  id: string;
  mode: RunMode;
  startedAt: string;
}

/** Stable reference for "no runs yet" - a selector must never return a fresh `[]` literal on every call, or useSyncExternalStore sees a changed snapshot each render and loops forever. */
export const EMPTY_RECENT_RUNS: RecentRun[] = [];

interface RecentRunsState {
  byProject: Record<string, RecentRun[]>;
  addRun: (projectId: string, run: RecentRun) => void;
}

const MAX_PER_PROJECT = 10;

export const useRecentRunsStore = create<RecentRunsState>()(
  persist(
    (set) => ({
      byProject: {},
      addRun: (projectId, run) =>
        set((state) => {
          const existing = state.byProject[projectId] ?? [];
          const next = [run, ...existing.filter((r) => r.id !== run.id)].slice(0, MAX_PER_PROJECT);
          return { byProject: { ...state.byProject, [projectId]: next } };
        }),
    }),
    { name: 'qa-recent-runs' },
  ),
);
