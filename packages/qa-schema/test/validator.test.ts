import { describe, expect, it } from 'vitest';
import { SchemaError, assertSupported, validateWith } from '../src/validator.js';

const URI = 'https://qa.local/schema/test/1';

function check(schema: unknown, instance: unknown, extra: Record<string, unknown> = {}) {
  const documents: Record<string, unknown> = { [URI]: schema, ...extra };
  return validateWith(schema, URI, instance, (uri) => documents[uri]);
}

describe('assertSupported', () => {
  it('refuses a keyword it does not implement instead of ignoring it', () => {
    // This is the whole point of hand-rolling the validator: a library would
    // accept `unevaluatedProperties`, ignore it, and quietly stop enforcing it.
    expect(() =>
      assertSupported({ type: 'object', unevaluatedProperties: false }, URI),
    ).toThrowError(/unsupported schema keyword "unevaluatedProperties"/);
  });

  it('allows x- extensions, which are annotations for the generator', () => {
    expect(() => assertSupported({ type: 'string', 'x-go-type': 'string' }, URI)).not.toThrow();
  });

  it('refuses a regex Go cannot compile', () => {
    expect(() => assertSupported({ type: 'string', pattern: '^(?=.*a)' }, URI)).toThrowError(
      /RE2 cannot compile/,
    );
    expect(() => assertSupported({ type: 'string', pattern: '(a)\\1' }, URI)).toThrowError(
      /RE2 cannot compile/,
    );
  });

  it('refuses a format it has no definition for', () => {
    expect(() => assertSupported({ type: 'string', format: 'ipv6' }, URI)).toThrowError(
      /unsupported format "ipv6"/,
    );
  });

  it('walks into applicators', () => {
    expect(() =>
      assertSupported({ allOf: [{ properties: { a: { contains: {} } } }] }, URI),
    ).toThrowError(/unsupported schema keyword "contains"/);
  });
});

describe('type', () => {
  it('treats a whole-numbered float as an integer, the way the spec does', () => {
    expect(check({ type: 'integer' }, 1).valid).toBe(true);
    expect(check({ type: 'integer' }, 1.5).valid).toBe(false);
    expect(check({ type: 'number' }, 1).valid).toBe(true);
  });

  it('stops at the type gate so one wrong field is one error', () => {
    const result = check({ type: 'string', minLength: 5, pattern: '^a' }, 42);
    expect(result.errors).toEqual([
      {
        instancePath: '',
        schemaPath: `${URI}/type`,
        keyword: 'type',
        message: 'expected string, got number',
      },
    ]);
  });

  it('accepts a nullable union', () => {
    expect(check({ type: ['integer', 'null'] }, null).valid).toBe(true);
    expect(check({ type: ['integer', 'null'] }, 3).valid).toBe(true);
    expect(check({ type: ['integer', 'null'] }, 'x').valid).toBe(false);
  });
});

describe('objects', () => {
  const schema = {
    type: 'object',
    additionalProperties: false,
    required: ['a'],
    properties: { a: { type: 'string' }, b: { type: 'integer' } },
  };

  it('names the missing property and the stray one', () => {
    const result = check(schema, { b: 1, c: true });
    expect(result.errors.map((e) => [e.instancePath, e.keyword])).toEqual([
      ['', 'required'],
      ['/c', 'additionalProperties'],
    ]);
  });

  it('reports errors in a stable order regardless of key order', () => {
    const first = check(schema, { z: 1, c: 2, b: 'no' });
    const second = check(schema, { b: 'no', c: 2, z: 1 });
    expect(first.errors).toEqual(second.errors);
    expect(first.errors.map((e) => e.instancePath)).toEqual(['', '/b', '/c', '/z']);
  });

  it('escapes a JSON Pointer token', () => {
    const result = check({ type: 'object', additionalProperties: false }, { 'a/b~c': 1 });
    expect(result.errors[0]?.instancePath).toBe('/a~1b~0c');
  });
});

describe('applicators', () => {
  it('summarises each oneOf branch so the failure still names a field', () => {
    const schema = {
      oneOf: [
        { type: 'object', required: ['ref'], additionalProperties: false, properties: { ref: { type: 'string' } } },
        {
          type: 'object',
          required: ['locator', 'unstable'],
          additionalProperties: false,
          properties: { locator: { type: 'string' }, unstable: { const: true } },
        },
      ],
    };
    const result = check(schema, { locator: '#a' });
    expect(result.valid).toBe(false);
    expect(result.errors[0]?.message).toContain('missing required property "unstable"');
  });

  it('complains when more than one oneOf branch matches', () => {
    const result = check({ oneOf: [{ type: 'string' }, { minLength: 0 }] }, 'x');
    expect(result.errors[0]?.message).toMatch(/matches 2 of the 2 allowed shapes/);
  });

  it('applies then only when if holds', () => {
    const schema = {
      type: 'object',
      properties: { kind: { type: 'string' } },
      if: { required: ['kind'], properties: { kind: { const: 'role' } } },
      then: { required: ['kind', 'name'] },
    };
    expect(check(schema, { kind: 'css' }).valid).toBe(true);
    expect(check(schema, { kind: 'role' }).valid).toBe(false);
    expect(check(schema, { kind: 'role', name: 'Save' }).valid).toBe(true);
  });

  it('resolves a $ref into another document', () => {
    const other = 'https://qa.local/schema/other/1';
    const result = check(
      { $ref: `${other}#/$defs/Positive` },
      -1,
      { [other]: { $defs: { Positive: { type: 'integer', minimum: 0 } } } },
    );
    expect(result.errors[0]?.keyword).toBe('minimum');
    expect(result.errors[0]?.schemaPath).toBe(`${other}#/$defs/Positive/minimum`);
  });

  it('survives a self-referential schema', () => {
    const schema = {
      $defs: {
        Node: {
          type: 'object',
          required: ['value'],
          properties: { value: { type: 'integer' }, next: { $ref: '#/$defs/Node' } },
        },
      },
      $ref: '#/$defs/Node',
    };
    expect(check(schema, { value: 1, next: { value: 2 } }).valid).toBe(true);
    expect(check(schema, { value: 1, next: { value: 'two' } }).valid).toBe(false);
  });

  it('raises a schema error, not a validation error, for an unresolvable $ref', () => {
    expect(() => check({ $ref: 'https://qa.local/schema/missing/1' }, {})).toThrow(SchemaError);
  });
});

describe('arrays and strings', () => {
  it('points uniqueItems at the duplicate, not at the array', () => {
    const result = check({ type: 'array', uniqueItems: true }, ['a', 'b', 'a']);
    expect(result.errors).toEqual([
      {
        instancePath: '/2',
        schemaPath: `${URI}/uniqueItems`,
        keyword: 'uniqueItems',
        message: 'duplicates the item at index 0',
      },
    ]);
  });

  it('counts string length in code points', () => {
    expect(check({ type: 'string', maxLength: 2 }, 'ab').valid).toBe(true);
    expect(check({ type: 'string', maxLength: 2 }, 'abc').valid).toBe(false);
  });

  it('rejects a non-portable regex supplied as data', () => {
    const schema = { type: 'string', format: 'regex' };
    expect(check(schema, '^/employees/[0-9]+$').valid).toBe(true);
    expect(check(schema, '^(?=.*x)').valid).toBe(false);
    expect(check(schema, '[').valid).toBe(false);
  });
});
