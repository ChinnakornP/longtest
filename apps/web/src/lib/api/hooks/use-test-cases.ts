import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { SetTestCaseStatusRequest, TestCaseListResponse, TestCaseRecord, TestCaseStatus } from '@/lib/api/qa-types';

/** Server max per `#/components/parameters/Limit` — the highest page a single call can read without paging. */
export const MAX_TEST_CASE_PAGE_SIZE = 200;

export function testCasesQueryKey(
  orgId: string | null,
  projectId: string,
  status?: TestCaseStatus,
  limit = MAX_TEST_CASE_PAGE_SIZE,
) {
  return ['test-cases', orgId, projectId, status ?? 'all', limit] as const;
}

export function testCaseQueryKey(orgId: string | null, testCaseId: string) {
  return ['test-cases', orgId, 'detail', testCaseId] as const;
}

/**
 * GET /api/v1/projects/{projectId}/test-cases?status=&limit=&offset= — `total`
 * on the response is the project's real count, independent of how many rows
 * this page actually returned; a caller after an aggregate (a count, a
 * summary) should read `total`, never `testCases.length`.
 */
export function useTestCases(
  orgId: string | null,
  projectId: string,
  status?: TestCaseStatus,
  limit: number = MAX_TEST_CASE_PAGE_SIZE,
) {
  return useQuery({
    queryKey: testCasesQueryKey(orgId, projectId, status, limit),
    queryFn: () => {
      const query = new URLSearchParams({ limit: String(limit) });
      if (status) query.set('status', status);
      return apiFetch<TestCaseListResponse>(`/api/v1/projects/${projectId}/test-cases?${query}`);
    },
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
