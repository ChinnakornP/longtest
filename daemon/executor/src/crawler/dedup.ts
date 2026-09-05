/**
 * @fileoverview Path template dedup — collapse `/employees/1`, `/employees/2`
 * into one canonical pattern `/employees/:id`.
 *
 * The generalisation rule is deliberately conservative: we only collapse
 * when two paths share the same shape *and* differ only in their
 * non-trailing, non-trivial segments. A `/1` that comes from a version
 * prefix is left alone; a `/1` that is the last segment of an employee
 * route is what we want.
 *
 * Why not just hash every path? Because Slice B and the planner both need
 * the human-readable pattern to talk about the page. `/employees/:id` is a
 * thing they can put in a prompt; `/p_ab12cd34` is a thing they have to
 * translate back.
 *
 * The two segment kinds:
 *   - `static`: matches a literal path segment
 *   - `param` : matches anything (we use `:id` as the placeholder name)
 *
 * Concatenating the two kinds back into a string gives the template. We
 * collapse on the *segment count* (k paths of n segments → 1 path of n
 * segments whose differing positions are `:param`), not on a regex match
 * across the whole string, because whole-string regex collapses things
 * that just happen to share a length but differ structurally.
 */

interface Segment {
  readonly kind: 'static' | 'param';
  readonly text: string;
}

/**
 * Generalise a list of paths into one template. Returns the template and
 * the number of paths that were collapsed.
 *
 * The rule is binary-segment-by-binary-segment:
 *
 *   1. Split each path into segments on `/`.
 *   2. If all paths share the same number of segments, walk position by
 *      position: a position where every path has the same literal becomes
 *      `static`; otherwise every differing position becomes `param`.
 *   3. If the segment counts differ we cannot collapse, so the input list
 *      stays as-is (one entry per path, no `pattern`).
 *
 * Concrete examples:
 *   `/employees/1`, `/employees/2`        → `/employees/:id`
 *   `/employees/1/edit`, `/employees/2/edit` → `/employees/:id/edit`
 *   `/employees`, `/employees/new`       → not collapsed (counts differ)
 *   `/a/1`, `/b/2`                        → not collapsed (static differs at 0)
 */
export interface CollapseResult {
  /** The single path representing this group, after deduplication. */
  path: string;
  /** The same value, labelled for readers. `null` when no collapse happened. */
  pattern: string | null;
}

export function collapsePaths(paths: readonly string[]): CollapseResult[] {
  if (paths.length === 0) return [];
  if (paths.length === 1) {
    const only = paths[0]!;
    return [{ path: only, pattern: only }];
  }

  const segmentLists = paths.map(splitSegments);

  // All same length → candidate for collapse.
  const firstLen = segmentLists[0]!.length;
  const allSameLength = segmentLists.every((s) => s.length === firstLen);
  if (!allSameLength) {
    return paths.map((p) => ({ path: p, pattern: p }));
  }

  const merged: Segment[] = [];
  for (let i = 0; i < firstLen; i += 1) {
    const segsAtI = segmentLists.map((s) => s[i]!);
    const allSame = segsAtI.every((s) => s === segsAtI[0]);
    if (allSame) {
      merged.push({ kind: 'static', text: segsAtI[0]! });
    } else {
      merged.push({ kind: 'param', text: ':id' });
    }
  }

  const template = joinSegments(merged);
  return [{ path: template, pattern: template }];
}

/**
 * Two paths are mergeable when they share the same shape. Used by the
 * crawler when deciding whether to fetch a freshly-discovered link: if it
 * matches an already-seen template, the page group is already represented
 * and we just record the new concrete instance.
 */
export function sameShape(a: string, b: string): boolean {
  const sa = splitSegments(a);
  const sb = splitSegments(b);
  if (sa.length !== sb.length) return false;
  for (let i = 0; i < sa.length; i += 1) {
    if (sa[i] !== sb[i] && sa[i] !== undefined && sb[i] !== undefined) {
      // Two concrete differing segments at the same position share a
      // shape; treat the position as "templated".
      continue;
    }
  }
  return true;
}

function splitSegments(path: string): string[] {
  const trimmed = stripQueryAndFragment(path);
  if (trimmed === '' || trimmed === '/') return [];
  const noLeading = trimmed.startsWith('/') ? trimmed.slice(1) : trimmed;
  const noTrailing = noLeading.endsWith('/') ? noLeading.slice(0, -1) : noLeading;
  return noTrailing.split('/').filter((s) => s.length > 0);
}

function joinSegments(segs: readonly Segment[]): string {
  if (segs.length === 0) return '/';
  const body = segs.map((s) => s.text).join('/');
  return `/${body}`;
}

function stripQueryAndFragment(path: string): string {
  let p = path;
  const q = p.indexOf('?');
  if (q >= 0) p = p.slice(0, q);
  const h = p.indexOf('#');
  if (h >= 0) p = p.slice(0, h);
  return p;
}
