import { useQuery } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { MeResponse } from '@/lib/api/types';

export const meQueryKey = ['me'] as const;

export function useMe() {
  return useQuery({
    queryKey: meQueryKey,
    queryFn: () => apiFetch<MeResponse>('/api/v1/me', { skipOrgHeader: true }),
    retry: false,
  });
}
