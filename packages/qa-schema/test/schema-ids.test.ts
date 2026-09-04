import { describe, expect, it } from 'vitest';
import { SCHEMA_IDS, isSchemaId } from '../src/index.js';

describe('schema ids', () => {
  it('exposes every stage-1 contract', () => {
    expect([...SCHEMA_IDS]).toEqual([
      'test-case@1',
      'application-map@1',
      'finding@1',
      'daemon-envelope@1',
    ]);
  });

  it('rejects an unknown id instead of guessing', () => {
    expect(isSchemaId('test-case@1')).toBe(true);
    expect(isSchemaId('test-case@2')).toBe(false);
    expect(isSchemaId('../../etc/passwd')).toBe(false);
  });
});
