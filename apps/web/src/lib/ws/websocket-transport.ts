import type { RunEventRecord } from '@/lib/api/qa-types';

import type { RunTransport, TransportFactory } from './types';

/**
 * `WS /api/v1/ws?runId=...` (T08). Read-only from the browser's side - the
 * daemon-envelope@1 frame carries more message types than a browser
 * subscriber cares about (hello/heartbeat/run.assign/run.cancel are
 * daemon<->server only), so anything that is not run.event/run.result is
 * dropped rather than surfaced.
 */
export function createWebSocketTransport(wsBaseUrl: string): TransportFactory {
  return (runId: string): RunTransport => {
    let socket: WebSocket | null = null;

    return {
      start(handlers) {
        const separator = wsBaseUrl.includes('?') ? '&' : '?';
        const url = `${wsBaseUrl}${separator}runId=${encodeURIComponent(runId)}`;
        socket = new WebSocket(url);

        socket.addEventListener('open', () => handlers.onStateChange('open'));
        socket.addEventListener('close', () => handlers.onStateChange('closed'));
        socket.addEventListener('error', () => handlers.onStateChange('error'));
        socket.addEventListener('message', (event: MessageEvent) => {
          const record = parseFrame(event.data);
          if (record) handlers.onMessage(record);
        });
      },
      stop() {
        socket?.close();
        socket = null;
      },
    };
  };
}

function parseFrame(data: unknown): RunEventRecord | null {
  if (typeof data !== 'string') return null;
  try {
    const parsed = JSON.parse(data) as { type?: unknown; seq?: unknown; ts?: unknown; payload?: unknown };
    if (
      (parsed.type === 'run.event' || parsed.type === 'run.result') &&
      typeof parsed.seq === 'number' &&
      typeof parsed.ts === 'string' &&
      typeof parsed.payload === 'object' &&
      parsed.payload !== null
    ) {
      return { seq: parsed.seq, type: parsed.type, ts: parsed.ts, payload: parsed.payload } as RunEventRecord;
    }
    return null;
  } catch {
    return null;
  }
}
