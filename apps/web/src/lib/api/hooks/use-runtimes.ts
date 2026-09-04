import { useMutation, useQuery } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { Runtime } from '@/lib/api/qa-types';
import type { PairingCodeResponse } from '@/lib/api/types';

export function runtimesQueryKey(orgId: string | null) {
  return ['runtimes', orgId] as const;
}

/** GET /api/v1/runtimes (T08) - which runtimes are online and what they can run. Distinct from useRequestPairingCode below, which is the T05 pairing flow that provisions one. */
export function useRuntimes(orgId: string | null) {
  return useQuery({
    queryKey: runtimesQueryKey(orgId),
    queryFn: () => apiFetch<Runtime[]>('/api/v1/runtimes'),
    enabled: orgId !== null,
    refetchInterval: 15_000,
  });
}

export function useRequestPairingCode(orgId: string | null) {
  return useMutation({
    mutationFn: () => {
      if (orgId === null) {
        return Promise.reject(new Error('No active org selected.'));
      }
      return apiFetch<PairingCodeResponse>(`/api/v1/orgs/${orgId}/runtimes/pair`, {
        method: 'POST',
      });
    },
  });
}
