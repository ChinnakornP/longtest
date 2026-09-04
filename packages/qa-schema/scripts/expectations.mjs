#!/usr/bin/env node
/**
 * Rebuilds fixtures/expectations.json — the oracle both validators are held to.
 *
 * It is deliberately NOT part of `make gen`. The Go and the TypeScript test
 * suites both assert their output against this file, so it only means something
 * while it is a record of a reviewed decision. Regenerating it on every build
 * would turn a behaviour regression into a silent diff.
 *
 * Run it on purpose, read the diff, then commit:
 *
 *     pnpm --filter @qa/schema run gen:expectations
 */
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { SCHEMA_IDS, validateJson } from '../dist/index.js';

const PKG = join(dirname(fileURLToPath(import.meta.url)), '..');
const FIXTURES = join(PKG, 'fixtures');

const expectations = {};

for (const id of SCHEMA_IDS) {
  const name = id.slice(0, id.lastIndexOf('@'));
  for (const bucket of ['valid', 'invalid']) {
    const dir = join(FIXTURES, name, bucket);
    for (const file of readdirSync(dir).sort()) {
      if (!file.endsWith('.json')) continue;
      const key = `${name}/${bucket}/${file}`;
      const result = validateJson(id, readFileSync(join(dir, file), 'utf8'));
      if (bucket === 'valid' && !result.valid) {
        throw new Error(`${key} is filed under valid/ but does not validate`);
      }
      if (bucket === 'invalid' && result.valid) {
        throw new Error(`${key} is filed under invalid/ but validates cleanly`);
      }
      expectations[key] = {
        schemaId: id,
        valid: result.valid,
        errors: result.errors.map((e) => ({
          instancePath: e.instancePath,
          keyword: e.keyword,
          message: e.message,
        })),
      };
    }
  }
}

writeFileSync(
  join(FIXTURES, 'expectations.json'),
  `${JSON.stringify(expectations, null, 2)}\n`,
  'utf8',
);
process.stdout.write(
  `qa-schema: expectations for ${Object.keys(expectations).length} fixtures written\n`,
);
