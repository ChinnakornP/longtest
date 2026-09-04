#!/usr/bin/env node
/**
 * Entry point for the `qa-schema` command. The implementation lives in
 * src/cli.ts and is compiled to dist/ by `make gen`, so this file stays plain
 * JavaScript and needs no loader.
 */
import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const entry = join(here, '..', 'dist', 'cli.js');

if (!existsSync(entry)) {
  process.stderr.write('qa-schema: dist/ is missing - build it first with `make gen`\n');
  process.exit(2);
}

const { run } = await import(pathToFileURL(entry).href);
process.exit(run(process.argv.slice(2)));
