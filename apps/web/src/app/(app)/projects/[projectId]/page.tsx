'use client';

import Link from 'next/link';
import { useParams, useRouter } from 'next/navigation';
import { useState } from 'react';
import { toast } from 'sonner';

import { RuntimePicker } from '@/components/runtime-picker';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { ApiError } from '@/lib/api/client';
import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useProject } from '@/lib/api/hooks/use-projects';
import { useCreateRun } from '@/lib/api/hooks/use-runs';
import { useRuntimes } from '@/lib/api/hooks/use-runtimes';
import type { RunMode } from '@/lib/api/qa-types';
import { RUN_MODE_VALUES } from '@/lib/api/qa-types';
import { canWrite } from '@/lib/auth/role';
import { EMPTY_RECENT_RUNS, useRecentRunsStore } from '@/lib/stores/recent-runs-store';

const MODE_LABEL: Record<RunMode, string> = {
  discover: 'Discover — map the application only',
  plan: 'Plan — discover, then generate test cases',
  execute: 'Execute — run existing test cases',
  full: 'Full — discover, plan, execute and analyze',
};

export default function ProjectDetailPage() {
  const params = useParams<{ projectId: string }>();
  const router = useRouter();
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id ?? null;
  const project = useProject(orgId, params.projectId);
  const runtimes = useRuntimes(orgId);
  const createRun = useCreateRun();
  const addRecentRun = useRecentRunsStore((s) => s.addRun);
  const recentRuns = useRecentRunsStore((s) => s.byProject[params.projectId] ?? EMPTY_RECENT_RUNS);

  const [runtimeId, setRuntimeId] = useState<string | null>(null);
  const [mode, setMode] = useState<RunMode>('full');
  const canStart = canWrite(activeOrg?.role);

  const onlineRuntimes = runtimes.data?.filter((r) => r.online) ?? [];
  const selectedRuntime = runtimeId ?? onlineRuntimes[0]?.id ?? null;

  const handleStart = () => {
    if (!selectedRuntime || !project.data) return;
    createRun.mutate(
      { projectId: project.data.id, runtimeId: selectedRuntime, mode },
      {
        onSuccess: (run) => {
          addRecentRun(project.data!.id, { id: run.id, mode: run.mode, startedAt: run.createdAt });
          router.push(`/runs/${run.id}`);
        },
        onError: (error) => {
          toast.error(error instanceof ApiError ? error.message : 'Could not start the run.');
        },
      },
    );
  };

  if (project.isLoading) {
    return <p className="text-muted-foreground text-sm">Loading project…</p>;
  }
  if (project.isError || !project.data) {
    return <p className="text-sm text-red-600">Project not found.</p>;
  }

  return (
    <div className="space-y-6">
      <div className="space-y-1">
        <h1 className="text-2xl font-semibold tracking-tight">{project.data.name}</h1>
        <p className="text-muted-foreground text-sm">{project.data.baseUrl}</p>
      </div>

      {canStart && (
        <Card>
          <CardHeader>
            <CardTitle>Start a run</CardTitle>
            <CardDescription>Pick a runtime and a mode, then start testing.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            {runtimes.isLoading && <p className="text-muted-foreground text-sm">Loading runtimes…</p>}
            {runtimes.isError && (
              <p className="text-sm text-red-600">Could not load runtimes. Try refreshing the page.</p>
            )}
            {runtimes.data && runtimes.data.length === 0 && (
              <p className="text-muted-foreground text-sm">
                No runtime is paired with this organization yet. Pair one from the Runtimes page first.
              </p>
            )}
            {runtimes.data && runtimes.data.length > 0 && onlineRuntimes.length === 0 && (
              <p className="text-sm text-amber-600">
                Every paired runtime is offline. Start the daemon on one of them, then refresh.
              </p>
            )}
            {runtimes.data && runtimes.data.length > 0 && (
              <RuntimePicker runtimes={runtimes.data} selectedId={selectedRuntime} onSelect={setRuntimeId} />
            )}

            <div className="space-y-1.5">
              <label htmlFor="run-mode" className="text-sm leading-none font-medium">
                Mode
              </label>
              <select
                id="run-mode"
                value={mode}
                onChange={(e) => setMode(e.target.value as RunMode)}
                className="border-border h-10 w-full rounded-lg border bg-transparent px-3 text-sm"
              >
                {RUN_MODE_VALUES.map((value) => (
                  <option key={value} value={value}>
                    {MODE_LABEL[value]}
                  </option>
                ))}
              </select>
            </div>

            <Button onClick={handleStart} disabled={!selectedRuntime || createRun.isPending}>
              {createRun.isPending ? 'Starting…' : 'Start Testing'}
            </Button>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>Recent runs</CardTitle>
          <CardDescription>Runs you started from this browser.</CardDescription>
        </CardHeader>
        <CardContent>
          {recentRuns.length === 0 ? (
            <p className="text-muted-foreground text-sm">No runs started yet.</p>
          ) : (
            <ul className="divide-border divide-y">
              {recentRuns.map((run) => (
                <li key={run.id}>
                  <Link href={`/runs/${run.id}`} className="hover:bg-muted flex items-center justify-between px-2 py-2 text-sm">
                    <span className="font-mono">{run.id}</span>
                    <span className="text-muted-foreground">
                      {run.mode} · {new Date(run.startedAt).toLocaleString()}
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
