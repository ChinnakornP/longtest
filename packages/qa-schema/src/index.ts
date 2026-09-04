/**
 * @qa/schema — the versioned wire contracts every component agrees on.
 *
 * Nothing in this repo may re-declare one of these shapes. If a payload crosses
 * a component boundary, its type comes from here and it is validated here.
 */
export {
  CONTRACT_VERSIONS,
  SCHEMA_IDS,
  SCHEMA_URIS,
  SchemaError,
  UnknownSchemaError,
  getSchemaDocument,
  isSchemaId,
  validate,
  validateJson,
  type SchemaId,
} from './registry.js';

export {
  assertSupported,
  jsonType,
  validateWith,
  type Resolver,
  type ValidationError,
  type ValidationResult,
} from './validator.js';

export * from './types.generated.js';
