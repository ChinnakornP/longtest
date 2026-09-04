#!/usr/bin/env node
/**
 * Generates Go and TypeScript types from the JSON Schemas in ./schemas.
 *
 * Stage-1 placeholder: the schemas themselves and the generator are T1. This
 * script exists so `make gen` is wired from the first commit and fails loudly
 * rather than silently doing nothing once schemas start landing.
 */
import { readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const schemaDir = join(here, '..', 'schemas');

const schemas = readdirSync(schemaDir).filter((f) => f.endsWith('.schema.json'));

if (schemas.length === 0) {
  console.warn('qa-schema: no *.schema.json found yet - contracts A/B/F land in T1');
  process.exit(0);
}

console.error(`qa-schema: ${schemas.length} schema(s) found but no generator is wired yet (T1)`);
process.exit(1);
