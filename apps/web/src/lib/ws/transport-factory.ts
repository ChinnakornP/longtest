import { resolveApiBase } from '@/lib/api/client';
import { createPollingTransport } from '@/mocks/ws-polling-transport';

import { createWebSocketTransport } from './websocket-transport';
import type { TransportFactory } from './types';

/** Mirrors resolveApiBase's mock-vs-real split in lib/api/client.ts: no configured API base means the dev mock backend, which has no real WS server behind it. */
export function getRunTransportFactory(): TransportFactory {
  const apiBase = resolveApiBase();
  if (!apiBase) {
    return createPollingTransport();
  }
  const wsUrl = `${apiBase.replace(/^http/, 'ws')}/api/v1/ws`;
  return createWebSocketTransport(wsUrl);
}
