/**
 * The schema registry: the only supported way to reach a contract document.
 *
 * `SCHEMA_IDS` is generated from the files in `schemas/`, so a new contract
 * cannot be added without appearing here, and one listed here cannot be missing
 * from disk.
 */
import {
  CONTRACT_VERSIONS,
  SCHEMA_DOCUMENTS,
  SCHEMA_IDS,
  SCHEMA_URIS,
  type SchemaId,
} from './schemas.generated.js';
import {
  SchemaError,
  assertSupported,
  validateWith,
  type Resolver,
  type ValidationResult,
} from './validator.js';

export { SCHEMA_IDS, CONTRACT_VERSIONS, SCHEMA_URIS };
export type { SchemaId };

/** Thrown when a caller names a contract that does not exist. */
export class UnknownSchemaError extends Error {
  constructor(id: string) {
    super(`unknown schema "${id}"; known ids: ${SCHEMA_IDS.join(', ')}`);
    this.name = 'UnknownSchemaError';
  }
}

export function isSchemaId(value: string): value is SchemaId {
  return (SCHEMA_IDS as readonly string[]).includes(value);
}

const URI_TO_ID = new Map<string, SchemaId>(
  SCHEMA_IDS.map((id) => [SCHEMA_URIS[id], id] as const),
);

const checked = new Set<SchemaId>();

/** Loads a contract document, checking once that it only uses supported keywords. */
export function getSchemaDocument(id: string): unknown {
  if (!isSchemaId(id)) throw new UnknownSchemaError(id);
  if (!checked.has(id)) {
    assertSupported(SCHEMA_DOCUMENTS[id], SCHEMA_URIS[id]);
    checked.add(id);
  }
  return SCHEMA_DOCUMENTS[id];
}

const resolve: Resolver = (uri) => {
  const id = URI_TO_ID.get(uri);
  return id === undefined ? undefined : getSchemaDocument(id);
};

/**
 * Validates `instance` against a contract.
 *
 * Throws `UnknownSchemaError` for an unknown id and `SchemaError` for a broken
 * schema. Invalid *data* is never an exception: it comes back as `errors`, each
 * one carrying the JSON Pointer of the field that failed.
 */
export function validate(id: string, instance: unknown): ValidationResult {
  const document = getSchemaDocument(id);
  return validateWith(document, SCHEMA_URIS[id as SchemaId], instance, resolve);
}

/** Parses JSON text and validates it. A syntax error is reported at the document root. */
export function validateJson(id: string, text: string): ValidationResult {
  if (!isSchemaId(id)) throw new UnknownSchemaError(id);
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch (cause) {
    return {
      valid: false,
      errors: [
        {
          instancePath: '',
          schemaPath: SCHEMA_URIS[id],
          keyword: 'parse',
          message: `not valid JSON: ${cause instanceof Error ? cause.message : String(cause)}`,
        },
      ],
    };
  }
  return validate(id, parsed);
}

export { SchemaError };
