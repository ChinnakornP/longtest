import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import type { CoverageStatus } from '@/lib/api/qa-types';

const LABEL: Record<CoverageStatus, string> = {
  covered: 'Covered',
  partial: 'Partial',
  uncovered: 'Uncovered',
};

/**
 * `partial` is deliberately not a lighter shade of `covered`'s green — several
 * cases each walking part of a workflow is not "almost covered", it is a
 * distinct, unresolved state. Never render this as a progress bar.
 */
const COLOR: Record<CoverageStatus, string> = {
  covered: 'bg-emerald-600 text-white border-transparent',
  partial: 'bg-amber-500 text-white border-transparent',
  uncovered: 'bg-red-600 text-white border-transparent',
};

export function CoverageStatusBadge({ status }: { status: CoverageStatus }) {
  return <Badge className={cn(COLOR[status])}>{LABEL[status]}</Badge>;
}
