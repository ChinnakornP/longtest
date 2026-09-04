import { cn } from '@/lib/utils';

interface ProgressProps {
  /** 0-100. Omit for an indeterminate bar (still running, no known total). */
  value?: number;
  className?: string;
}

export function Progress({ value, className }: ProgressProps) {
  const clamped = value === undefined ? undefined : Math.min(100, Math.max(0, value));
  return (
    <div
      className={cn('bg-muted h-2 w-full overflow-hidden rounded-full', className)}
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={clamped}
    >
      <div
        className={cn(
          'bg-primary h-full rounded-full transition-[width]',
          clamped === undefined && 'w-1/3 animate-pulse',
        )}
        style={clamped === undefined ? undefined : { width: `${clamped}%` }}
      />
    </div>
  );
}
