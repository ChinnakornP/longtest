import type { RunEventRecord, RunEventsPage } from '@/lib/api/qa-types';

import type { ConnectionState, RunTransport, TransportFactory } from './types';

export interface RunStreamHandlers {
  onEvent: (record: RunEventRecord) => void;
  onConnectionState: (state: ConnectionState) => void;
}

export interface RunStreamOptions {
  runId: string;
  /** GET /runs/{id}/events?since={seq} - also what the reconnect path uses to close any gap, per the T11 resume requirement. */
  fetchEventsSince: (since: number) => Promise<RunEventsPage>;
  transportFactory: TransportFactory;
  /** Resume point for a page opened mid-run. Defaults to 0 (fetch full history) - see the T11 acceptance criterion for a fresh page load. */
  initialSince?: number;
  maxBackoffMs?: number;
}

/**
 * Transport-agnostic run event stream: REST catch-up, then a live transport,
 * with backoff reconnect that re-catches-up before resubscribing. Dedupes on
 * `seq` so at-least-once delivery (daemon-envelope@1) never reaches the UI
 * twice, and a transport that replays from the beginning (see
 * mocks/ws-polling-transport.ts) is handled the same way a real WS resuming
 * from an earlier point would be.
 */
export class RunStream {
  private readonly seenSeqs = new Set<number>();
  private maxSeq: number;
  private transport: RunTransport | null = null;
  private stopped = false;
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  private attempt = 0;

  constructor(
    private readonly options: RunStreamOptions,
    private readonly handlers: RunStreamHandlers,
  ) {
    this.maxSeq = options.initialSince ?? 0;
  }

  start(): void {
    this.stopped = false;
    void this.catchUpThenConnect();
  }

  stop(): void {
    this.stopped = true;
    if (this.retryTimer) clearTimeout(this.retryTimer);
    this.retryTimer = null;
    this.transport?.stop();
    this.transport = null;
  }

  private applyRecord(record: RunEventRecord): void {
    if (this.seenSeqs.has(record.seq)) return;
    this.seenSeqs.add(record.seq);
    if (record.seq > this.maxSeq) this.maxSeq = record.seq;
    this.handlers.onEvent(record);
  }

  private async catchUpThenConnect(): Promise<void> {
    if (this.stopped) return;
    this.handlers.onConnectionState(this.attempt === 0 ? 'connecting' : 'reconnecting');

    try {
      const page = await this.options.fetchEventsSince(this.maxSeq);
      if (this.stopped) return;
      for (const record of page.events) this.applyRecord(record);
    } catch {
      if (!this.stopped) this.scheduleRetry();
      return;
    }

    this.connect();
  }

  private connect(): void {
    if (this.stopped) return;
    const transport = this.options.transportFactory(this.options.runId);
    this.transport = transport;
    transport.start({
      onMessage: (record) => this.applyRecord(record),
      onStateChange: (state) => {
        if (this.stopped) return;
        if (state === 'open') {
          this.attempt = 0;
          this.handlers.onConnectionState('open');
        } else {
          this.transport = null;
          this.scheduleRetry();
        }
      },
    });
  }

  private static readonly OFFLINE_AFTER_ATTEMPTS = 5;

  private scheduleRetry(): void {
    if (this.stopped) return;
    this.handlers.onConnectionState(
      this.attempt >= RunStream.OFFLINE_AFTER_ATTEMPTS ? 'offline' : 'reconnecting',
    );
    const base = 500;
    const max = this.options.maxBackoffMs ?? 10_000;
    const delay = Math.min(max, base * 2 ** this.attempt) + Math.random() * 250;
    this.attempt += 1;
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null;
      void this.catchUpThenConnect();
    }, delay);
  }
}
