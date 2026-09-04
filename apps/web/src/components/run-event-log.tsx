'use client';

import type { RunEventPayload, RunEventPayloadLevel, RunResultPayload } from '@qa/schema';
import { useVirtualizer } from '@tanstack/react-virtual';
import { useRef } from 'react';

import type { RunEventRecord } from '@/lib/api/qa-types';
import { cn } from '@/lib/utils';

const ROW_HEIGHT = 32;

const LEVEL_COLOR: Record<RunEventPayloadLevel, string> = {
  debug: 'text-muted-foreground',
  info: 'text-foreground',
  warn: 'text-amber-600',
  error: 'text-red-600',
};

/**
 * Virtualized so a 10k-event run stays scroll-smooth: only rows within the
 * viewport (+ overscan) are ever mounted, regardless of how long `events`
 * gets. See run-event-log.test.tsx for the assertion that the DOM node
 * count does not grow with the event count.
 */
export function RunEventLog({ events }: { events: RunEventRecord[] }) {
  const parentRef = useRef<HTMLDivElement>(null);
  const virtualizer = useVirtualizer({
    count: events.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 12,
  });

  if (events.length === 0) {
    return <p className="text-muted-foreground py-8 text-center text-sm">No events yet.</p>;
  }

  return (
    <div
      ref={parentRef}
      className="border-border h-96 overflow-y-auto rounded-lg border font-mono text-xs"
      role="log"
      aria-label="Run event log"
    >
      <div style={{ height: virtualizer.getTotalSize(), width: '100%', position: 'relative' }}>
        {virtualizer.getVirtualItems().map((virtualRow) => {
          const record = events[virtualRow.index];
          if (!record) return null;
          return (
            <div
              key={record.seq}
              data-index={virtualRow.index}
              style={{
                position: 'absolute',
                top: 0,
                left: 0,
                width: '100%',
                height: ROW_HEIGHT,
                transform: `translateY(${virtualRow.start}px)`,
              }}
              className="border-border/60 flex items-center gap-2 border-b px-3"
            >
              <EventRow record={record} />
            </div>
          );
        })}
      </div>
    </div>
  );
}

function EventRow({ record }: { record: RunEventRecord }) {
  if (record.type === 'run.result') {
    const payload = record.payload as RunResultPayload;
    return (
      <span className="text-muted-foreground truncate">
        [{formatTime(record.ts)}] run finished: {payload.status}
      </span>
    );
  }

  const payload = record.payload as RunEventPayload;
  return (
    <>
      <span className="text-muted-foreground w-20 shrink-0">{formatTime(record.ts)}</span>
      <span className={cn('w-12 shrink-0 uppercase', LEVEL_COLOR[payload.level])}>{payload.level}</span>
      <span className="text-muted-foreground w-16 shrink-0 uppercase">{payload.phase}</span>
      {payload.testCaseId && <span className="shrink-0 font-semibold">{payload.testCaseId}</span>}
      <span className="flex-1 truncate">{payload.message}</span>
    </>
  );
}

function formatTime(ts: string): string {
  const date = new Date(ts);
  return Number.isNaN(date.getTime()) ? ts : date.toLocaleTimeString();
}
