import { waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { RunEventRecord, RunEventsPage } from '@/lib/api/qa-types';

import { RunStream } from './run-stream';
import type { RunTransport, RunTransportHandlers, TransportFactory } from './types';

class FakeTransport implements RunTransport {
  handlers: RunTransportHandlers | null = null;
  stopCount = 0;

  start(handlers: RunTransportHandlers): void {
    this.handlers = handlers;
  }

  stop(): void {
    this.stopCount += 1;
  }
}

function event(seq: number, message: string): RunEventRecord {
  return {
    seq,
    type: 'run.event',
    ts: new Date(seq).toISOString(),
    payload: { phase: 'discover', level: 'info', code: 'page_discovered', message },
  };
}

describe('RunStream', () => {
  it('catches up via REST from the given since, then applies live events without duplicates', async () => {
    const applied: RunEventRecord[] = [];
    const transports: FakeTransport[] = [];
    const transportFactory: TransportFactory = () => {
      const transport = new FakeTransport();
      transports.push(transport);
      return transport;
    };

    const backlog = [event(1, 'a'), event(2, 'b'), event(3, 'c')];
    const fetchEventsSince = vi.fn(
      async (since: number): Promise<RunEventsPage> => ({
        events: backlog.filter((e) => e.seq > since),
        nextSince: Math.max(since, ...backlog.map((e) => e.seq), 0),
      }),
    );

    const stream = new RunStream(
      { runId: 'run-1', fetchEventsSince, transportFactory, maxBackoffMs: 50 },
      { onEvent: (record) => applied.push(record), onConnectionState: () => {} },
    );

    stream.start();

    await waitFor(() => expect(applied.map((e) => e.seq)).toEqual([1, 2, 3]));
    expect(fetchEventsSince).toHaveBeenCalledWith(0);
    expect(transports).toHaveLength(1);

    // A naive/mock transport that replays from the beginning must not produce duplicates.
    transports[0]!.handlers?.onMessage(backlog[0]!);
    transports[0]!.handlers?.onMessage(backlog[1]!);
    expect(applied.map((e) => e.seq)).toEqual([1, 2, 3]);

    // A genuinely new live event is still applied.
    const four = event(4, 'd');
    backlog.push(four);
    transports[0]!.handlers?.onMessage(four);
    expect(applied.map((e) => e.seq)).toEqual([1, 2, 3, 4]);

    stream.stop();
  });

  it('resumes from the last seen seq and loses nothing across a reconnect', async () => {
    const applied: RunEventRecord[] = [];
    const transports: FakeTransport[] = [];
    const transportFactory: TransportFactory = () => {
      const transport = new FakeTransport();
      transports.push(transport);
      return transport;
    };

    const backlog = [event(1, 'a'), event(2, 'b')];
    const fetchEventsSince = vi.fn(
      async (since: number): Promise<RunEventsPage> => ({
        events: backlog.filter((e) => e.seq > since),
        nextSince: Math.max(since, ...backlog.map((e) => e.seq), 0),
      }),
    );

    const stream = new RunStream(
      { runId: 'run-1', fetchEventsSince, transportFactory, maxBackoffMs: 50 },
      { onEvent: (record) => applied.push(record), onConnectionState: () => {} },
    );

    stream.start();
    await waitFor(() => expect(transports).toHaveLength(1));
    await waitFor(() => expect(applied.map((e) => e.seq)).toEqual([1, 2]));

    // Connection drops before event 3 arrives live.
    transports[0]!.handlers?.onStateChange('closed');
    const three = event(3, 'c');
    backlog.push(three);

    await waitFor(() => expect(transports).toHaveLength(2), { timeout: 2000 });
    expect(fetchEventsSince).toHaveBeenLastCalledWith(2);
    expect(transports[0]!.stopCount).toBe(0); // RunStream never re-stops an already-closed transport

    transports[1]!.handlers?.onMessage(three);
    expect(applied.map((e) => e.seq)).toEqual([1, 2, 3]);

    stream.stop();
    expect(transports[1]!.stopCount).toBe(1);
  });

  it('reports offline after repeated catch-up failures and keeps retrying', async () => {
    const states: string[] = [];
    const fetchEventsSince = vi.fn(async (): Promise<RunEventsPage> => {
      throw new Error('network down');
    });
    const transportFactory: TransportFactory = () => new FakeTransport();

    const stream = new RunStream(
      { runId: 'run-1', fetchEventsSince, transportFactory, maxBackoffMs: 10 },
      { onEvent: () => {}, onConnectionState: (state) => states.push(state) },
    );

    stream.start();
    await waitFor(() => expect(states).toContain('offline'), { timeout: 3000 });
    stream.stop();
  });
});
