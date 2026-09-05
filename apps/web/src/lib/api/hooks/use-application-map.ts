import { useQuery } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { ApplicationMap } from '@/lib/api/qa-types';

export function applicationMapQueryKey(orgId: string | null, projectId: string) {
  return ['application-map', orgId, projectId] as const;
}

/** GET /api/v1/projects/{projectId}/appmap — the application-map@1 document, passed through verbatim. */
export function useApplicationMap(orgId: string | null, projectId: string) {
  return useQuery({
    queryKey: applicationMapQueryKey(orgId, projectId),
    queryFn: () => apiFetch<ApplicationMap>(`/api/v1/projects/${projectId}/appmap`),
    enabled: orgId !== null && projectId.length > 0,
    retry: false,
  });
}
