import { apiFetch } from '@/lib/api/client';
import type { RunEventsPage } from '@/lib/api/qa-types';
import type { RunTransport, TransportFactory } from '@/lib/ws/types';

const POLL_INTERVAL_MS = 400;

/**
 * Dev-only stand-in for `WS /api/v1/ws?runId=` (T08, not yet landed - and
 * even once it lands, a Next.js route handler cannot upgrade to a real
 * WebSocket without a custom server). Polls the same
 * `GET /runs/{id}/events?since=` mock endpoint the REST catch-up already
 * uses, replaying from seq 0 every tick, so it can never disagree with that
 * endpoint and it exercises RunStream's real dedupe/resume logic rather than
 * a second, separate fake of it. Selected by transport-factory.ts only when
 * NEXT_PUBLIC_API_BASE_URL is unset (dev against the mock backend) - never
 * the live transport.
 */
export function createPollingTransport(): TransportFactory {
  return (runId: string): RunTransport => {
    let timer: ReturnType<typeof setInterval> | null = null;
    let stopped = false;

    return {
      start(handlers) {
        handlers.onStateChange('open');

        const poll = async () => {
          if (stopped) return;
          try {
            const page = await apiFetch<RunEventsPage>(`/api/v1/runs/${runId}/events?since=0`);
            if (stopped) return;
            for (const record of page.events) handlers.onMessage(record);
          } catch {
            if (!stopped) handlers.onStateChange('error');
          }
        };

        void poll();
        timer = setInterval(() => void poll(), POLL_INTERVAL_MS);
      },
      stop() {
        stopped = true;
        if (timer) clearInterval(timer);
        timer = null;
      },
    };
  };
}
