/**
 * @fileoverview Stable `ref` derivation for crawler-discovered elements.
 *
 * A `ref` is the only handle the planner and the executor have for an
 * element. If it shifts between two runs against the same website, every
 * test case that referenced it dies. Slice A's acceptance criterion makes
 * this the single highest-risk piece: it must be a pure function of
 * (page path, role, label) with zero time-, order- or randomness-dependence.
 *
 * Ref shape: `^[A-Za-z0-9][A-Za-z0-9_-]*(\\.[A-Za-z0-9][A-Za-z0-9_-]*)*$`
 *
 * Construction rule (this is the part that MUST stay stable across runs):
 *
 *   pageRef(role, pagePath)   = "page." + slugPath(pagePath)
 *   elementRef(role, label)   = slug(role) + "." + slugLabel(label)
 *   ref                       = pageRef + "." + elementRef
 *
 * Slugging is documented inline; it is intentionally lossy on display text
 * but lossless on shape so two runs against the same DOM produce the same
 * string. Same-label collisions on the same page get a deterministic
 * 1-based counter suffix, never a hash.
 */
import type { RawElementType } from './types.ts';

/** Subset of `application-map@1` Element.type we care about for the page ref. */
const PAGE_ROLE_PREFIX = 'page' as const;

/** Max slug length. Longer text gets truncated + a short stable hash. */
const MAX_SLUG_LEN = 40;

/**
 * Slug a path into a ref-safe identifier. `/employees/:id` → `employees.id`.
 * Query strings, fragments and a leading `/` are stripped; uppercase letters
 * are lowercased; non-alphanumerics collapse to `.`. The result is never
 * empty (a bare `/` becomes `root`); the schema regex forbids a leading
 * underscore so we cannot use `_root`.
 */
export function slugPath(path: string): string {
  // Strip query and fragment first; they are not part of the path identity.
  let p = path;
  const q = p.indexOf('?');
  if (q >= 0) p = p.slice(0, q);
  const h = p.indexOf('#');
  if (h >= 0) p = p.slice(0, h);
  p = p.trim();
  if (p === '' || p === '/') return 'root';
  // Drop the leading slash so we don't double-dot.
  if (p.startsWith('/')) p = p.slice(1);
  if (p.endsWith('/')) p = p.slice(0, -1);
  // /a/:id/b → a.id.b
  const slugged = p
    .toLowerCase()
    .replace(/[^a-z0-9_.-]+/g, '.')
    .replace(/\.{2,}/g, '.')
    .replace(/^[.\-_]+|[.\-_]+$/g, '');
  return slugged === '' ? 'root' : slugged;
}

/**
 * Slug a label or role string into a ref-safe identifier. Whitespace and
 * punctuation collapse to `-`; uppercase becomes lowercase. Empty input
 * becomes `_unlabelled` so the ref still satisfies the schema regex.
 */
export function slugLabel(label: string): string {
  const trimmed = label.trim().toLowerCase();
  if (trimmed === '') return '_unlabelled';
  const slugged = trimmed
    .replace(/[^a-z0-9_-]+/g, '-')
    .replace(/-{2,}/g, '-')
    .replace(/^-+|-+$/g, '');
  if (slugged === '') return '_unlabelled';
  if (slugged.length <= MAX_SLUG_LEN) return slugged;
  // Truncate and append a stable 4-character hash so two long-but-distinct
  // labels do not collide on the same page. The hash is FNV-1a 32-bit,
  // written in hex — deterministic, fast, no dependency.
  const hash = fnv1a32(trimmed).toString(16).padStart(8, '0').slice(0, 4);
  return `${slugged.slice(0, MAX_SLUG_LEN - 5)}-${hash}`;
}

/**
 * The page ref is `page.<slugPath>`. The full ref of an element is the page
 * ref + `.` + `<role>.<slugLabel>`. Both halves must pass the schema regex,
 * which means they have to start with `[A-Za-z0-9]` and contain only
 * `[A-Za-z0-9_-]`. The slugs above guarantee that.
 */
export function buildPageRef(pagePath: string): string {
  return `${PAGE_ROLE_PREFIX}.${slugPath(pagePath)}`;
}

/**
 * Build the ref for one element. Counter is the 1-based index of this
 * element among siblings with the same (role, label) on the same page —
 * callers maintain the counter and pass it in so the derivation itself
 * stays pure.
 */
export function buildElementRef(input: {
  pageRef: string;
  role: RawElementType;
  label: string;
  /** 1-based collision counter; 1 means "first occurrence". */
  collision: number;
}): string {
  const roleSlug = slugLabel(input.role);
  const labelSlug = slugLabel(input.label);
  const base = `${input.pageRef}.${roleSlug}.${labelSlug}`;
  if (input.collision <= 1) return base;
  // Disambiguate duplicate (role, label) on the same page with a
  // deterministic counter. The schema regex accepts `_N` because it is
  // pure `[A-Za-z0-9_-]`.
  return `${base}_${input.collision}`;
}

/** FNV-1a 32-bit. Small, allocation-free, deterministic. */
function fnv1a32(s: string): number {
  let hash = 0x811c9dc5;
  for (let i = 0; i < s.length; i += 1) {
    hash ^= s.charCodeAt(i);
    // Equivalent to `hash *= 16777619` with 32-bit overflow.
    hash = (hash + ((hash << 1) + (hash << 4) + (hash << 7) + (hash << 8) + (hash << 24))) >>> 0;
  }
  return hash >>> 0;
}
