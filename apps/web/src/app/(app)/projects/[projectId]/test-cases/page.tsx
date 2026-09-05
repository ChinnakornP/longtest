'use client';

import Link from 'next/link';
import { useParams, useRouter } from 'next/navigation';
import { useMemo, useState } from 'react';
import { toast } from 'sonner';

import { RuntimePicker } from '@/components/runtime-picker';
import { TestCaseStatusBadge } from '@/components/test-case-status-badge';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { ApiError } from '@/lib/api/client';
import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useCreateRun } from '@/lib/api/hooks/use-runs';
import { useRuntimes } from '@/lib/api/hooks/use-runtimes';
import { useTestCases } from '@/lib/api/hooks/use-test-cases';
import type { TestCaseRecord, TestCaseStatus } from '@/lib/api/qa-types';
import { TEST_CASE_STATUS_VALUES } from '@/lib/api/qa-types';
import { canWrite } from '@/lib/auth/role';
import { useRecentRunsStore } from '@/lib/stores/recent-runs-store';
import { cn } from '@/lib/utils';

const PRIORITY_ORDER = ['critical', 'high', 'medium', 'low'] as const;
const FILTERS: Array<TestCaseStatus | 'all'> = ['all', ...TEST_CASE_STATUS_VALUES];
const FILTER_LABEL: Record<TestCaseStatus | 'all', string> = {
  all: 'All',
  draft: 'Draft',
  approved: 'Approved',
  archived: 'Archived',
};

/** What the confirm-run dialog is about to start: an explicit selection, or "every approved case" left for the server to resolve. */
type RunTarget = { kind: 'selected'; ids: string[] } | { kind: 'allApproved' };

export default function TestCasesPage() {
  const params = useParams<{ projectId: string }>();
  const router = useRouter();
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id ?? null;
  const canRun = canWrite(activeOrg?.role);

  const [filter, setFilter] = useState<TestCaseStatus | 'all'>('all');

  // Unfiltered, used for the plan summary and the approved count — independent of which tab is active, and read via `total`, never `.length`, since a page this size can be capped by the server's max page size.
  const summary = useTestCases(orgId, params.projectId, undefined);
  const approvedSummary = useTestCases(orgId, params.projectId, 'approved', 1);
  // The visible list: server-side `?status=` filtering, not an in-memory filter of the summary page — dedupes against `summary` automatically when filter is 'all' (same query key).
  const list = useTestCases(orgId, params.projectId, filter === 'all' ? undefined : filter);

  const runtimes = useRuntimes(orgId);
  const createRun = useCreateRun();
  const addRecentRun = useRecentRunsStore((s) => s.addRun);

  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [runTarget, setRunTarget] = useState<RunTarget | null>(null);
  const [runtimeId, setRuntimeId] = useState<string | null>(null);

  const visible = list.data?.testCases ?? [];
  const summaryRows = summary.data?.testCases ?? [];
  const totalCount = summary.data?.total ?? 0;
  const approvedCount = approvedSummary.data?.total ?? 0;
  const truncated = totalCount > summaryRows.length;

  const priorityCounts = useMemo(() => countBy(summaryRows, (tc) => tc.priority), [summaryRows]);
  const categoryCounts = useMemo(() => countBy(summaryRows, (tc) => tc.category), [summaryRows]);

  const onlineRuntimes = runtimes.data?.filter((r) => r.online) ?? [];

  const toggleSelected = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const openRunDialog = (target: RunTarget) => {
    if (target.kind === 'selected' && target.ids.length === 0) return;
    setRunTarget(target);
    setRuntimeId(onlineRuntimes[0]?.id ?? null);
  };

  const confirmRun = () => {
    if (!runTarget || !runtimeId) return;
    createRun.mutate(
      {
        projectId: params.projectId,
        runtimeId,
        mode: 'execute',
        // Absent means "every approved case", resolved server-side — sending
        // the client's own list here would silently narrow "run the suite"
        // down to only what this browser happened to have loaded.
        testCaseIds: runTarget.kind === 'selected' ? runTarget.ids : undefined,
      },
      {
        onSuccess: (run) => {
          addRecentRun(params.projectId, { id: run.id, mode: run.mode, startedAt: run.createdAt });
          setRunTarget(null);
          router.push(`/runs/${run.id}`);
        },
        onError: (error) => {
          toast.error(error instanceof ApiError ? error.message : 'Could not start the run.');
        },
      },
    );
  };

  if (summary.isLoading || list.isLoading) {
    return <p className="text-muted-foreground text-sm">Loading test cases…</p>;
  }
  if (summary.isError || list.isError) {
    return <p className="text-sm text-red-600">Could not load test cases. Try refreshing the page.</p>;
  }

  const runDialogCount = runTarget?.kind === 'selected' ? runTarget.ids.length : approvedCount;

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div className="space-y-1">
          <h1 className="text-2xl font-semibold tracking-tight">Test cases</h1>
          <p className="text-muted-foreground text-sm">Review what the planner generated, then approve what should run as regression.</p>
        </div>
        {canRun && (
          <div className="flex gap-2">
            <Button
              variant="outline"
              onClick={() => openRunDialog({ kind: 'selected', ids: [...selected] })}
              disabled={selected.size === 0}
            >
              Run selected ({selected.size})
            </Button>
            <Button onClick={() => openRunDialog({ kind: 'allApproved' })} disabled={approvedCount === 0}>
              Run all approved ({approvedCount})
            </Button>
          </div>
        )}
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Plan summary</CardTitle>
          <CardDescription>
            {totalCount} test cases total.
            {truncated && ` Counts below are based on the first ${summaryRows.length} — refine by status to see the rest.`}
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-6 sm:grid-cols-2">
          <div className="space-y-2">
            <p className="text-muted-foreground text-xs font-medium tracking-wide uppercase">By priority</p>
            <div className="flex flex-wrap gap-2">
              {PRIORITY_ORDER.map((p) => (
                <Badge key={p} variant="outline">
                  {p}: {priorityCounts[p] ?? 0}
                </Badge>
              ))}
            </div>
          </div>
          <div className="space-y-2">
            <p className="text-muted-foreground text-xs font-medium tracking-wide uppercase">By category</p>
            <div className="flex flex-wrap gap-2">
              {Object.entries(categoryCounts).map(([category, count]) => (
                <Badge key={category} variant="outline">
                  {category}: {count}
                </Badge>
              ))}
            </div>
          </div>
        </CardContent>
      </Card>

      <div className="flex items-center gap-1">
        {FILTERS.map((value) => (
          <button
            key={value}
            type="button"
            onClick={() => setFilter(value)}
            className={cn(
              'rounded-md px-3 py-1.5 text-sm transition-colors',
              filter === value ? 'bg-muted text-foreground' : 'text-muted-foreground hover:text-foreground',
            )}
          >
            {FILTER_LABEL[value]}
          </button>
        ))}
      </div>

      {visible.length === 0 ? (
        <p className="text-muted-foreground text-sm">No test cases in this view.</p>
      ) : (
        <ul className="divide-border border-border divide-y rounded-lg border">
          {visible.map((tc) => (
            <TestCaseRow
              key={tc.id}
              testCase={tc}
              projectId={params.projectId}
              canRun={canRun}
              selectable={tc.status !== 'archived'}
              selected={selected.has(tc.id)}
              onToggle={() => toggleSelected(tc.id)}
            />
          ))}
        </ul>
      )}

      <Dialog open={runTarget !== null} onOpenChange={(open) => !open && setRunTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {runTarget?.kind === 'allApproved'
                ? `Run all approved test cases (${runDialogCount})`
                : `Run ${runDialogCount} test case(s)`}
            </DialogTitle>
          </DialogHeader>
          <div className="space-y-4">
            {runtimes.isLoading && <p className="text-muted-foreground text-sm">Loading runtimes…</p>}
            {onlineRuntimes.length === 0 && !runtimes.isLoading && (
              <p className="text-sm text-amber-600">No runtime is online. Start the daemon on a paired runtime first.</p>
            )}
            {onlineRuntimes.length > 0 && (
              <RuntimePicker runtimes={runtimes.data ?? []} selectedId={runtimeId} onSelect={setRuntimeId} />
            )}
            <Button onClick={confirmRun} disabled={!runtimeId || createRun.isPending}>
              {createRun.isPending ? 'Starting…' : 'Start run'}
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function TestCaseRow({
  testCase,
  projectId,
  canRun,
  selectable,
  selected,
  onToggle,
}: {
  testCase: TestCaseRecord;
  projectId: string;
  canRun: boolean;
  selectable: boolean;
  selected: boolean;
  onToggle: () => void;
}) {
  return (
    <li className="hover:bg-muted flex items-center gap-3 px-4 py-3">
      {canRun && (
        <input
          type="checkbox"
          checked={selected}
          disabled={!selectable}
          onChange={onToggle}
          aria-label={`Select ${testCase.ref}`}
          className="size-4"
        />
      )}
      <Link href={`/projects/${projectId}/test-cases/${testCase.id}`} className="flex flex-1 items-center justify-between gap-3">
        <div className="space-y-0.5">
          <div className="flex items-center gap-2">
            <span className="font-mono text-sm">{testCase.ref}</span>
            <span className="text-sm font-medium">{testCase.name}</span>
          </div>
          <div className="flex gap-1.5">
            <Badge variant="outline">{testCase.priority}</Badge>
            <Badge variant="outline">{testCase.category}</Badge>
          </div>
        </div>
        <TestCaseStatusBadge status={testCase.status} />
      </Link>
    </li>
  );
}

function countBy<T>(items: T[], key: (item: T) => string): Record<string, number> {
  const counts: Record<string, number> = {};
  for (const item of items) {
    const k = key(item);
    counts[k] = (counts[k] ?? 0) + 1;
  }
  return counts;
}
