'use client';

import Link from 'next/link';
import { useParams } from 'next/navigation';
import { toast } from 'sonner';

import { TestCaseStatusBadge } from '@/components/test-case-status-badge';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { ApiError } from '@/lib/api/client';
import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useApplicationMap } from '@/lib/api/hooks/use-application-map';
import { useSetTestCaseStatus, useTestCase } from '@/lib/api/hooks/use-test-cases';
import type { TestCaseStatus } from '@/lib/api/qa-types';
import { canWrite } from '@/lib/auth/role';
import { buildElementIndex, describeAssertion, describeStep } from '@/lib/qa/describe-test-case';

/** Legal PATCH transitions per the T14 contract: draft -> approved | archived, approved -> archived | draft, archived -> draft. */
const NEXT_STATUSES: Record<TestCaseStatus, TestCaseStatus[]> = {
  draft: ['approved', 'archived'],
  approved: ['archived', 'draft'],
  archived: ['draft'],
};

const STATUS_ACTION_LABEL: Record<TestCaseStatus, string> = {
  draft: 'Move back to draft',
  approved: 'Approve',
  archived: 'Archive',
};

export default function TestCaseDetailPage() {
  const params = useParams<{ projectId: string; testCaseId: string }>();
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id ?? null;
  const canReview = canWrite(activeOrg?.role);

  const testCase = useTestCase(orgId, params.testCaseId);
  const appMap = useApplicationMap(orgId, params.projectId);
  const setStatus = useSetTestCaseStatus(orgId);

  const elements = buildElementIndex(appMap.data);

  const handleTransition = (status: TestCaseStatus) => {
    setStatus.mutate(
      { testCaseId: params.testCaseId, status },
      {
        onSuccess: () => toast.success(`Test case ${STATUS_ACTION_LABEL[status].toLowerCase()}d.`),
        onError: (error) => {
          toast.error(error instanceof ApiError ? error.message : 'Could not update the test case.');
        },
      },
    );
  };

  if (testCase.isLoading) {
    return <p className="text-muted-foreground text-sm">Loading test case…</p>;
  }
  if (testCase.isError || !testCase.data) {
    return <p className="text-sm text-red-600">Test case not found.</p>;
  }

  const { data: tc } = testCase;
  const payload = tc.payload;

  return (
    <div className="space-y-6">
      <div>
        <Link href={`/projects/${params.projectId}/test-cases`} className="text-muted-foreground text-sm hover:underline">
          ← Back to test cases
        </Link>
      </div>

      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="space-y-1">
          <div className="flex items-center gap-2">
            <span className="font-mono text-sm">{tc.ref}</span>
            <h1 className="text-2xl font-semibold tracking-tight">{tc.name}</h1>
          </div>
          {payload.description && <p className="text-muted-foreground text-sm">{payload.description}</p>}
          <div className="flex flex-wrap gap-1.5 pt-1">
            <TestCaseStatusBadge status={tc.status} />
            <Badge variant="outline">{tc.priority}</Badge>
            <Badge variant="outline">{tc.category}</Badge>
            <Badge variant="outline">v{tc.version}</Badge>
          </div>
        </div>

        {canReview && (
          <div className="flex gap-2">
            {NEXT_STATUSES[tc.status].map((next) => (
              <Button
                key={next}
                variant={next === 'approved' ? 'default' : 'outline'}
                onClick={() => handleTransition(next)}
                disabled={setStatus.isPending}
              >
                {STATUS_ACTION_LABEL[next]}
              </Button>
            ))}
          </div>
        )}
      </div>

      {payload.preconditions && payload.preconditions.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle>Preconditions</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-wrap gap-1.5">
            {payload.preconditions.map((p) => (
              <Badge key={p} variant="outline">
                {p}
              </Badge>
            ))}
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>Steps</CardTitle>
        </CardHeader>
        <CardContent>
          <ol className="space-y-2">
            {payload.steps.map((step, index) => (
              <li key={index} className="flex gap-3 text-sm">
                <span className="text-muted-foreground w-6 shrink-0 text-right font-mono">{index + 1}.</span>
                <span>{describeStep(step, elements)}</span>
              </li>
            ))}
          </ol>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Assertions</CardTitle>
        </CardHeader>
        <CardContent>
          <ol className="space-y-2">
            {payload.assertions.map((assertion, index) => (
              <li key={index} className="flex gap-3 text-sm">
                <span className="text-muted-foreground w-6 shrink-0 text-right font-mono">{index + 1}.</span>
                <span>{describeAssertion(assertion, elements)}</span>
              </li>
            ))}
          </ol>
        </CardContent>
      </Card>

      {payload.tags && payload.tags.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {payload.tags.map((tag) => (
            <Badge key={tag} variant="outline">
              #{tag}
            </Badge>
          ))}
        </div>
      )}

      <p className="text-muted-foreground text-xs">
        Editing steps or assertions isn&apos;t available yet — the API only supports approving or archiving a case as-is.
        Version history isn&apos;t exposed either, so there is no diff to show here yet.
      </p>
    </div>
  );
}
