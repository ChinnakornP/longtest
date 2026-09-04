import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { CreateRunRequest, ReportResponse, Run } from '@/lib/api/qa-types';

export function runQueryKey(orgId: string | null, runId: string) {
  return ['runs', orgId, runId] as const;
}

export function runReportQueryKey(orgId: string | null, runId: string) {
  return ['runs', orgId, runId, 'report'] as const;
}

export function useRun(orgId: string | null, runId: string) {
  return useQuery({
    queryKey: runQueryKey(orgId, runId),
    queryFn: () => apiFetch<Run>(`/api/v1/runs/${runId}`),
    enabled: orgId !== null && runId.length > 0,
  });
}

export function useCreateRun() {
  return useMutation({
    mutationFn: (body: CreateRunRequest) => apiFetch<Run>('/api/v1/runs', { method: 'POST', body }),
  });
}

export function useCancelRun(orgId: string | null, runId: string) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: () => apiFetch<Run>(`/api/v1/runs/${runId}/cancel`, { method: 'POST' }),
    onSuccess: (run) => {
      queryClient.setQueryData(runQueryKey(orgId, runId), run);
    },
  });
}

/** Only meaningful once the run has settled (see mocks/qa-store.ts#getRunReport) - the UI should gate this on run.status !== 'running'. */
export function useRunReport(orgId: string | null, runId: string, enabled: boolean) {
  return useQuery({
    queryKey: runReportQueryKey(orgId, runId),
    queryFn: () => apiFetch<ReportResponse>(`/api/v1/runs/${runId}/report`),
    enabled: orgId !== null && runId.length > 0 && enabled,
  });
}
