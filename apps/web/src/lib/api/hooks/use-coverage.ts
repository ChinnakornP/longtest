import { useQuery } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { CoverageReport } from '@/lib/api/qa-types';

export function coverageQueryKey(orgId: string | null, projectId: string) {
  return ['coverage', orgId, projectId] as const;
}

/**
 * GET /api/v1/projects/{projectId}/coverage. 404s when the project has no
 * Application Map yet — surfaced to the caller as `isError`, not coerced into
 * an empty report (an empty report would claim the app has no gaps).
 */
export function useCoverage(orgId: string | null, projectId: string) {
  return useQuery({
    queryKey: coverageQueryKey(orgId, projectId),
    queryFn: () => apiFetch<CoverageReport>(`/api/v1/projects/${projectId}/coverage`),
    enabled: orgId !== null && projectId.length > 0,
  });
}
