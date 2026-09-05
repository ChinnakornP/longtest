import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import type { RiskLevel } from '@/lib/api/qa-types';

const LABEL: Record<RiskLevel, string> = {
  high: 'High risk',
  medium: 'Medium risk',
  low: 'Low risk',
};

const COLOR: Record<RiskLevel, string> = {
  high: 'border-red-600 text-red-700',
  medium: 'border-amber-500 text-amber-700',
  low: 'border-border text-muted-foreground',
};

export function RiskBadge({ risk }: { risk: RiskLevel }) {
  return (
    <Badge variant="outline" className={cn(COLOR[risk])}>
      {LABEL[risk]}
    </Badge>
  );
}
