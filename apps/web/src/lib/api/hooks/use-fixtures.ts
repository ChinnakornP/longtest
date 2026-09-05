import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { Fixture, FixtureListResponse, RegisterFixtureRequest } from '@/lib/api/qa-types';

export function fixturesQueryKey(orgId: string | null, projectId: string) {
  return ['fixtures', orgId, projectId] as const;
}

/** GET /api/v1/projects/{projectId}/fixtures — names and descriptions only, never credentials. */
export function useFixtures(orgId: string | null, projectId: string) {
  return useQuery({
    queryKey: fixturesQueryKey(orgId, projectId),
    queryFn: () => apiFetch<FixtureListResponse>(`/api/v1/projects/${projectId}/fixtures`),
    enabled: orgId !== null && projectId.length > 0,
  });
}

/** POST /api/v1/projects/{projectId}/fixtures — idempotent registration of a name, never a credential. */
export function useRegisterFixture(orgId: string | null, projectId: string) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (body: RegisterFixtureRequest) =>
      apiFetch<Fixture>(`/api/v1/projects/${projectId}/fixtures`, { method: 'POST', body }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: fixturesQueryKey(orgId, projectId) });
    },
  });
}

/** DELETE /api/v1/projects/{projectId}/fixtures/{name} */
export function useDeleteFixture(orgId: string | null, projectId: string) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (name: string) =>
      apiFetch<void>(`/api/v1/projects/${projectId}/fixtures/${encodeURIComponent(name)}`, { method: 'DELETE' }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: fixturesQueryKey(orgId, projectId) });
    },
  });
}
