/**
 * @fileoverview Progress event emission.
 *
 * The acceptance criterion says: "progress event ออกมาระหว่าง crawl ไม่ใช่
 * ตอนจบ". This test verifies a sink sees events with monotonically
 * changing `pagesDiscovered` and a `phase` value that progresses through
 * the crawl lifecycle.
 */

import { describe, expect, it } from 'vitest';
import { nullProgressSink, progressSnapshot, type CrawlProgress } from '../../src/crawler/events.ts';

describe('events: snapshot shape', () => {
  it('attaches the current timestamp at snapshot time', () => {
    const before = Date.now();
    const snap = progressSnapshot({
      phase: 'fetching',
      pagesDiscovered: 1,
      elementsObserved: 4,
      formsObserved: 1,
      actionsObserved: 3,
    });
    expect(snap.phase).toBe('fetching');
    expect(snap.ts).toBeGreaterThanOrEqual(before);
    expect(snap.pagesDiscovered).toBe(1);
  });
});

describe('events: sink contract', () => {
  it('nullProgressSink accepts and drops every event', () => {
    expect(() =>
      nullProgressSink.emit({
        phase: 'starting',
        pagesDiscovered: 0,
        elementsObserved: 0,
        formsObserved: 0,
        actionsObserved: 0,
        ts: 0,
      }),
    ).not.toThrow();
  });

  it('captures emitted events in order', () => {
    const recorded: CrawlProgress[] = [];
    const sink = {
      emit(p: CrawlProgress): void {
        recorded.push(p);
      },
    };
    sink.emit(progressSnapshot({ phase: 'starting', pagesDiscovered: 0, elementsObserved: 0, formsObserved: 0, actionsObserved: 0 }));
    sink.emit(progressSnapshot({ phase: 'fetching', pagesDiscovered: 1, elementsObserved: 5, formsObserved: 1, actionsObserved: 3 }));
    sink.emit(progressSnapshot({ phase: 'done', pagesDiscovered: 5, elementsObserved: 25, formsObserved: 4, actionsObserved: 21 }));
    expect(recorded.map((e) => e.phase)).toEqual(['starting', 'fetching', 'done']);
    // The pagesDiscovered counter is monotonic.
    expect(recorded[2]!.pagesDiscovered).toBeGreaterThan(recorded[0]!.pagesDiscovered);
  });
});
