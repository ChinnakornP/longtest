/**
 * Contract identifiers shared by every component.
 *
 * The generated types land next to this file (`*.generated.ts`) once T1 wires
 * up `make gen`. The names below are frozen now so the backend, the daemon and
 * the executor can be written against them in parallel.
 */

export const SCHEMA_IDS = [
  'test-case@1',
  'application-map@1',
  'finding@1',
  'daemon-envelope@1',
] as const;

export type SchemaId = (typeof SCHEMA_IDS)[number];

export function isSchemaId(value: string): value is SchemaId {
  return (SCHEMA_IDS as readonly string[]).includes(value);
}
