'use client';

import type { RunEventPayload, RunResultPayload } from '@qa/schema';
import { useEffect, useReducer, useRef, useState } from 'react';

import { apiFetch } from '@/lib/api/client';
import type { RunCounters, RunEventRecord, RunEventsPage, RunPhase } from '@/lib/api/qa-types';
import { applyRunEventToCounters } from '@/lib/run-events/counters';
import { RunStream, type RunStreamHandlers } from '@/lib/ws/run-stream';
import { getRunTransportFactory } from '@/lib/ws/transport-factory';
import type { ConnectionState } from '@/lib/ws/types';

export interface RunStreamState {
  events: RunEventRecord[];
  connectionState: ConnectionState;
  counters: RunCounters;
  phase: RunPhase | null;
  finished: boolean;
  finishedStatus: RunResultPayload['status'] | null;
}

/**
 * Live view of one run: REST catch-up from seq 0 (so a page opened mid-run
 * sees full history, not just what happened after mount), then a live
 * transport, deduped by seq (see lib/ws/run-stream.ts). Events/counters live
 * in refs rather than React state so a 10k-event run doesn't copy the whole
 * array on every single append - callers re-render via the version bump and
 * read the ref's current contents.
 */
export function useRunStream(runId: string, orgId: string | null): RunStreamState {
  const eventsRef = useRef<RunEventRecord[]>([]);
  const countersRef = useRef<RunCounters>({});
  const phaseRef = useRef<RunPhase | null>(null);
  const finishedRef = useRef<{ done: boolean; status: RunResultPayload['status'] | null }>({
    done: false,
    status: null,
  });
  const [, bump] = useReducer((n: number) => n + 1, 0);
  const [connectionState, setConnectionState] = useState<ConnectionState>('connecting');

  useEffect(() => {
    eventsRef.current = [];
    countersRef.current = {};
    phaseRef.current = null;
    finishedRef.current = { done: false, status: null };
    setConnectionState('connecting');
    bump();

    if (!orgId || !runId) return;

    let stream: RunStream | null = null;
    const handlers: RunStreamHandlers = {
      onEvent: (record) => {
        eventsRef.current.push(record);
        if (record.type === 'run.event') {
          countersRef.current = applyRunEventToCounters(countersRef.current, record.payload as RunEventPayload);
          phaseRef.current = (record.payload as RunEventPayload).phase;
        } else {
          finishedRef.current = { done: true, status: (record.payload as RunResultPayload).status };
          // The run is over - stop reconnecting/polling instead of running forever in the background.
          stream?.stop();
        }
        bump();
      },
      onConnectionState: setConnectionState,
    };

    stream = new RunStream(
      {
        runId,
        initialSince: 0,
        fetchEventsSince: (since) =>
          apiFetch<RunEventsPage>(`/api/v1/runs/${runId}/events?since=${since}`, { orgId }),
        transportFactory: getRunTransportFactory(),
      },
      handlers,
    );
    stream.start();

    return () => stream?.stop();
  }, [runId, orgId]);

  return {
    events: eventsRef.current,
    connectionState,
    counters: countersRef.current,
    phase: phaseRef.current,
    finished: finishedRef.current.done,
    finishedStatus: finishedRef.current.status,
  };
}
