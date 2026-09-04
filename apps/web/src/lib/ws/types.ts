import type { RunEventRecord } from '@/lib/api/qa-types';

export type TransportState = 'open' | 'closed' | 'error';

export interface RunTransportHandlers {
  onMessage: (record: RunEventRecord) => void;
  onStateChange: (state: TransportState) => void;
}

/** One connection attempt to the run's live event stream. No internal reconnect - RunStream owns retry/backoff and creates a fresh transport per attempt. */
export interface RunTransport {
  start(handlers: RunTransportHandlers): void;
  stop(): void;
}

export type TransportFactory = (runId: string) => RunTransport;

export type ConnectionState = 'connecting' | 'open' | 'reconnecting' | 'offline';
