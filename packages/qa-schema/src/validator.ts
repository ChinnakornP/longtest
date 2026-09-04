/**
 * A JSON Schema (draft 2020-12) validator over the subset of keywords the QA
 * contracts actually use.
 *
 * Why not a library: the Go backend and this TypeScript package have to agree
 * on the *same* verdict and the *same* error paths for the same document — an
 * executor that accepts a frame the backend rejected is a bug nobody can see.
 * Two independent third-party validators agree on validity but not on error
 * output, and both of them ignore keywords they do not implement. This one
 * refuses to load a schema that uses a keyword it does not implement
 * (`assertSupported`), which is the same fail-closed stance the contracts take
 * on unknown actions and unknown enum members.
 *
 * The Go implementation in `server/pkg/qaschema/validator.go` is a line-for-line
 * port. Any change here has to be mirrored there, and the fixture expectations
 * in `fixtures/expectations.json` are what proves it was.
 */

export interface ValidationError {
  /** JSON Pointer into the validated instance. */
  instancePath: string;
  /** Where in the schema the failing keyword lives, prefixed with the schema URI. */
  schemaPath: string;
  keyword: string;
  message: string;
}

export interface ValidationResult {
  valid: boolean;
  errors: ValidationError[];
}

/** Thrown for a schema this validator cannot honour. Never for invalid data. */
export class SchemaError extends Error {}

type Json = unknown;
type JsonObject = { [key: string]: Json };

const ANNOTATION_KEYWORDS = [
  '$schema',
  '$id',
  '$anchor',
  '$comment',
  'title',
  'description',
  'default',
  'examples',
  'deprecated',
  'readOnly',
  'writeOnly',
];

const APPLICATOR_SCHEMA = new Set([
  'if',
  'then',
  'else',
  'not',
  'items',
  'additionalProperties',
  'propertyNames',
]);
const APPLICATOR_MAP = new Set(['properties', 'patternProperties', '$defs']);
const APPLICATOR_LIST = new Set(['allOf', 'anyOf', 'oneOf']);

const ASSERTION_KEYWORDS = [
  '$ref',
  'type',
  'const',
  'enum',
  'minLength',
  'maxLength',
  'pattern',
  'format',
  'minimum',
  'maximum',
  'exclusiveMinimum',
  'exclusiveMaximum',
  'multipleOf',
  'minItems',
  'maxItems',
  'uniqueItems',
  'required',
  'dependentRequired',
  'minProperties',
  'maxProperties',
];

/**
 * Regex constructs that JavaScript accepts and Go's RE2 does not. A pattern is
 * useless to us if it only works on one side of the wire, so it is rejected
 * when the schema loads rather than behaving differently per language.
 */
const NON_PORTABLE_REGEX = /\(\?=|\(\?!|\(\?<|\\[1-9]/;

const FORMAT_PATTERNS: Record<string, RegExp> = {
  uuid: /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/,
  'date-time':
    /^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?([Zz]|[+-][0-9]{2}:[0-9]{2})$/,
  uri: /^[A-Za-z][A-Za-z0-9+.-]*:[^\s]+$/,
};

const KNOWN_KEYWORDS = new Set<string>([
  ...ANNOTATION_KEYWORDS,
  ...APPLICATOR_SCHEMA,
  ...APPLICATOR_MAP,
  ...APPLICATOR_LIST,
  ...ASSERTION_KEYWORDS,
]);

function isObject(value: Json): value is JsonObject {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** The JSON Schema type name of a runtime value. `integer` is not returned; see `matchesType`. */
export function jsonType(value: Json): string {
  if (value === null) return 'null';
  if (Array.isArray(value)) return 'array';
  switch (typeof value) {
    case 'string':
      return 'string';
    case 'boolean':
      return 'boolean';
    case 'number':
      return 'number';
    case 'object':
      return 'object';
    default:
      return 'unknown';
  }
}

function matchesType(value: Json, want: string): boolean {
  if (want === 'integer') {
    return typeof value === 'number' && Number.isFinite(value) && Math.trunc(value) === value;
  }
  return jsonType(value) === want;
}

function escapePointer(token: string): string {
  return token.replace(/~/g, '~0').replace(/\//g, '~1');
}

function unescapePointer(token: string): string {
  return token.replace(/~1/g, '/').replace(/~0/g, '~');
}

/** Stable rendering used by `uniqueItems`, `const` and `enum` comparison. */
function canonical(value: Json): string {
  if (value === null) return 'null';
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`;
  if (isObject(value)) {
    const keys = Object.keys(value).sort();
    return `{${keys.map((k) => `${JSON.stringify(k)}:${canonical(value[k])}`).join(',')}}`;
  }
  if (typeof value === 'number') return Number.isFinite(value) ? String(value) : 'null';
  return JSON.stringify(value) ?? 'null';
}

function deepEqual(a: Json, b: Json): boolean {
  return canonical(a) === canonical(b);
}

function quote(value: Json): string {
  if (typeof value === 'string') return JSON.stringify(value);
  return canonical(value);
}

function assertPortableRegex(pattern: Json, where: string): void {
  if (typeof pattern !== 'string') {
    throw new SchemaError(`${where}: pattern must be a string`);
  }
  if (NON_PORTABLE_REGEX.test(pattern)) {
    throw new SchemaError(
      `${where}: pattern uses a construct Go's RE2 cannot compile (lookaround or backreference)`,
    );
  }
  try {
    new RegExp(pattern, 'u');
  } catch {
    throw new SchemaError(`${where}: pattern does not compile`);
  }
}

/**
 * Walks a schema document and rejects any keyword this validator does not
 * implement. Unknown `x-` extensions are annotations and are allowed through.
 *
 * This is the guard that stops a schema author from adding, say, `oneOf` under
 * a keyword the Go port does not walk: the schema fails to load on both sides
 * instead of quietly validating nothing on one of them.
 */
export function assertSupported(document: Json, uri: string): void {
  const walk = (node: Json, path: string): void => {
    if (typeof node === 'boolean') return;
    if (!isObject(node)) {
      throw new SchemaError(
        `${uri}${path}: expected a schema object or boolean, got ${jsonType(node)}`,
      );
    }
    for (const key of Object.keys(node).sort()) {
      if (key.startsWith('x-')) continue;
      if (!KNOWN_KEYWORDS.has(key)) {
        throw new SchemaError(`${uri}${path}: unsupported schema keyword "${key}"`);
      }
      const value = node[key];
      if (APPLICATOR_SCHEMA.has(key)) {
        walk(value, `${path}/${key}`);
      } else if (APPLICATOR_MAP.has(key)) {
        if (!isObject(value)) throw new SchemaError(`${uri}${path}/${key}: expected an object`);
        for (const name of Object.keys(value).sort()) {
          walk(value[name], `${path}/${key}/${escapePointer(name)}`);
        }
      } else if (APPLICATOR_LIST.has(key)) {
        if (!Array.isArray(value)) throw new SchemaError(`${uri}${path}/${key}: expected an array`);
        value.forEach((item, i) => walk(item, `${path}/${key}/${i}`));
      } else if (key === 'pattern') {
        assertPortableRegex(value, `${uri}${path}/pattern`);
      } else if (key === 'format') {
        if (typeof value !== 'string' || (value !== 'regex' && FORMAT_PATTERNS[value] === undefined)) {
          throw new SchemaError(`${uri}${path}/format: unsupported format "${String(value)}"`);
        }
      }
    }
  };
  walk(document, '');
}

/** Resolves a schema URI to the document that declares it. */
export type Resolver = (uri: string) => Json | undefined;

interface Ctx {
  resolve: Resolver;
  /** Guards against a `$ref` cycle that never reaches an assertion. */
  seen: Set<string>;
}

function pointerInto(document: Json, fragment: string, uri: string): Json {
  if (fragment === '') return document;
  let node: Json = document;
  for (const rawToken of fragment.split('/').slice(1)) {
    const token = unescapePointer(rawToken);
    if (isObject(node)) {
      if (!(token in node)) throw new SchemaError(`${uri}: cannot resolve pointer ${fragment}`);
      node = node[token];
    } else if (Array.isArray(node)) {
      const idx = Number(token);
      if (!Number.isInteger(idx) || idx < 0 || idx >= node.length) {
        throw new SchemaError(`${uri}: cannot resolve pointer ${fragment}`);
      }
      node = node[idx];
    } else {
      throw new SchemaError(`${uri}: cannot resolve pointer ${fragment}`);
    }
  }
  return node;
}

function err(
  instancePath: string,
  schemaPath: string,
  keyword: string,
  message: string,
): ValidationError {
  return { instancePath, schemaPath, keyword, message };
}

function validateNode(
  schema: Json,
  instance: Json,
  instancePath: string,
  schemaPath: string,
  ctx: Ctx,
): ValidationError[] {
  if (schema === true) return [];
  if (schema === false) {
    return [err(instancePath, schemaPath, 'false', 'no value is accepted here')];
  }
  if (!isObject(schema)) {
    throw new SchemaError(`${schemaPath}: expected a schema object or boolean`);
  }

  const errors: ValidationError[] = [];

  // $ref first. A sibling keyword still applies in 2020-12, so this is not a jump.
  const ref = schema['$ref'];
  if (typeof ref === 'string') {
    const hash = ref.indexOf('#');
    const base = hash === -1 ? ref : ref.slice(0, hash);
    const fragment = hash === -1 ? '' : ref.slice(hash + 1);
    const currentUri = schemaPath.split('#')[0] ?? '';
    const targetUri = base === '' ? currentUri : base;
    const doc = ctx.resolve(targetUri);
    if (doc === undefined) {
      throw new SchemaError(`${schemaPath}/$ref: unknown schema "${targetUri}"`);
    }
    const target = pointerInto(doc, fragment, targetUri);
    const nextPath = `${targetUri}#${fragment}`;
    const cycleKey = `${nextPath} ${instancePath}`;
    if (!ctx.seen.has(cycleKey)) {
      ctx.seen.add(cycleKey);
      try {
        errors.push(...validateNode(target, instance, instancePath, nextPath, ctx));
      } finally {
        ctx.seen.delete(cycleKey);
      }
    }
  }

  // type is a gate: once it fails, every type-specific keyword below would only
  // repeat the same complaint with a different noun.
  if ('type' in schema) {
    const want = schema['type'];
    const wanted = Array.isArray(want) ? want : [want];
    const ok = wanted.some((t) => typeof t === 'string' && matchesType(instance, t));
    if (!ok) {
      const names = wanted.map((t) => String(t)).join(', ');
      errors.push(
        err(
          instancePath,
          `${schemaPath}/type`,
          'type',
          `expected ${names}, got ${jsonType(instance)}`,
        ),
      );
      return errors;
    }
  }

  if ('const' in schema && !deepEqual(instance, schema['const'])) {
    errors.push(
      err(
        instancePath,
        `${schemaPath}/const`,
        'const',
        `expected ${quote(schema['const'])}, got ${quote(instance)}`,
      ),
    );
  }

  const enumValues = schema['enum'];
  if (Array.isArray(enumValues) && !enumValues.some((candidate) => deepEqual(instance, candidate))) {
    errors.push(
      err(
        instancePath,
        `${schemaPath}/enum`,
        'enum',
        `${quote(instance)} is not one of: ${enumValues.map(quote).join(', ')}`,
      ),
    );
  }

  if (typeof instance === 'string') {
    errors.push(...validateString(schema, instance, instancePath, schemaPath));
  }
  if (typeof instance === 'number') {
    errors.push(...validateNumber(schema, instance, instancePath, schemaPath));
  }
  if (Array.isArray(instance)) {
    errors.push(...validateArray(schema, instance, instancePath, schemaPath, ctx));
  }
  if (isObject(instance)) {
    errors.push(...validateObject(schema, instance, instancePath, schemaPath, ctx));
  }

  errors.push(...validateApplicators(schema, instance, instancePath, schemaPath, ctx));
  return errors;
}

function validateString(
  schema: JsonObject,
  value: string,
  instancePath: string,
  schemaPath: string,
): ValidationError[] {
  const errors: ValidationError[] = [];
  const length = [...value].length;
  const minLength = schema['minLength'];
  if (typeof minLength === 'number' && length < minLength) {
    errors.push(
      err(
        instancePath,
        `${schemaPath}/minLength`,
        'minLength',
        `length ${length} is below the minimum of ${minLength}`,
      ),
    );
  }
  const maxLength = schema['maxLength'];
  if (typeof maxLength === 'number' && length > maxLength) {
    errors.push(
      err(
        instancePath,
        `${schemaPath}/maxLength`,
        'maxLength',
        `length ${length} exceeds the maximum of ${maxLength}`,
      ),
    );
  }
  const pattern = schema['pattern'];
  if (typeof pattern === 'string' && !new RegExp(pattern, 'u').test(value)) {
    errors.push(
      err(
        instancePath,
        `${schemaPath}/pattern`,
        'pattern',
        `${JSON.stringify(value)} does not match ${JSON.stringify(pattern)}`,
      ),
    );
  }
  const format = schema['format'];
  if (typeof format === 'string') {
    if (format === 'regex') {
      let ok = !NON_PORTABLE_REGEX.test(value);
      if (ok) {
        try {
          new RegExp(value, 'u');
        } catch {
          ok = false;
        }
      }
      if (!ok) {
        errors.push(
          err(
            instancePath,
            `${schemaPath}/format`,
            'format',
            `${JSON.stringify(value)} is not a portable regular expression`,
          ),
        );
      }
    } else {
      const re = FORMAT_PATTERNS[format];
      if (re === undefined) {
        throw new SchemaError(`${schemaPath}/format: unsupported format "${format}"`);
      }
      if (!re.test(value)) {
        errors.push(
          err(
            instancePath,
            `${schemaPath}/format`,
            'format',
            `${JSON.stringify(value)} is not a valid ${format}`,
          ),
        );
      }
    }
  }
  return errors;
}

function validateNumber(
  schema: JsonObject,
  value: number,
  instancePath: string,
  schemaPath: string,
): ValidationError[] {
  const errors: ValidationError[] = [];
  const bound = (
    keyword: string,
    fails: (limit: number) => boolean,
    text: (limit: number) => string,
  ): void => {
    const limit = schema[keyword];
    if (typeof limit === 'number' && fails(limit)) {
      errors.push(err(instancePath, `${schemaPath}/${keyword}`, keyword, text(limit)));
    }
  };
  bound(
    'minimum',
    (l) => value < l,
    (l) => `${value} is below the minimum of ${l}`,
  );
  bound(
    'maximum',
    (l) => value > l,
    (l) => `${value} exceeds the maximum of ${l}`,
  );
  bound(
    'exclusiveMinimum',
    (l) => value <= l,
    (l) => `${value} must be greater than ${l}`,
  );
  bound(
    'exclusiveMaximum',
    (l) => value >= l,
    (l) => `${value} must be less than ${l}`,
  );
  const multipleOf = schema['multipleOf'];
  if (typeof multipleOf === 'number' && multipleOf > 0) {
    const ratio = value / multipleOf;
    if (Math.abs(ratio - Math.round(ratio)) > 1e-9) {
      errors.push(
        err(
          instancePath,
          `${schemaPath}/multipleOf`,
          'multipleOf',
          `${value} is not a multiple of ${multipleOf}`,
        ),
      );
    }
  }
  return errors;
}

function validateArray(
  schema: JsonObject,
  value: Json[],
  instancePath: string,
  schemaPath: string,
  ctx: Ctx,
): ValidationError[] {
  const errors: ValidationError[] = [];
  const minItems = schema['minItems'];
  if (typeof minItems === 'number' && value.length < minItems) {
    errors.push(
      err(
        instancePath,
        `${schemaPath}/minItems`,
        'minItems',
        `${value.length} items is below the minimum of ${minItems}`,
      ),
    );
  }
  const maxItems = schema['maxItems'];
  if (typeof maxItems === 'number' && value.length > maxItems) {
    errors.push(
      err(
        instancePath,
        `${schemaPath}/maxItems`,
        'maxItems',
        `${value.length} items exceeds the maximum of ${maxItems}`,
      ),
    );
  }
  if (schema['uniqueItems'] === true) {
    const seen = new Map<string, number>();
    for (let i = 0; i < value.length; i += 1) {
      const key = canonical(value[i]);
      const first = seen.get(key);
      if (first === undefined) {
        seen.set(key, i);
      } else {
        errors.push(
          err(
            `${instancePath}/${i}`,
            `${schemaPath}/uniqueItems`,
            'uniqueItems',
            `duplicates the item at index ${first}`,
          ),
        );
      }
    }
  }
  if ('items' in schema) {
    for (let i = 0; i < value.length; i += 1) {
      errors.push(
        ...validateNode(
          schema['items'],
          value[i],
          `${instancePath}/${i}`,
          `${schemaPath}/items`,
          ctx,
        ),
      );
    }
  }
  return errors;
}

function validateObject(
  schema: JsonObject,
  value: JsonObject,
  instancePath: string,
  schemaPath: string,
  ctx: Ctx,
): ValidationError[] {
  const errors: ValidationError[] = [];
  const present = Object.keys(value).sort();

  const required = schema['required'];
  if (Array.isArray(required)) {
    for (const name of required) {
      if (typeof name === 'string' && !(name in value)) {
        errors.push(
          err(
            instancePath,
            `${schemaPath}/required`,
            'required',
            `missing required property "${name}"`,
          ),
        );
      }
    }
  }

  const dependents = schema['dependentRequired'];
  if (isObject(dependents)) {
    for (const trigger of Object.keys(dependents).sort()) {
      if (!(trigger in value)) continue;
      const needed = dependents[trigger];
      if (!Array.isArray(needed)) continue;
      for (const name of needed) {
        if (typeof name === 'string' && !(name in value)) {
          errors.push(
            err(
              instancePath,
              `${schemaPath}/dependentRequired/${escapePointer(trigger)}`,
              'dependentRequired',
              `property "${name}" is required when "${trigger}" is present`,
            ),
          );
        }
      }
    }
  }

  const minProperties = schema['minProperties'];
  if (typeof minProperties === 'number' && present.length < minProperties) {
    errors.push(
      err(
        instancePath,
        `${schemaPath}/minProperties`,
        'minProperties',
        `${present.length} properties is below the minimum of ${minProperties}`,
      ),
    );
  }
  const maxProperties = schema['maxProperties'];
  if (typeof maxProperties === 'number' && present.length > maxProperties) {
    errors.push(
      err(
        instancePath,
        `${schemaPath}/maxProperties`,
        'maxProperties',
        `${present.length} properties exceeds the maximum of ${maxProperties}`,
      ),
    );
  }

  const properties = isObject(schema['properties']) ? schema['properties'] : undefined;
  const patternProperties = isObject(schema['patternProperties'])
    ? schema['patternProperties']
    : undefined;

  if ('propertyNames' in schema) {
    for (const name of present) {
      errors.push(
        ...validateNode(
          schema['propertyNames'],
          name,
          `${instancePath}/${escapePointer(name)}`,
          `${schemaPath}/propertyNames`,
          ctx,
        ),
      );
    }
  }

  for (const name of present) {
    let matched = false;
    if (properties !== undefined && name in properties) {
      matched = true;
      errors.push(
        ...validateNode(
          properties[name],
          value[name],
          `${instancePath}/${escapePointer(name)}`,
          `${schemaPath}/properties/${escapePointer(name)}`,
          ctx,
        ),
      );
    }
    if (patternProperties !== undefined) {
      for (const pattern of Object.keys(patternProperties).sort()) {
        if (new RegExp(pattern, 'u').test(name)) {
          matched = true;
          errors.push(
            ...validateNode(
              patternProperties[pattern],
              value[name],
              `${instancePath}/${escapePointer(name)}`,
              `${schemaPath}/patternProperties/${escapePointer(pattern)}`,
              ctx,
            ),
          );
        }
      }
    }
    if (!matched && 'additionalProperties' in schema) {
      const additional = schema['additionalProperties'];
      if (additional === false) {
        errors.push(
          err(
            `${instancePath}/${escapePointer(name)}`,
            `${schemaPath}/additionalProperties`,
            'additionalProperties',
            `property "${name}" is not allowed here`,
          ),
        );
      } else {
        errors.push(
          ...validateNode(
            additional,
            value[name],
            `${instancePath}/${escapePointer(name)}`,
            `${schemaPath}/additionalProperties`,
            ctx,
          ),
        );
      }
    }
  }
  return errors;
}

function validateApplicators(
  schema: JsonObject,
  instance: Json,
  instancePath: string,
  schemaPath: string,
  ctx: Ctx,
): ValidationError[] {
  const errors: ValidationError[] = [];

  const allOf = schema['allOf'];
  if (Array.isArray(allOf)) {
    allOf.forEach((sub, i) => {
      errors.push(...validateNode(sub, instance, instancePath, `${schemaPath}/allOf/${i}`, ctx));
    });
  }

  const anyOf = schema['anyOf'];
  if (Array.isArray(anyOf)) {
    const failures = anyOf.map((sub, i) =>
      validateNode(sub, instance, instancePath, `${schemaPath}/anyOf/${i}`, ctx),
    );
    if (!failures.some((f) => f.length === 0)) {
      errors.push(
        err(
          instancePath,
          `${schemaPath}/anyOf`,
          'anyOf',
          `does not match any of the ${anyOf.length} allowed shapes: ${summarise(failures)}`,
        ),
      );
    }
  }

  const oneOf = schema['oneOf'];
  if (Array.isArray(oneOf)) {
    const failures = oneOf.map((sub, i) =>
      validateNode(sub, instance, instancePath, `${schemaPath}/oneOf/${i}`, ctx),
    );
    const matched = failures.filter((f) => f.length === 0).length;
    if (matched === 0) {
      errors.push(
        err(
          instancePath,
          `${schemaPath}/oneOf`,
          'oneOf',
          `does not match any of the ${oneOf.length} allowed shapes: ${summarise(failures)}`,
        ),
      );
    } else if (matched > 1) {
      errors.push(
        err(
          instancePath,
          `${schemaPath}/oneOf`,
          'oneOf',
          `matches ${matched} of the ${oneOf.length} allowed shapes, expected exactly one`,
        ),
      );
    }
  }

  if ('not' in schema) {
    if (validateNode(schema['not'], instance, instancePath, `${schemaPath}/not`, ctx).length === 0) {
      errors.push(
        err(instancePath, `${schemaPath}/not`, 'not', 'matches a shape that is not allowed here'),
      );
    }
  }

  if ('if' in schema) {
    const conditionHolds =
      validateNode(schema['if'], instance, instancePath, `${schemaPath}/if`, ctx).length === 0;
    const branch = conditionHolds ? 'then' : 'else';
    if (branch in schema) {
      errors.push(
        ...validateNode(schema[branch], instance, instancePath, `${schemaPath}/${branch}`, ctx),
      );
    }
  }

  return errors;
}

/** One line per branch, so a oneOf failure still names the field that was wrong. */
function summarise(failures: ValidationError[][]): string {
  return failures
    .map((branch, i) => {
      const first = branch[0];
      const detail = first === undefined ? 'ok' : `${first.instancePath || '/'}: ${first.message}`;
      return `[${i}] ${detail}`;
    })
    .join('; ');
}

/** Validates `instance` against `schema`, resolving `$ref` through `resolve`. */
export function validateWith(
  schema: Json,
  schemaUri: string,
  instance: Json,
  resolve: Resolver,
): ValidationResult {
  const errors = validateNode(schema, instance, '', schemaUri, { resolve, seen: new Set() });
  return { valid: errors.length === 0, errors };
}
