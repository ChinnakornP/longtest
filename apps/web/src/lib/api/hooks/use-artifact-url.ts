import { useQuery } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { ArtifactUrlResponse } from '@/lib/api/qa-types';

/** GET /api/v1/artifacts/{id} -> presigned URL. See qa-types.ts: this endpoint is a frontend assumption, not part of the published T08 contract. */
export function useArtifactUrl(artifactId: string | null) {
  return useQuery({
    queryKey: ['artifacts', artifactId] as const,
    queryFn: () => apiFetch<ArtifactUrlResponse>(`/api/v1/artifacts/${artifactId}`),
    enabled: artifactId !== null,
    staleTime: 5 * 60 * 1000,
  });
}
