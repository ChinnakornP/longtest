import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import type { RunStatus } from '@/lib/api/qa-types';

const LABEL: Record<RunStatus, string> = {
  queued: 'Queued',
  running: 'Running',
  completed: 'Completed',
  failed: 'Failed',
  cancelled: 'Cancelled',
  error: 'Error',
};

const COLOR: Record<RunStatus, string> = {
  queued: 'bg-muted text-foreground border-transparent',
  running: 'bg-blue-600 text-white border-transparent',
  completed: 'bg-emerald-600 text-white border-transparent',
  failed: 'bg-red-600 text-white border-transparent',
  cancelled: 'bg-muted text-muted-foreground border-transparent',
  error: 'bg-red-600 text-white border-transparent',
};

export function RunStatusBadge({ status }: { status: RunStatus }) {
  return <Badge className={cn(COLOR[status])}>{LABEL[status]}</Badge>;
}
