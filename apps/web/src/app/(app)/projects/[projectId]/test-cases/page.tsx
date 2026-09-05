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
import { cn } from '@/lib/utils';

const PRIORITY_ORDER = ['critical', 'high', 'medium', 'low'] as const;
const FILTERS: Array<TestCaseStatus | 'all'> = ['all', ...TEST_CASE_STATUS_VALUES];
const FILTER_LABEL: Record<TestCaseStatus | 'all', string> = {
  all: 'All',
  draft: 'Draft',
  approved: 'Approved',
  archived: 'Archived',
};

export default function TestCasesPage() {
  const params = useParams<{ projectId: string }>();
  const router = useRouter();
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id ?? null;
  const canRun = canWrite(activeOrg?.role);

  const testCases = useTestCases(orgId, params.projectId);
  const runtimes = useRuntimes(orgId);
  const createRun = useCreateRun();

  const [filter, setFilter] = useState<TestCaseStatus | 'all'>('all');
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [runDialogIds, setRunDialogIds] = useState<string[] | null>(null);
  const [runtimeId, setRuntimeId] = useState<string | null>(null);

  const all = testCases.data?.testCases ?? [];
  const visible = filter === 'all' ? all : all.filter((tc) => tc.status === filter);

  const priorityCounts = useMemo(() => countBy(all, (tc) => tc.priority), [all]);
  const categoryCounts = useMemo(() => countBy(all, (tc) => tc.category), [all]);
  const approvedIds = useMemo(() => all.filter((tc) => tc.status === 'approved').map((tc) => tc.id), [all]);

  const onlineRuntimes = runtimes.data?.filter((r) => r.online) ?? [];

  const toggleSelected = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const openRunDialog = (ids: string[]) => {
    if (ids.length === 0) return;
    setRunDialogIds(ids);
    setRuntimeId(onlineRuntimes[0]?.id ?? null);
  };

  const confirmRun = () => {
    if (!runDialogIds || !runtimeId) return;
    createRun.mutate(
      { projectId: params.projectId, runtimeId, mode: 'execute', testCaseIds: runDialogIds },
      {
        onSuccess: (run) => {
          setRunDialogIds(null);
          router.push(`/runs/${run.id}`);
        },
        onError: (error) => {
          toast.error(error instanceof ApiError ? error.message : 'Could not start the run.');
        },
      },
    );
  };

  if (testCases.isLoading) {
    return <p className="text-muted-foreground text-sm">Loading test cases…</p>;
  }
  if (testCases.isError) {
    return <p className="text-sm text-red-600">Could not load test cases. Try refreshing the page.</p>;
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div className="space-y-1">
          <h1 className="text-2xl font-semibold tracking-tight">Test cases</h1>
          <p className="text-muted-foreground text-sm">Review what the planner generated, then approve what should run as regression.</p>
        </div>
        {canRun && (
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => openRunDialog([...selected])} disabled={selected.size === 0}>
              Run selected ({selected.size})
            </Button>
            <Button onClick={() => openRunDialog(approvedIds)} disabled={approvedIds.length === 0}>
              Run all approved ({approvedIds.length})
            </Button>
          </div>
        )}
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Plan summary</CardTitle>
          <CardDescription>{all.length} test cases total.</CardDescription>
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

      <Dialog open={runDialogIds !== null} onOpenChange={(open) => !open && setRunDialogIds(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Run {runDialogIds?.length ?? 0} test case(s)</DialogTitle>
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
