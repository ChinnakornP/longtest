'use client';

import type { Artifact, ExecutionResult } from '@qa/schema';

import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { useArtifactUrl } from '@/lib/api/hooks/use-artifact-url';

export function ExecutionDetailDialog({
  execution,
  onOpenChange,
}: {
  execution: ExecutionResult | null;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={execution !== null} onOpenChange={onOpenChange}>
      <DialogContent>
        {execution && (
          <>
            <DialogHeader>
              <DialogTitle>{execution.testCaseId}</DialogTitle>
              <DialogDescription>
                {execution.result === 'fail' ? execution.message ?? 'Assertion failed.' : `Result: ${execution.result}`}
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4">
              <StepList execution={execution} />
              <div className="space-y-2">
                <h4 className="text-sm font-medium">Evidence</h4>
                {execution.artifacts.length === 0 ? (
                  <p className="text-muted-foreground text-sm">No artifacts captured.</p>
                ) : (
                  <ul className="space-y-2">
                    {execution.artifacts.map((artifact) => (
                      <ArtifactRow key={artifact.id} artifact={artifact} />
                    ))}
                  </ul>
                )}
              </div>
            </div>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}

function StepList({ execution }: { execution: ExecutionResult }) {
  return (
    <div className="space-y-1">
      <h4 className="text-sm font-medium">Steps</h4>
      <ol className="space-y-1 text-sm">
        {execution.steps.map((step) => (
          <li key={step.index} className="flex items-center gap-2">
            <span
              className={
                step.status === 'pass'
                  ? 'text-emerald-600'
                  : step.status === 'fail' || step.status === 'error'
                    ? 'text-red-600'
                    : 'text-muted-foreground'
              }
            >
              {step.status}
            </span>
            <span>{step.action}</span>
            {step.message && <span className="text-muted-foreground">— {step.message}</span>}
          </li>
        ))}
      </ol>
    </div>
  );
}

function ArtifactRow({ artifact }: { artifact: Artifact }) {
  const { data, isLoading } = useArtifactUrl(artifact.id);
  const filename = artifact.key.split('/').pop() ?? artifact.id;

  return (
    <li className="border-border flex items-center justify-between gap-3 rounded-md border p-2 text-sm">
      <div>
        <p className="font-medium capitalize">{artifact.kind}</p>
        <p className="text-muted-foreground text-xs">{filename}</p>
      </div>
      {artifact.kind === 'screenshot' && data && (
        <img
          src={data.url}
          alt={`${artifact.kind} for ${filename}`}
          className="h-12 w-12 rounded border object-cover"
        />
      )}
      {data ? (
        <a
          href={data.url}
          target="_blank"
          rel="noreferrer"
          className="text-primary shrink-0 text-sm font-medium hover:underline"
        >
          Open
        </a>
      ) : (
        <span className="text-muted-foreground text-xs">{isLoading ? 'Loading…' : 'Unavailable'}</span>
      )}
    </li>
  );
}
