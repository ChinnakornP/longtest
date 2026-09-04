import { afterEach, describe, expect, it, vi } from 'vitest';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { run } from '../src/cli.js';

const FIXTURES = join(dirname(fileURLToPath(import.meta.url)), '..', 'fixtures');

function capture(argv: string[]): { code: number; out: string; err: string } {
  let out = '';
  let err = '';
  const stdout = vi.spyOn(process.stdout, 'write').mockImplementation((chunk) => {
    out += String(chunk);
    return true;
  });
  const stderr = vi.spyOn(process.stderr, 'write').mockImplementation((chunk) => {
    err += String(chunk);
    return true;
  });
  try {
    return { code: run(argv), out, err };
  } finally {
    stdout.mockRestore();
    stderr.mockRestore();
  }
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('qa-schema validate', () => {
  // The exit code is the interface: CI gates on it, and so does the daemon's
  // shell plumbing.
  it('exits 0 for a document that validates', () => {
    const result = capture([
      'validate',
      'test-case@1',
      join(FIXTURES, 'test-case/valid/create-employee.json'),
    ]);
    expect(result.code).toBe(0);
    expect(result.out).toContain('ok');
  });

  it('exits 1 and prints the failing field for a document that does not', () => {
    const result = capture([
      'validate',
      'test-case@1',
      join(FIXTURES, 'test-case/invalid/unknown-action.json'),
    ]);
    expect(result.code).toBe(1);
    expect(result.out).toContain('/steps/1/action');
    expect(result.out).toContain('[enum]');
  });

  it('exits 1 if any file in a batch fails', () => {
    const result = capture([
      'validate',
      'finding@1',
      join(FIXTURES, 'finding/valid/product-bug-500.json'),
      join(FIXTURES, 'finding/invalid/no-evidence.json'),
    ]);
    expect(result.code).toBe(1);
    expect(result.out).toContain('ok');
    expect(result.out).toContain('FAIL');
  });

  it('exits 2 for a usage problem, so CI can tell it apart from a bad document', () => {
    expect(capture([]).code).toBe(2);
    expect(capture(['validate', 'test-case@1']).code).toBe(2);
    expect(capture(['lint', 'x']).code).toBe(2);
    const unknownSchema = capture(['validate', 'test-case@9', join(FIXTURES, 'expectations.json')]);
    expect(unknownSchema.code).toBe(2);
    expect(unknownSchema.err).toContain('unknown schema');
    const missingFile = capture(['validate', 'test-case@1', join(FIXTURES, 'nope.json')]);
    expect(missingFile.code).toBe(2);
    expect(missingFile.err).toContain('cannot read');
  });

  it('emits a machine-readable report with --json', () => {
    const result = capture([
      'validate',
      '--json',
      'finding@1',
      join(FIXTURES, 'finding/invalid/confidence-out-of-range.json'),
    ]);
    expect(result.code).toBe(1);
    const parsed = JSON.parse(result.out);
    expect(parsed.reports[0].valid).toBe(false);
    expect(parsed.reports[0].errors[0].instancePath).toBe('/confidence');
  });

  it('lists the contract ids', () => {
    const result = capture(['list']);
    expect(result.code).toBe(0);
    expect(result.out.trim().split('\n')).toContain('daemon-envelope@1');
  });
});
