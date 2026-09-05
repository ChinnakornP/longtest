'use client';

import { useParams, useRouter } from 'next/navigation';
import { toast } from 'sonner';

import { CoverageStatusBadge } from '@/components/coverage-status-badge';
import { RiskBadge } from '@/components/risk-badge';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { ApiError } from '@/lib/api/client';
import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useCoverage } from '@/lib/api/hooks/use-coverage';
import { useCreateRun } from '@/lib/api/hooks/use-runs';
import { useRuntimes } from '@/lib/api/hooks/use-runtimes';
import type { CoverageSuggestion, PageCoverage, WorkflowCoverage } from '@/lib/api/qa-types';
import { canWrite } from '@/lib/auth/role';
import { useRecentRunsStore } from '@/lib/stores/recent-runs-store';

export default function CoveragePage() {
  const params = useParams<{ projectId: string }>();
  const router = useRouter();
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id ?? null;
  const canGenerate = canWrite(activeOrg?.role);

  const coverage = useCoverage(orgId, params.projectId);
  const runtimes = useRuntimes(orgId);
  const createRun = useCreateRun();
  const addRecentRun = useRecentRunsStore((s) => s.addRun);
  const onlineRuntimeId = runtimes.data?.find((r) => r.online)?.id ?? null;

  const handleGenerateTests = () => {
    if (!onlineRuntimeId) {
      toast.error('No runtime is online. Start the daemon on a paired runtime first.');
      return;
    }
    createRun.mutate(
      { projectId: params.projectId, runtimeId: onlineRuntimeId, mode: 'plan' },
      {
        onSuccess: (run) => {
          addRecentRun(params.projectId, { id: run.id, mode: run.mode, startedAt: run.createdAt });
          router.push(`/runs/${run.id}`);
        },
        onError: (error) => {
          toast.error(error instanceof ApiError ? error.message : 'Could not start planning.');
        },
      },
    );
  };

  if (coverage.isLoading) {
    return <p className="text-muted-foreground text-sm">Loading coverage…</p>;
  }
  if (coverage.isError) {
    return (
      <p className="text-sm text-red-600">
        Could not load coverage — this project may not have an Application Map yet. Run discovery first.
      </p>
    );
  }
  if (!coverage.data) return null;
  const report = coverage.data;

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="space-y-1">
          <h1 className="text-2xl font-semibold tracking-tight">Coverage</h1>
          <p className="text-sm">{report.summary}</p>
        </div>
        {canGenerate && report.suggestedTestCount > 0 && (
          <Button onClick={handleGenerateTests} disabled={createRun.isPending}>
            {createRun.isPending ? 'Starting…' : `Generate tests (${report.suggestedTestCount} suggested)`}
          </Button>
        )}
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Workflows</CardTitle>
          <CardDescription>End-to-end paths through the application.</CardDescription>
        </CardHeader>
        <CardContent>
          {report.workflows.length === 0 ? (
            <p className="text-muted-foreground text-sm">No workflows discovered yet.</p>
          ) : (
            <ul className="divide-border divide-y">
              {report.workflows.map((workflow) => (
                <WorkflowRow key={workflow.ref} workflow={workflow} />
              ))}
            </ul>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Pages</CardTitle>
          <CardDescription>Pages no workflow's path already accounts for.</CardDescription>
        </CardHeader>
        <CardContent>
          {report.pages.length === 0 ? (
            <p className="text-muted-foreground text-sm">Every discovered page is on a covered workflow path.</p>
          ) : (
            <ul className="divide-border divide-y">
              {report.pages.map((page) => (
                <PageRow key={page.ref} page={page} />
              ))}
            </ul>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Categories</CardTitle>
          <CardDescription>Approved cases per contract category.</CardDescription>
        </CardHeader>
        <CardContent>
          <ul className="divide-border divide-y">
            {report.categories.map((cat) => (
              <li key={cat.category} className="flex items-center justify-between py-2 text-sm">
                <span>{cat.category}</span>
                <span className="text-muted-foreground">
                  {cat.approved} approved
                  {cat.suggestedTests > 0 ? ` · ${cat.suggestedTests} suggested` : ''}
                </span>
              </li>
            ))}
          </ul>
        </CardContent>
      </Card>
    </div>
  );
}

function WorkflowRow({ workflow }: { workflow: WorkflowCoverage }) {
  return (
    <li className="space-y-1.5 py-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-sm font-medium">{workflow.name}</span>
        <div className="flex items-center gap-1.5">
          <CoverageStatusBadge status={workflow.status} />
          <RiskBadge risk={workflow.risk} />
        </div>
      </div>
      {workflow.expectedOutcome && <p className="text-muted-foreground text-xs">{workflow.expectedOutcome}</p>}
      <p className="text-muted-foreground text-xs">
        Best case covers {Math.round(workflow.coverageRatio * 100)}% of the path
        {workflow.coveringCaseRefs.length > 0 ? ` · ${workflow.coveringCaseRefs.join(', ')}` : ''}
      </p>
      <SuggestionList suggestions={workflow.suggestions} />
    </li>
  );
}

function PageRow({ page }: { page: PageCoverage }) {
  return (
    <li className="space-y-1.5 py-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="font-mono text-sm">{page.path}</span>
        <div className="flex items-center gap-1.5">
          <CoverageStatusBadge status={page.status} />
          <RiskBadge risk={page.risk} />
        </div>
      </div>
      {page.title && <p className="text-muted-foreground text-xs">{page.title}</p>}
      <SuggestionList suggestions={page.suggestions} />
    </li>
  );
}

function SuggestionList({ suggestions }: { suggestions?: CoverageSuggestion[] }) {
  if (!suggestions || suggestions.length === 0) return null;
  return (
    <ul className="space-y-1 pt-1">
      {suggestions.map((s, i) => (
        <li key={i} className="flex items-center gap-2 text-xs">
          <Badge variant="outline">{s.category}</Badge>
          <span className="text-muted-foreground">{s.reason}</span>
        </li>
      ))}
    </ul>
  );
}
