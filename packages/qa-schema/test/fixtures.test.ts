import { describe, expect, it } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { SCHEMA_IDS, validateJson, type SchemaId } from '../src/index.js';

const FIXTURES = join(dirname(fileURLToPath(import.meta.url)), '..', 'fixtures');

interface Expectation {
  schemaId: string;
  valid: boolean;
  errors: { instancePath: string; keyword: string; message: string }[];
}

const expectations: Record<string, Expectation> = JSON.parse(
  readFileSync(join(FIXTURES, 'expectations.json'), 'utf8'),
);

function fixturesFor(id: SchemaId, bucket: 'valid' | 'invalid'): string[] {
  const name = id.slice(0, id.lastIndexOf('@'));
  return readdirSync(join(FIXTURES, name, bucket))
    .filter((f) => f.endsWith('.json'))
    .sort()
    .map((f) => `${name}/${bucket}/${f}`);
}

describe.each(SCHEMA_IDS)('%s fixtures', (id) => {
  const valid = fixturesFor(id, 'valid');
  const invalid = fixturesFor(id, 'invalid');

  it('has at least three valid and three invalid documents', () => {
    expect(valid.length).toBeGreaterThanOrEqual(3);
    expect(invalid.length).toBeGreaterThanOrEqual(3);
  });

  it.each(valid)('%s validates', (key) => {
    const result = validateJson(id, readFileSync(join(FIXTURES, key), 'utf8'));
    expect(result.errors).toEqual([]);
    expect(result.valid).toBe(true);
  });

  it.each(invalid)('%s is rejected, and the error names the field', (key) => {
    const result = validateJson(id, readFileSync(join(FIXTURES, key), 'utf8'));
    expect(result.valid).toBe(false);
    expect(result.errors.length).toBeGreaterThan(0);
    for (const error of result.errors) {
      // An error nobody can act on is not an error report. Every one of them
      // carries the pointer of the field that failed and the keyword that
      // failed it.
      expect(error.instancePath).toMatch(/^(\/.*)?$/);
      expect(error.keyword).not.toBe('');
      expect(error.message).not.toBe('');
    }
  });
});

describe('expectations.json', () => {
  it('covers every fixture on disk', () => {
    const onDisk = SCHEMA_IDS.flatMap((id) => [
      ...fixturesFor(id, 'valid'),
      ...fixturesFor(id, 'invalid'),
    ]).sort();
    expect(Object.keys(expectations).sort()).toEqual(onDisk);
  });

  it('matches what this validator produces, field for field', () => {
    // The Go suite asserts the same file. That is what makes "the two
    // validators agree" a test rather than an intention.
    for (const [key, expected] of Object.entries(expectations)) {
      const result = validateJson(expected.schemaId, readFileSync(join(FIXTURES, key), 'utf8'));
      expect(
        {
          key,
          valid: result.valid,
          errors: result.errors.map((e) => ({
            instancePath: e.instancePath,
            keyword: e.keyword,
            message: e.message,
          })),
        },
        key,
      ).toEqual({ key, valid: expected.valid, errors: expected.errors });
    }
  });
});
