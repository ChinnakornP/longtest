/**
 * @fileoverview Progress emission for the crawler.
 *
 * The crawler reuses the existing executor event channel from T06: a
 * `{ "event": "progress", "data": {...} }` JSON-RPC frame on stdout. The
 * daemon's `onExecutorEvent` already fans these out as `executor_progress`
 * run events (see `daemon/runtime/execute.go`).
 *
 * The shape of `data` is a small object so the UI can render a number
 * without parsing free-form strings. The keys are stable so the UI does
 * not have to special-case strings.
 */

export interface CrawlProgress {
  /** Where we are in the crawl. */
  phase: 'starting' | 'fetching' | 'extracting' | 'deduplicating' | 'finalising' | 'done';
  /** Pages discovered so far. -1 for "we have not counted yet". */
  pagesDiscovered: number;
  /** Elements observed so far (across all pages). */
  elementsObserved: number;
  /** Number of `<form>` elements seen. */
  formsObserved: number;
  /** Number of interactive elements seen (buttons/links/inputs/etc). */
  actionsObserved: number;
  /** When the progress event was emitted (ms since epoch). */
  ts: number;
}

/**
 * Sink for progress events. The runner / stdio-loop owns the actual
 * `process.stdout.write` of the JSON-RPC frame; this interface lets the
 * crawler stay free of stdio.
 */
export interface ProgressSink {
  emit(progress: CrawlProgress): void;
}

/** A sink that drops every event. Useful in tests. */
export const nullProgressSink: ProgressSink = {
  emit: () => {
    /* drop */
  },
};

export function progressSnapshot(input: {
  phase: CrawlProgress['phase'];
  pagesDiscovered: number;
  elementsObserved: number;
  formsObserved: number;
  actionsObserved: number;
}): CrawlProgress {
  return { ...input, ts: Date.now() };
}
