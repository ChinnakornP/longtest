/**
 * `qa-schema` — the validator as a command, so CI and the daemon's shell-level
 * plumbing can gate on a contract without linking either language binding.
 *
 * Exit codes are the interface:
 *   0  the document is valid
 *   1  the document is invalid (every failing field is printed)
 *   2  the command itself was wrong (bad usage, unreadable file, broken schema)
 */
import { readFileSync } from 'node:fs';
import { SCHEMA_IDS, UnknownSchemaError, validateJson } from './registry.js';
import { SchemaError, type ValidationError } from './validator.js';

const USAGE = `usage: qa-schema <command> [options]

commands:
  validate <schema-id> <file>...   validate each file against a contract
                                   ("-" reads stdin)
  list                             print the known contract ids

options:
  --json                           machine-readable report on stdout
  -h, --help                       this text

schema ids:
${SCHEMA_IDS.map((id) => `  ${id}`).join('\n')}
`;

interface FileReport {
  file: string;
  schemaId: string;
  valid: boolean;
  errors: ValidationError[];
}

function readInput(file: string): string {
  return readFileSync(file === '-' ? 0 : file, 'utf8');
}

function humanReport(report: FileReport): string {
  if (report.valid) return `ok   ${report.file}  (${report.schemaId})`;
  const lines = [`FAIL ${report.file}  (${report.schemaId})`];
  for (const error of report.errors) {
    lines.push(`  ${error.instancePath || '/'}: ${error.message}  [${error.keyword}]`);
  }
  return lines.join('\n');
}

export function run(argv: string[]): number {
  const asJson = argv.includes('--json');
  const args = argv.filter((a) => a !== '--json');

  if (args.length === 0 || args[0] === '-h' || args[0] === '--help') {
    process.stdout.write(USAGE);
    return args.length === 0 ? 2 : 0;
  }

  const [command, ...rest] = args;

  if (command === 'list') {
    process.stdout.write(asJson ? `${JSON.stringify(SCHEMA_IDS, null, 2)}\n` : `${SCHEMA_IDS.join('\n')}\n`);
    return 0;
  }

  if (command !== 'validate') {
    process.stderr.write(`qa-schema: unknown command "${command}"\n\n${USAGE}`);
    return 2;
  }

  const [schemaId, ...files] = rest;
  if (schemaId === undefined || files.length === 0) {
    process.stderr.write(`qa-schema: validate needs a schema id and at least one file\n\n${USAGE}`);
    return 2;
  }

  const reports: FileReport[] = [];
  for (const file of files) {
    let text: string;
    try {
      text = readInput(file);
    } catch (cause) {
      process.stderr.write(
        `qa-schema: cannot read ${file}: ${cause instanceof Error ? cause.message : String(cause)}\n`,
      );
      return 2;
    }
    try {
      const result = validateJson(schemaId, text);
      reports.push({ file, schemaId, valid: result.valid, errors: result.errors });
    } catch (cause) {
      if (cause instanceof UnknownSchemaError || cause instanceof SchemaError) {
        process.stderr.write(`qa-schema: ${cause.message}\n`);
        return 2;
      }
      throw cause;
    }
  }

  if (asJson) {
    process.stdout.write(`${JSON.stringify({ reports }, null, 2)}\n`);
  } else {
    process.stdout.write(`${reports.map(humanReport).join('\n')}\n`);
  }
  return reports.every((r) => r.valid) ? 0 : 1;
}
