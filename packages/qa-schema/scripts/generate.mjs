#!/usr/bin/env node
/**
 * Generates the language bindings for the contracts in ./schemas.
 *
 * Outputs (all of them are `make gen` artefacts, never hand-edited):
 *   packages/qa-schema/src/schemas.generated.ts   embedded documents + ids
 *   packages/qa-schema/src/types.generated.ts     TypeScript types
 *   packages/qa-schema/dist/                      compiled JS + .d.ts (tsc)
 *   server/pkg/qaschema/                          Go types + embedded schemas
 *   daemon/pkg/qaschema/                          the same package, mirrored
 *
 * The Go validator is authored in server/pkg/qaschema and mirrored into the
 * daemon module by this script: the two modules must not import each other, and
 * two hand-maintained copies would drift within a sprint.
 *
 * Running twice in a row must leave the working tree clean. Everything below is
 * therefore sorted, never stamped with a time, and formatted by gofmt.
 */
import { spawnSync } from 'node:child_process';
import { createRequire } from 'node:module';
import {
  cpSync,
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
const PKG = join(dirname(fileURLToPath(import.meta.url)), '..');
const REPO = join(PKG, '..', '..');
const SCHEMA_DIR = join(PKG, 'schemas');
const GO_PACKAGE = 'qaschema';
const GO_SOURCE = join(REPO, 'server', 'pkg', 'qaschema');
const GO_MIRROR = join(REPO, 'daemon', 'pkg', 'qaschema');

const URI_PREFIX = 'https://qa.local/schema/';
const NON_PORTABLE_REGEX = /\(\?=|\(\?!|\(\?<|\\[1-9]/;

/** Go initialisms, so `runId` becomes `RunID` rather than `RunId`. */
const INITIALISMS = new Map([
  ['ID', 'ID'],
  ['IDS', 'IDs'],
  ['URL', 'URL'],
  ['URI', 'URI'],
  ['API', 'API'],
  ['HTTP', 'HTTP'],
  ['JSON', 'JSON'],
  ['UI', 'UI'],
  ['OS', 'OS'],
  ['DOM', 'DOM'],
]);

class GenError extends Error {}

// ---------------------------------------------------------------------------
// load + check
// ---------------------------------------------------------------------------

function loadSchemas() {
  const files = readdirSync(SCHEMA_DIR)
    .filter((f) => f.endsWith('.schema.json'))
    .sort();
  if (files.length === 0) throw new GenError(`no *.schema.json in ${SCHEMA_DIR}`);

  const schemas = files.map((file) => {
    const text = readFileSync(join(SCHEMA_DIR, file), 'utf8');
    let doc;
    try {
      doc = JSON.parse(text);
    } catch (cause) {
      throw new GenError(`${file}: not valid JSON: ${cause.message}`);
    }
    const uri = doc.$id;
    if (typeof uri !== 'string' || !uri.startsWith(URI_PREFIX)) {
      throw new GenError(`${file}: $id must start with ${URI_PREFIX}`);
    }
    const [name, major, ...extra] = uri.slice(URI_PREFIX.length).split('/');
    if (extra.length > 0 || !name || !/^[0-9]+$/.test(major ?? '')) {
      throw new GenError(`${file}: $id must look like ${URI_PREFIX}<name>/<major>, got ${uri}`);
    }
    if (`${name}.schema.json` !== file) {
      throw new GenError(`${file}: file name must match the $id name "${name}"`);
    }
    if (typeof doc['x-contract-version'] !== 'string') {
      throw new GenError(`${file}: missing "x-contract-version"`);
    }
    if (!doc['x-contract-version'].startsWith(`${major}.`)) {
      throw new GenError(
        `${file}: x-contract-version ${doc['x-contract-version']} does not belong to major ${major}`,
      );
    }
    if (typeof doc.$ref !== 'string') {
      throw new GenError(`${file}: the document must $ref its root type`);
    }
    if (typeof doc.$defs !== 'object' || doc.$defs === null) {
      throw new GenError(`${file}: missing $defs`);
    }
    return { file, id: `${name}@${major}`, name, major, uri, doc, text };
  });

  schemas.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  return schemas;
}

/** Rejects regexes that only work in one of the two runtimes. */
function checkPatterns(schemas) {
  const walk = (node, where) => {
    if (Array.isArray(node)) {
      node.forEach((item, i) => walk(item, `${where}/${i}`));
      return;
    }
    if (typeof node !== 'object' || node === null) return;
    for (const key of Object.keys(node).sort()) {
      const value = node[key];
      if (key === 'pattern') {
        if (typeof value !== 'string') throw new GenError(`${where}/pattern: must be a string`);
        if (NON_PORTABLE_REGEX.test(value)) {
          throw new GenError(
            `${where}/pattern: "${value}" uses lookaround or a backreference, which Go's RE2 cannot compile`,
          );
        }
        try {
          new RegExp(value, 'u');
        } catch (cause) {
          throw new GenError(`${where}/pattern: "${value}" does not compile: ${cause.message}`);
        }
      }
      walk(value, `${where}/${key}`);
    }
  };
  for (const schema of schemas) walk(schema.doc, schema.uri);
}

// ---------------------------------------------------------------------------
// naming
// ---------------------------------------------------------------------------

function words(value) {
  return value
    .replace(/[^A-Za-z0-9]+/g, ' ')
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .trim()
    .split(/\s+/)
    .filter(Boolean);
}

function pascal(value) {
  return words(value)
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1).toLowerCase())
    .join('');
}

function goName(value) {
  return words(value)
    .map((word) => {
      const initialism = INITIALISMS.get(word.toUpperCase());
      if (initialism !== undefined) return initialism;
      return word.charAt(0).toUpperCase() + word.slice(1);
    })
    .join('');
}

function screamingSnake(value) {
  return words(value)
    .map((word) => word.toUpperCase())
    .join('_');
}

// ---------------------------------------------------------------------------
// model
// ---------------------------------------------------------------------------

function buildModel(schemas) {
  /** pointer (`<uri>#/$defs/<Name>`) -> exported type name */
  const typeByPointer = new Map();
  /** type name -> where it came from, for collision reporting */
  const origin = new Map();

  for (const schema of schemas) {
    for (const defName of Object.keys(schema.doc.$defs)) {
      const def = schema.doc.$defs[defName];
      if (def['x-codegen'] === 'skip') continue;
      if (origin.has(defName)) {
        throw new GenError(
          `type name collision: "${defName}" is declared by both ${origin.get(defName)} and ${schema.id}`,
        );
      }
      origin.set(defName, schema.id);
      typeByPointer.set(`${schema.uri}#/$defs/${defName}`, defName);
    }
  }

  const declarations = [];
  const declared = new Set();

  const declare = (decl) => {
    if (declared.has(decl.name)) return;
    declared.add(decl.name);
    declarations.push(decl);
  };

  const resolveRef = (ref, schema, where) => {
    const pointer = ref.startsWith('#') ? `${schema.uri}${ref}` : ref;
    const name = typeByPointer.get(pointer);
    if (name === undefined) {
      throw new GenError(`${where}: $ref "${ref}" does not point at a generated $defs entry`);
    }
    return name;
  };

  /** Maps one property/items schema to a Go and a TypeScript type. */
  const renderType = (node, schema, owner, hint, where) => {
    if (typeof node === 'boolean') {
      throw new GenError(`${where}: a boolean schema has no generated type`);
    }
    if (typeof node['x-go-type'] === 'string' || typeof node['x-ts-type'] === 'string') {
      if (typeof node['x-go-type'] !== 'string' || typeof node['x-ts-type'] !== 'string') {
        throw new GenError(`${where}: x-go-type and x-ts-type must be given together`);
      }
      return { go: node['x-go-type'], ts: node['x-ts-type'] };
    }
    if (typeof node.$ref === 'string') {
      const name = resolveRef(node.$ref, schema, where);
      return { go: goName(name), ts: name };
    }
    if (Array.isArray(node.enum)) {
      if (!node.enum.every((v) => typeof v === 'string')) {
        throw new GenError(`${where}: only string enums can be generated`);
      }
      const name = `${owner}${pascal(hint)}`;
      declare({
        kind: 'enum',
        name,
        goName: goName(name),
        values: node.enum,
        description: node.description,
        origin: where,
      });
      return { go: goName(name), ts: name };
    }
    if ('const' in node) {
      const value = node.const;
      if (typeof value === 'number') return { go: Number.isInteger(value) ? 'int' : 'float64', ts: String(value) };
      if (typeof value === 'boolean') return { go: 'bool', ts: String(value) };
      if (typeof value === 'string') return { go: 'string', ts: JSON.stringify(value) };
      throw new GenError(`${where}: unsupported const type`);
    }

    const types = Array.isArray(node.type) ? node.type : [node.type];
    const nullable = types.includes('null');
    const concrete = types.filter((t) => t !== 'null');
    if (concrete.length !== 1) {
      throw new GenError(`${where}: expected exactly one non-null type, got ${JSON.stringify(node.type)}`);
    }

    let rendered;
    switch (concrete[0]) {
      case 'string':
        rendered = { go: 'string', ts: 'string' };
        break;
      case 'integer':
        rendered = { go: 'int', ts: 'number' };
        break;
      case 'number':
        rendered = { go: 'float64', ts: 'number' };
        break;
      case 'boolean':
        rendered = { go: 'bool', ts: 'boolean' };
        break;
      case 'array': {
        if (node.items === undefined) throw new GenError(`${where}: array without items`);
        const item = renderType(node.items, schema, owner, `${hint}Item`, `${where}/items`);
        rendered = { go: `[]${item.go}`, ts: `${item.ts}[]` };
        break;
      }
      case 'object':
        if (node.properties !== undefined) {
          throw new GenError(`${where}: inline object shapes must be lifted into $defs`);
        }
        rendered = { go: 'map[string]any', ts: 'Record<string, unknown>' };
        break;
      default:
        throw new GenError(`${where}: unsupported type ${JSON.stringify(node.type)}`);
    }

    return nullable ? { ...rendered, nullable: true } : rendered;
  };

  const structFields = (schema, defName, def, where) => {
    const required = new Set(Array.isArray(def.required) ? def.required : []);
    return Object.keys(def.properties).map((prop) => {
      const node = def.properties[prop];
      const type = renderType(node, schema, defName, prop, `${where}/properties/${prop}`);
      return {
        json: prop,
        go: goName(prop),
        goType: type.go,
        tsType: type.ts,
        required: required.has(prop),
        nullable: type.nullable === true,
        description: typeof node.description === 'string' ? node.description : undefined,
      };
    });
  };

  /**
   * Go has no sum type, so a discriminated union becomes one struct holding
   * every variant's fields. Where two variants disagree on a field's type the
   * merged field keeps the raw JSON: losing the value would be worse than
   * making the caller decode it.
   */
  const mergeVariants = (variantNames, discriminator) => {
    const merged = [];
    const byJson = new Map();
    for (const variant of variantNames) {
      const decl = declarations.find((d) => d.name === variant);
      if (decl === undefined || decl.kind !== 'struct') {
        throw new GenError(`union variant "${variant}" is not a generated struct`);
      }
      for (const field of decl.fields) {
        const existing = byJson.get(field.json);
        if (existing === undefined) {
          const copy = {
            ...field,
            required: field.json === discriminator,
          };
          byJson.set(field.json, copy);
          merged.push(copy);
        } else if (existing.goType !== field.goType) {
          existing.goType = 'json.RawMessage';
          existing.tsType = 'unknown';
          existing.conflicting = true;
        }
      }
    }
    return merged;
  };

  for (const schema of schemas) {
    for (const defName of Object.keys(schema.doc.$defs).sort()) {
      const def = schema.doc.$defs[defName];
      const where = `${schema.uri}#/$defs/${defName}`;
      if (def['x-codegen'] === 'skip') continue;

      // Discriminated unions and oneOf unions need their variants declared first.
      const union = def['x-codegen']?.union;
      if (union !== undefined) {
        const variants = [];
        for (const key of Object.keys(union.variants)) {
          const name = union.variants[key];
          if (!variants.includes(name)) variants.push(name);
        }
        declare({
          kind: 'union',
          name: defName,
          goName: goName(defName),
          variants,
          discriminator: union.discriminator,
          discriminatorSchema: def.properties?.[union.discriminator],
          schema,
          where,
          description: def.description,
          origin: where,
        });
        continue;
      }
      if (Array.isArray(def.oneOf)) {
        const variants = def.oneOf.map((branch, i) => {
          if (typeof branch.$ref !== 'string') {
            throw new GenError(`${where}/oneOf/${i}: a union branch must be a $ref to a $defs entry`);
          }
          return resolveRef(branch.$ref, schema, `${where}/oneOf/${i}`);
        });
        declare({
          kind: 'union',
          name: defName,
          goName: goName(defName),
          variants,
          description: def.description,
          origin: where,
        });
        continue;
      }
      if (Array.isArray(def.enum)) {
        if (!def.enum.every((v) => typeof v === 'string')) {
          throw new GenError(`${where}: only string enums can be generated`);
        }
        declare({
          kind: 'enum',
          name: defName,
          goName: goName(defName),
          values: def.enum,
          description: def.description,
          origin: where,
        });
        continue;
      }
      if (def.type === 'object' && def.properties !== undefined) {
        declare({
          kind: 'struct',
          name: defName,
          goName: goName(defName),
          fields: structFields(schema, defName, def, where),
          description: def.description,
          origin: where,
        });
        continue;
      }
      const alias = renderType(def, schema, defName, '', where);
      declare({
        kind: 'alias',
        name: defName,
        goName: goName(defName),
        goType: alias.nullable === true ? `*${alias.go}` : alias.go,
        tsType: alias.nullable === true ? `${alias.ts} | null` : alias.ts,
        description: def.description,
        origin: where,
      });
    }
  }

  // Second pass: unions can only be flattened once their variants exist.
  for (const decl of declarations) {
    if (decl.kind !== 'union') continue;
    decl.fields = mergeVariants(decl.variants, decl.discriminator);
    if (decl.discriminator !== undefined) {
      // Each variant pins the discriminator to a literal; the merged struct
      // wants the enum instead, so an unknown member is still a typed value.
      const type = renderType(
        decl.discriminatorSchema,
        decl.schema,
        decl.name,
        decl.discriminator,
        `${decl.where}/properties/${decl.discriminator}`,
      );
      const field = decl.fields.find((f) => f.json === decl.discriminator);
      field.goType = type.go;
      field.tsType = type.ts;
    }
    delete decl.discriminatorSchema;
    delete decl.schema;
    delete decl.where;
  }

  declarations.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return declarations;
}

// ---------------------------------------------------------------------------
// rendering: TypeScript
// ---------------------------------------------------------------------------

const TS_HEADER = `/* eslint-disable */
// Code generated by packages/qa-schema/scripts/generate.mjs. DO NOT EDIT.
// Run \`make gen\` after changing anything under packages/qa-schema/schemas.
`;

function tsDoc(text, indent = '') {
  if (!text) return '';
  const lines = wrap(text, 88 - indent.length);
  if (lines.length === 1) return `${indent}/** ${lines[0]} */\n`;
  return `${indent}/**\n${lines.map((l) => `${indent} * ${l}`).join('\n')}\n${indent} */\n`;
}

function renderSchemasTs(schemas) {
  const out = [TS_HEADER];
  out.push(`export const SCHEMA_IDS = [\n${schemas.map((s) => `  '${s.id}',`).join('\n')}\n] as const;\n`);
  out.push(`export type SchemaId = (typeof SCHEMA_IDS)[number];\n`);
  out.push(
    `export const SCHEMA_URIS: Record<SchemaId, string> = {\n${schemas
      .map((s) => `  '${s.id}': '${s.uri}',`)
      .join('\n')}\n};\n`,
  );
  out.push(
    `export const CONTRACT_VERSIONS: Record<SchemaId, string> = {\n${schemas
      .map((s) => `  '${s.id}': '${s.doc['x-contract-version']}',`)
      .join('\n')}\n};\n`,
  );
  out.push(`export const SCHEMA_DOCUMENTS: Record<SchemaId, unknown> = {`);
  for (const schema of schemas) {
    const body = JSON.stringify(schema.doc, null, 2)
      .split('\n')
      .map((line, i) => (i === 0 ? line : `  ${line}`))
      .join('\n');
    out.push(`  '${schema.id}': ${body},`);
  }
  out.push(`};\n`);
  return `${out.join('\n')}`;
}

function renderTypesTs(declarations) {
  const out = [TS_HEADER];
  for (const decl of declarations) {
    out.push(renderTsDecl(decl));
  }
  return out.join('\n');
}

function renderTsDecl(decl) {
  const doc = tsDoc(decl.description);
  switch (decl.kind) {
    case 'enum': {
      const constName = `${screamingSnake(decl.name)}_VALUES`;
      return (
        `${doc}export const ${constName} = [\n` +
        decl.values.map((v) => `  ${JSON.stringify(v)},`).join('\n') +
        `\n] as const;\n\n` +
        `export type ${decl.name} = (typeof ${constName})[number];\n`
      );
    }
    case 'alias':
      return `${doc}export type ${decl.name} = ${decl.tsType};\n`;
    case 'union':
      return `${doc}export type ${decl.name} = ${decl.variants.join(' | ')};\n`;
    case 'struct': {
      const fields = decl.fields
        .map((field) => {
          const fieldDoc = tsDoc(field.description, '  ');
          const optional = field.required ? '' : '?';
          const type = field.nullable ? `${field.tsType} | null` : field.tsType;
          return `${fieldDoc}  ${tsKey(field.json)}${optional}: ${type};`;
        })
        .join('\n');
      return `${doc}export interface ${decl.name} {\n${fields}\n}\n`;
    }
    default:
      throw new GenError(`unknown declaration kind ${decl.kind}`);
  }
}

function tsKey(name) {
  return /^[A-Za-z_$][A-Za-z0-9_$]*$/.test(name) ? name : JSON.stringify(name);
}

// ---------------------------------------------------------------------------
// rendering: Go
// ---------------------------------------------------------------------------

function wrap(text, width) {
  const lines = [];
  let current = '';
  for (const word of String(text).split(/\s+/).filter(Boolean)) {
    if (current === '') current = word;
    else if (`${current} ${word}`.length <= width) current += ` ${word}`;
    else {
      lines.push(current);
      current = word;
    }
  }
  if (current !== '') lines.push(current);
  return lines.length === 0 ? [''] : lines;
}

function goDoc(name, text, indent = '') {
  const body = text ? `${name} — ${text}` : `${name} is part of the generated contract types.`;
  return wrap(body, 76 - indent.length)
    .map((line) => `${indent}// ${line}`)
    .join('\n');
}

function renderTypesGo(declarations) {
  const usesJSON = declarations.some(
    (d) => d.fields?.some((f) => f.goType === 'json.RawMessage') || false,
  );
  const out = [
    '// Code generated by packages/qa-schema/scripts/generate.mjs. DO NOT EDIT.',
    '// Run `make gen` after changing anything under packages/qa-schema/schemas.',
    '',
    `package ${GO_PACKAGE}`,
    '',
  ];
  if (usesJSON) out.push('import "encoding/json"', '');

  for (const decl of declarations) {
    out.push(renderGoDecl(decl), '');
  }
  return `${out.join('\n').replace(/\n+$/, '')}\n`;
}

function renderGoDecl(decl) {
  const name = decl.goName;
  switch (decl.kind) {
    case 'enum': {
      const lines = [goDoc(name, decl.description), `type ${name} string`, ''];
      lines.push('const (');
      for (const value of decl.values) {
        lines.push(`\t${name}${goName(value)} ${name} = ${JSON.stringify(value)}`);
      }
      lines.push(')', '');
      lines.push(`// ${name}Values lists every member this build knows about, in schema order.`);
      lines.push(
        `var ${name}Values = []${name}{${decl.values.map((v) => `${name}${goName(v)}`).join(', ')}}`,
      );
      lines.push('');
      lines.push(
        `// IsValid reports whether v is a member this build knows about. A value that`,
        `// is not must be rejected with an error: quietly treating it as a default is`,
        `// how an unimplemented action turns into a passing test.`,
        `func (v ${name}) IsValid() bool {`,
        `\tfor _, candidate := range ${name}Values {`,
        `\t\tif v == candidate {`,
        `\t\t\treturn true`,
        `\t\t}`,
        `\t}`,
        `\treturn false`,
        `}`,
      );
      return lines.join('\n');
    }
    case 'alias':
      return `${goDoc(name, decl.description)}\ntype ${name} = ${decl.goType}`;
    case 'union': {
      const note =
        `Go has no sum type, so this struct carries the fields of every variant ` +
        `(${decl.variants.map(goName).join(', ')}), all optional except the discriminator. ` +
        `Switch on it, then read the fields that variant defines; the schema is what ` +
        `guarantees they are present.` +
        (decl.fields.some((f) => f.conflicting)
          ? ' A field whose type differs between variants is kept as raw JSON.'
          : '');
      const doc = goDoc(name, `${decl.description ? `${decl.description} ` : ''}${note}`);
      return `${doc}\n${goStructBody(decl)}`;
    }
    case 'struct':
      return `${goDoc(name, decl.description)}\n${goStructBody(decl)}`;
    default:
      throw new GenError(`unknown declaration kind ${decl.kind}`);
  }
}

function goStructBody(decl) {
  const lines = [`type ${decl.goName} struct {`];
  decl.fields.forEach((field, index) => {
    if (field.description) {
      if (index > 0) lines.push('');
      lines.push(goDoc(field.go, field.description, '\t'));
    }
    lines.push(`\t${field.go} ${goFieldType(field)} \`json:"${goTag(field)}"\``);
  });
  lines.push('}');
  return lines.join('\n');
}

function goFieldType(field) {
  const base = field.goType;
  const isReference = base.startsWith('[]') || base.startsWith('map[') || base === 'json.RawMessage';
  if (field.required && !field.nullable) return base;
  if (isReference) return base;
  return `*${base}`;
}

function goTag(field) {
  return field.required && !field.nullable ? field.json : `${field.json},omitempty`;
}

function renderIdsGo(schemas) {
  const lines = [
    '// Code generated by packages/qa-schema/scripts/generate.mjs. DO NOT EDIT.',
    '// Run `make gen` after changing anything under packages/qa-schema/schemas.',
    '',
    `package ${GO_PACKAGE}`,
    '',
    '// SchemaIDs lists every contract in this package, sorted. It is generated from',
    '// the files in packages/qa-schema/schemas, so it cannot drift from what is on',
    '// disk the way a hand-written list can.',
    'var SchemaIDs = []string{',
    ...schemas.map((s) => `\t${JSON.stringify(s.id)},`),
    '}',
    '',
    '// ContractVersions maps a schema id to its full semantic version. The major',
    '// matches the id suffix; a minor bump means members were added, and a consumer',
    '// built against an older minor must reject what it does not recognise.',
    'var ContractVersions = map[string]string{',
    ...schemas.map((s) => `\t${JSON.stringify(s.id)}: ${JSON.stringify(s.doc['x-contract-version'])},`),
    '}',
    '',
    '// schemaURIs maps a schema id to the $id used by $ref.',
    'var schemaURIs = map[string]string{',
    ...schemas.map((s) => `\t${JSON.stringify(s.id)}: ${JSON.stringify(s.uri)},`),
    '}',
    '',
    '// schemaFiles maps a schema id to its file inside the embedded schemas/ directory.',
    'var schemaFiles = map[string]string{',
    ...schemas.map((s) => `\t${JSON.stringify(s.id)}: ${JSON.stringify(s.file)},`),
    '}',
  ];
  return `${lines.join('\n')}\n`;
}

// ---------------------------------------------------------------------------
// writing
// ---------------------------------------------------------------------------

const written = [];

function write(path, content) {
  mkdirSync(dirname(path), { recursive: true });
  const previous = existsSync(path) ? readFileSync(path, 'utf8') : null;
  if (previous !== content) writeFileSync(path, content, 'utf8');
  written.push(relative(REPO, path));
}

function gofmt(paths) {
  if (paths.length === 0) return;
  const result = spawnSync('gofmt', ['-w', ...paths], { encoding: 'utf8' });
  if (result.error !== undefined || result.status !== 0) {
    throw new GenError(
      `gofmt failed (it is required so the generated Go passes \`make lint-go\`): ${
        result.error?.message ?? result.stderr
      }`,
    );
  }
}

function buildDist() {
  const tsc = require.resolve('typescript/bin/tsc');
  rmSync(join(PKG, 'dist'), { recursive: true, force: true });
  const result = spawnSync(process.execPath, [tsc, '-p', join(PKG, 'tsconfig.build.json')], {
    encoding: 'utf8',
    cwd: PKG,
  });
  if (result.status !== 0) {
    throw new GenError(`tsc failed:\n${result.stdout}${result.stderr}`);
  }
}

function main() {
  const schemas = loadSchemas();
  checkPatterns(schemas);
  const declarations = buildModel(schemas);

  write(join(PKG, 'src', 'schemas.generated.ts'), renderSchemasTs(schemas));
  write(join(PKG, 'src', 'types.generated.ts'), renderTypesTs(declarations));

  const typesGo = renderTypesGo(declarations);
  const idsGo = renderIdsGo(schemas);

  // The Go validator is authored once, in the server module, and mirrored.
  const authored = readdirSync(GO_SOURCE)
    .filter((f) => f.endsWith('.go') && !f.endsWith('.gen.go'))
    .sort();
  if (authored.length === 0) {
    throw new GenError(`${relative(REPO, GO_SOURCE)} has no authored Go sources to mirror`);
  }

  const goFiles = [];
  for (const dir of [GO_SOURCE, GO_MIRROR]) {
    const schemaOut = join(dir, 'schemas');
    rmSync(schemaOut, { recursive: true, force: true });
    mkdirSync(schemaOut, { recursive: true });
    for (const schema of schemas) {
      cpSync(join(SCHEMA_DIR, schema.file), join(schemaOut, schema.file));
      written.push(relative(REPO, join(schemaOut, schema.file)));
    }
    write(join(dir, 'types.gen.go'), typesGo);
    write(join(dir, 'ids.gen.go'), idsGo);
    goFiles.push(join(dir, 'types.gen.go'), join(dir, 'ids.gen.go'));
  }

  for (const file of authored) {
    const body = readFileSync(join(GO_SOURCE, file), 'utf8');
    // The mirror is a byte copy, so the package has to be self-contained. An
    // import of another server package would compile here and break there.
    if (body.includes('ChinnakornP/longtest/server')) {
      throw new GenError(
        `server/pkg/qaschema/${file} names its own module path; the daemon mirror is a ` +
          `verbatim copy, so this package may only import the standard library`,
      );
    }
    write(
      join(GO_MIRROR, file),
      `// Mirrored from server/pkg/qaschema/${file} by packages/qa-schema/scripts/generate.mjs.\n` +
        `// DO NOT EDIT: change the server copy and run \`make gen\`.\n\n${body}`,
    );
    goFiles.push(join(GO_MIRROR, file));
  }

  gofmt(goFiles);
  buildDist();

  const names = schemas.map((s) => s.id).join(', ');
  process.stdout.write(
    `qa-schema: ${schemas.length} contracts (${names})\n` +
      `qa-schema: ${declarations.length} generated types -> ` +
      `${relative(REPO, join(PKG, 'src'))}, ${relative(REPO, GO_SOURCE)}, ${relative(REPO, GO_MIRROR)}\n` +
      `qa-schema: dist built with tsc\n`,
  );
}

try {
  main();
} catch (cause) {
  if (cause instanceof GenError) {
    process.stderr.write(`qa-schema: ${cause.message}\n`);
    process.exit(1);
  }
  throw cause;
}
