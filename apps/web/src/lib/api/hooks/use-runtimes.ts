import { useMutation } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { PairingCodeResponse } from '@/lib/api/types';

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
