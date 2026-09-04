'use client';

import type { ExecutionResult } from '@qa/schema';
import { useState } from 'react';

import { ExecutionDetailDialog } from '@/components/artifact-viewer-dialog';
import { cn } from '@/lib/utils';

const RESULT_COLOR: Record<ExecutionResult['result'], string> = {
  pass: 'text-emerald-600',
  fail: 'text-red-600',
  error: 'text-red-600',
  skipped: 'text-muted-foreground',
};

const RESULT_ICON: Record<ExecutionResult['result'], string> = {
  pass: '✓',
  fail: '✗',
  error: '✗',
  skipped: '○',
};

export function ExecutionList({ executions }: { executions: ExecutionResult[] }) {
  const [selected, setSelected] = useState<ExecutionResult | null>(null);

  if (executions.length === 0) {
    return <p className="text-muted-foreground text-sm">No executions yet.</p>;
  }

  return (
    <>
      <ul className="divide-border border-border divide-y rounded-lg border">
        {executions.map((execution) => (
          <li key={execution.testCaseId}>
            <button
              type="button"
              onClick={() => setSelected(execution)}
              className="hover:bg-muted focus-visible:ring-primary flex w-full items-center gap-3 px-4 py-2.5 text-left text-sm focus-visible:ring-2 focus-visible:outline-none"
            >
              <span className={cn('w-4 shrink-0 font-semibold', RESULT_COLOR[execution.result])}>
                {RESULT_ICON[execution.result]}
              </span>
              <span className="flex-1 truncate">{execution.testCaseId}</span>
              {execution.artifacts.length > 0 && (
                <span className="text-muted-foreground text-xs">{execution.artifacts.length} artifacts</span>
              )}
            </button>
          </li>
        ))}
      </ul>
      <ExecutionDetailDialog execution={selected} onOpenChange={(open) => !open && setSelected(null)} />
    </>
  );
}
