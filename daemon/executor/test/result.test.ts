/**
 * Result envelope: every ExecutionResult the executor produces must satisfy
 * `execution-result@1`. These tests prove the round-trip both for hand-built
 * fixtures (matching the ones in `packages/qa-schema/fixtures/`) and for
 * the cases the runner actually emits.
 */

import { describe, expect, it } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { validate } from '@qa/schema';
import { ResultBuilder, emptyStepResult, emptyAssertionResult } from '../src/result.ts';

const FIXTURES = join(__dirname, '..', '..', '..', 'packages', 'qa-schema', 'fixtures');

function loadFixture(category: 'valid' | 'invalid', name: string): unknown {
  const text = readFileSync(join(FIXTURES, 'execution-result', category, name), 'utf8');
  return JSON.parse(text);
}

describe('ResultBuilder', () => {
  it('produces a passing result for an empty success path', () => {
    const startedAt = new Date('2026-09-04T09:15:00Z');
    const builder = new ResultBuilder({ testCaseId: 'TC-100', startedAt });
    builder.addStep(emptyStepResult(0, 'navigate', 'pass'));
    const endedAt = new Date('2026-09-04T09:15:01Z');
    builder.end(endedAt);
    const result = builder.finalise();
    expect(result.result).toBe('pass');
    expect(result.steps.length).toBe(1);
    expect(result.startedAt).toBe(startedAt.toISOString());
    expect(result.endedAt).toBe(endedAt.toISOString());
    expect(result.durationMs).toBe(1000);
  });

  it('flags any fail as fail and skips nothing as skipped', () => {
    const startedAt = new Date('2026-09-04T09:15:00Z');
    const builder = new ResultBuilder({ testCaseId: 'TC-101', startedAt });
    builder.addStep(emptyStepResult(0, 'navigate', 'pass'));
    builder.addAssertion(emptyAssertionResult(0, 'visible', 'fail'));
    builder.end(new Date('2026-09-04T09:15:01Z'));
    const result = builder.finalise();
    expect(result.result).toBe('fail');
  });

  it('flags any error step as error (harness failure)', () => {
    const startedAt = new Date('2026-09-04T09:15:00Z');
    const builder = new ResultBuilder({ testCaseId: 'TC-102', startedAt });
    builder.addStep(emptyStepResult(0, 'navigate', 'error'));
    builder.end(new Date('2026-09-04T09:15:01Z'));
    const result = builder.finalise();
    expect(result.result).toBe('error');
  });

  it('reports skipped when no steps and no assertions ran', () => {
    const startedAt = new Date('2026-09-04T09:15:00Z');
    const builder = new ResultBuilder({ testCaseId: 'TC-103', startedAt });
    builder.end(new Date('2026-09-04T09:15:00Z'));
    const result = builder.finalise();
    expect(result.result).toBe('skipped');
  });

  it('throws if the produced result fails schema validation', () => {
    const startedAt = new Date('2026-09-04T09:15:00Z');
    const builder = new ResultBuilder({ testCaseId: 'TC-104', startedAt });
    // Add a step with an invalid action — schema enum v1 has no "scroll"
    builder.addStep({ index: 0, action: 'scroll' as never, status: 'pass' });
    builder.end(new Date('2026-09-04T09:15:01Z'));
    expect(() => builder.finalise()).toThrow(/execution-result@1/);
  });
});

describe('execution-result schema fixtures', () => {
  // Round-trip: every fixture from the qa-schema fixtures directory must
  // still validate through the executor's call site. This is the same
  // round-trip the runner relies on at finalise() time.
  const validFiles = readdirSync(join(FIXTURES, 'execution-result', 'valid')).sort();
  const invalidFiles = readdirSync(join(FIXTURES, 'execution-result', 'invalid')).sort();

  it.each(validFiles)('valid fixture %s validates', (file) => {
    const result = validate('execution-result@1', loadFixture('valid', file));
    expect(result.valid).toBe(true);
  });

  it.each(invalidFiles)('invalid fixture %s is rejected', (file) => {
    const result = validate('execution-result@1', loadFixture('invalid', file));
    expect(result.valid).toBe(false);
  });
});
