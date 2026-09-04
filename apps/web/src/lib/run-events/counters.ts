/**
 * Maps a daemon-envelope `run.event` `code` to the RunCounters field it
 * increments. This vocabulary (page_discovered, test_passed, ...) is a
 * frontend assumption - the daemon-envelope@1 schema only documents `code`
 * as "machine-readable, e.g. page_discovered", it does not enumerate the
 * full set. T09/T13-T15 own the real vocabulary; flagged for Architect to
 * confirm once they land. Shared between the dev mock run generator
 * (mocks/qa-store.ts) and the live view so both agree on one source of
 * truth instead of drifting.
 */
import type { RunEventPayload } from '@qa/schema';

import type { RunCounters } from '@/lib/api/qa-types';

const COUNTER_FIELD_BY_CODE: Partial<Record<string, keyof RunCounters>> = {
  page_discovered: 'pages',
  workflow_discovered: 'workflows',
  form_discovered: 'forms',
  action_discovered: 'actions',
  test_passed: 'passed',
  test_failed: 'failed',
  test_skipped: 'skipped',
};

export function applyRunEventToCounters(counters: RunCounters, payload: RunEventPayload): RunCounters {
  const field = COUNTER_FIELD_BY_CODE[payload.code];
  if (!field) return counters;
  return { ...counters, [field]: (counters[field] ?? 0) + 1 };
}

export function foldRunCounters(payloads: RunEventPayload[]): RunCounters {
  return payloads.reduce(applyRunEventToCounters, {});
}
