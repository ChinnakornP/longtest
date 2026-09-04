import { describe, expect, it } from 'vitest';
import { readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  CONTRACT_VERSIONS,
  SCHEMA_IDS,
  SCHEMA_URIS,
  UnknownSchemaError,
  getSchemaDocument,
  isSchemaId,
  validate,
  validateJson,
} from '../src/index.js';

const PKG = join(dirname(fileURLToPath(import.meta.url)), '..');

describe('schema ids', () => {
  it('covers every contract the stage-2 plan froze', () => {
    expect([...SCHEMA_IDS]).toEqual([
      'application-map@1',
      'daemon-envelope@1',
      'execution-result@1',
      'finding@1',
      'test-case@1',
      'test-plan@1',
    ]);
  });

  it('is generated from the schema directory, so it cannot drift from disk', () => {
    const onDisk = readdirSync(join(PKG, 'schemas'))
      .filter((f) => f.endsWith('.schema.json'))
      .map((f) => f.replace('.schema.json', ''))
      .sort();
    const declared = SCHEMA_IDS.map((id) => id.slice(0, id.lastIndexOf('@'))).sort();
    expect(declared).toEqual(onDisk);
  });

  it('pairs every id with a uri and a contract version in the same major', () => {
    for (const id of SCHEMA_IDS) {
      const [name, major] = id.split('@');
      expect(SCHEMA_URIS[id]).toBe(`https://qa.local/schema/${name}/${major}`);
      expect(CONTRACT_VERSIONS[id].startsWith(`${major}.`)).toBe(true);
    }
  });

  it('rejects an unknown id instead of guessing', () => {
    expect(isSchemaId('test-case@1')).toBe(true);
    expect(isSchemaId('test-case@2')).toBe(false);
    expect(isSchemaId('../../etc/passwd')).toBe(false);
    expect(() => getSchemaDocument('test-case@2')).toThrow(UnknownSchemaError);
    expect(() => validate('nope', {})).toThrow(UnknownSchemaError);
  });

  it('loads every document without tripping the keyword allowlist', () => {
    for (const id of SCHEMA_IDS) {
      expect(getSchemaDocument(id)).toBeTypeOf('object');
    }
  });
});

describe('validateJson', () => {
  it('reports malformed JSON at the document root rather than throwing', () => {
    const result = validateJson('finding@1', '{ nope');
    expect(result.valid).toBe(false);
    expect(result.errors[0]?.keyword).toBe('parse');
    expect(result.errors[0]?.instancePath).toBe('');
  });
});
