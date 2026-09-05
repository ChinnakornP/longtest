import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { SetTestCaseStatusRequest, TestCaseListResponse, TestCaseRecord, TestCaseStatus } from '@/lib/api/qa-types';

export function testCasesQueryKey(orgId: string | null, projectId: string, status?: TestCaseStatus) {
  return ['test-cases', orgId, projectId, status ?? 'all'] as const;
}

export function testCaseQueryKey(orgId: string | null, testCaseId: string) {
  return ['test-cases', orgId, 'detail', testCaseId] as const;
}

/** GET /api/v1/projects/{projectId}/test-cases?status= */
export function useTestCases(orgId: string | null, projectId: string, status?: TestCaseStatus) {
  return useQuery({
    queryKey: testCasesQueryKey(orgId, projectId, status),
    queryFn: () =>
      apiFetch<TestCaseListResponse>(
        `/api/v1/projects/${projectId}/test-cases${status ? `?status=${status}` : ''}`,
      ),
    enabled: orgId !== null && projectId.length > 0,
  });
}

/** GET /api/v1/test-cases/{testCaseId} */
export function useTestCase(orgId: string | null, testCaseId: string) {
  return useQuery({
    queryKey: testCaseQueryKey(orgId, testCaseId),
    queryFn: () => apiFetch<TestCaseRecord>(`/api/v1/test-cases/${testCaseId}`),
    enabled: orgId !== null && testCaseId.length > 0,
  });
}

/**
 * PATCH /api/v1/test-cases/{testCaseId} — status only. Transitions:
 * draft -> approved | archived, approved -> archived | draft, archived -> draft.
 * There is no endpoint to edit steps/assertions; see qa-types.ts.
 */
export function useSetTestCaseStatus(orgId: string | null) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: ({ testCaseId, status }: { testCaseId: string; status: TestCaseStatus }) =>
      apiFetch<TestCaseRecord>(`/api/v1/test-cases/${testCaseId}`, {
        method: 'PATCH',
        body: { status } satisfies SetTestCaseStatusRequest,
      }),
    onSuccess: async (updated) => {
      queryClient.setQueryData(testCaseQueryKey(orgId, updated.id), updated);
      await queryClient.invalidateQueries({ queryKey: ['test-cases', orgId, updated.projectId] });
      await queryClient.invalidateQueries({ queryKey: ['coverage', orgId, updated.projectId] });
    },
  });
}
