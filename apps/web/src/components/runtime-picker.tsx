import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import type { Runtime } from '@/lib/api/qa-types';

export function RuntimePicker({
  runtimes,
  selectedId,
  onSelect,
}: {
  runtimes: Runtime[];
  selectedId: string | null;
  onSelect: (runtimeId: string) => void;
}) {
  return (
    <fieldset className="space-y-2">
      <legend className="sr-only">Choose a runtime</legend>
      {runtimes.map((runtime) => (
        <label
          key={runtime.id}
          className={cn(
            'border-border flex cursor-pointer items-start gap-3 rounded-lg border p-3 text-sm has-[:checked]:border-primary',
            !runtime.online && 'cursor-not-allowed opacity-60',
          )}
        >
          <input
            type="radio"
            name="runtime"
            className="mt-1"
            checked={selectedId === runtime.id}
            disabled={!runtime.online}
            onChange={() => onSelect(runtime.id)}
          />
          <div className="flex-1 space-y-1">
            <div className="flex items-center gap-2">
              <span className="font-medium">{runtime.name}</span>
              <span className={cn('inline-block size-2 rounded-full', runtime.online ? 'bg-emerald-500' : 'bg-muted-foreground')} />
              <span className="text-muted-foreground text-xs">{runtime.online ? 'Online' : 'Offline'}</span>
            </div>
            <div className="flex flex-wrap gap-1.5">
              {runtime.browsers.map((browser) => (
                <Badge key={browser} variant="outline">
                  {browser}
                </Badge>
              ))}
              {runtime.agents.map((agent) => (
                <Badge key={agent.name} variant="outline" className={!agent.ok ? 'text-muted-foreground' : undefined}>
                  {agent.name}
                  {!agent.ok ? ' (unavailable)' : ''}
                </Badge>
              ))}
            </div>
            {!runtime.online && runtime.lastSeenAt && (
              <p className="text-muted-foreground text-xs">
                Last seen {new Date(runtime.lastSeenAt).toLocaleString()}
              </p>
            )}
          </div>
        </label>
      ))}
    </fieldset>
  );
}
