'use client';

import { useParams } from 'next/navigation';
import { useEffect } from 'react';
import { toast } from 'sonner';

import { ExecutionList } from '@/components/execution-list';
import { RunEventLog } from '@/components/run-event-log';
import { RunStatusBadge } from '@/components/run-status-badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { ApiError } from '@/lib/api/client';
import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useRunStream } from '@/lib/api/hooks/use-run-stream';
import { useCancelRun, useRun, useRunReport } from '@/lib/api/hooks/use-runs';
import type { RunCounters } from '@/lib/api/qa-types';
import { canWrite } from '@/lib/auth/role';
import type { ConnectionState } from '@/lib/ws/types';

const RUNNING_STATUSES = new Set(['queued', 'running']);

export default function RunDetailPage() {
  const params = useParams<{ runId: string }>();
  const runId = params.runId;
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id ?? null;

  const run = useRun(orgId, runId);
  const stream = useRunStream(runId, orgId);
  const cancelRun = useCancelRun(orgId, runId);

  const isRunning = run.data ? RUNNING_STATUSES.has(run.data.status) : true;
  const report = useRunReport(orgId, runId, run.data ? !isRunning : false);

  // The stream tells us the run finished before a poll of useRun would - refetch once so the status badge (and cancel button) reflect the authoritative outcome without waiting on a timer.
  const { refetch: refetchRun } = run;
  useEffect(() => {
    if (stream.finished) void refetchRun();
  }, [stream.finished, refetchRun]);

  const handleCancel = () => {
    cancelRun.mutate(undefined, {
      onError: (error) => {
        toast.error(error instanceof ApiError ? error.message : 'Could not cancel the run.');
      },
    });
  };

  if (run.isLoading) {
    return <p className="text-muted-foreground text-sm">Loading run…</p>;
  }
  if (run.isError || !run.data) {
    const notFound = run.error instanceof ApiError && run.error.status === 404;
    return (
      <p className="text-sm text-red-600">
        {notFound ? 'This run does not exist, or you do not have access to it.' : 'Could not load this run.'}
      </p>
    );
  }

  const counters = { ...run.data.counters, ...stream.counters };
  const canCancel = canWrite(activeOrg?.role) && RUNNING_STATUSES.has(run.data.status);

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between gap-4">
        <div className="space-y-1">
          <h1 className="text-2xl font-semibold tracking-tight">Run {run.data.id.slice(0, 8)}</h1>
          <p className="text-muted-foreground text-sm">
            Mode: {run.data.mode} · Phase: {stream.phase ?? run.data.phase ?? '—'}
          </p>
        </div>
        <div className="flex items-center gap-3">
          <RunStatusBadge status={run.data.status} />
          {canCancel && (
            <Button variant="outline" onClick={handleCancel} disabled={cancelRun.isPending}>
              {cancelRun.isPending ? 'Cancelling…' : 'Cancel run'}
            </Button>
          )}
        </div>
      </div>

      <ConnectionBanner state={stream.connectionState} isRunning={isRunning} />

      {run.data.status === 'error' && run.data.errorMessage && (
        <p className="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-700">
          {run.data.errorMessage}
        </p>
      )}

      <CountersGrid counters={counters} />

      <Card>
        <CardHeader>
          <CardTitle>Event log</CardTitle>
          <CardDescription>{stream.events.length.toLocaleString()} events</CardDescription>
        </CardHeader>
        <CardContent>
          <RunEventLog events={stream.events} />
        </CardContent>
      </Card>

      {!isRunning && (
        <Card>
          <CardHeader>
            <CardTitle>Executions</CardTitle>
            <CardDescription>Click a row to inspect steps and evidence.</CardDescription>
          </CardHeader>
          <CardContent>
            {report.isLoading && <p className="text-muted-foreground text-sm">Loading report…</p>}
            {report.data && <ExecutionList executions={report.data.executions} />}
          </CardContent>
        </Card>
      )}
    </div>
  );
}

const COUNTER_LABEL: Partial<Record<keyof RunCounters, string>> = {
  pages: 'Pages discovered',
  workflows: 'Workflows discovered',
  forms: 'Forms',
  actions: 'Actions',
  passed: 'Passed',
  failed: 'Failed',
  skipped: 'Skipped',
};

function CountersGrid({ counters }: { counters: RunCounters }) {
  const entries = (Object.keys(COUNTER_LABEL) as Array<keyof RunCounters>).filter((key) => counters[key] !== undefined);
  if (entries.length === 0) return null;

  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
      {entries.map((key) => (
        <Card key={key}>
          <CardContent className="p-4">
            <p className="text-2xl font-semibold">{counters[key]}</p>
            <p className="text-muted-foreground text-xs">{COUNTER_LABEL[key]}</p>
          </CardContent>
        </Card>
      ))}
    </div>
  );
}

function ConnectionBanner({ state, isRunning }: { state: ConnectionState; isRunning: boolean }) {
  if (!isRunning || state === 'open') return null;

  if (state === 'offline') {
    return (
      <p className="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-700">
        Lost the connection to the runtime and could not reconnect. This run&apos;s status may be stale — check
        that the runtime is still online.
      </p>
    );
  }

  return (
    <p className="rounded-lg border border-amber-200 bg-amber-50 p-3 text-sm text-amber-700">
      {state === 'connecting' ? 'Connecting…' : 'Reconnecting…'}
    </p>
  );
}
