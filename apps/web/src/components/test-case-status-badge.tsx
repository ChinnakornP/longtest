import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import type { TestCaseStatus } from '@/lib/api/qa-types';

const LABEL: Record<TestCaseStatus, string> = {
  draft: 'Draft',
  approved: 'Approved',
  archived: 'Archived',
};

const COLOR: Record<TestCaseStatus, string> = {
  draft: 'bg-amber-100 text-amber-900 border-transparent',
  approved: 'bg-emerald-600 text-white border-transparent',
  archived: 'bg-muted text-muted-foreground border-transparent',
};

export function TestCaseStatusBadge({ status }: { status: TestCaseStatus }) {
  return <Badge className={cn(COLOR[status])}>{LABEL[status]}</Badge>;
}
