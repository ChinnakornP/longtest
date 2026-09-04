import type { RunEventPayload } from '@qa/schema';
import { describe, expect, it } from 'vitest';

import { applyRunEventToCounters, foldRunCounters } from './counters';

function payload(code: string): RunEventPayload {
  return { phase: 'discover', level: 'info', code, message: code };
}

describe('applyRunEventToCounters', () => {
  it('increments the mapped counter field', () => {
    const next = applyRunEventToCounters({}, payload('page_discovered'));
    expect(next).toEqual({ pages: 1 });
  });

  it('leaves counters unchanged for an unmapped code', () => {
    const start = { pages: 2 };
    expect(applyRunEventToCounters(start, payload('agent_output_invalid'))).toBe(start);
  });

  it('folds a sequence of events into cumulative counters', () => {
    const events = [
      payload('page_discovered'),
      payload('page_discovered'),
      payload('workflow_discovered'),
      payload('test_passed'),
      payload('test_failed'),
    ];
    expect(foldRunCounters(events)).toEqual({ pages: 2, workflows: 1, passed: 1, failed: 1 });
  });
});
