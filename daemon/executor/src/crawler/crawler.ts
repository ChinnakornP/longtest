/**
 * @fileoverview BFS crawler — visits the app starting at `baseUrl`, applies
 * depth / max-pages / same-domain / robots.txt limits, extracts one
 * `RawPage` per visited page, dedups paths into templates, and writes the
 * result to the workspace.
 *
 * The crawler is a *sidecar module*: it owns a Chromium page through a
 * borrowed Playwright `BrowserContext`, but the lifecycle of that context
 * is the caller's (the daemon keeps the sidecar alive across test cases;
 * the integration test borrows it for the duration of one test). The
 * contract is:
 *
 *   - `crawl()` returns a `RawCrawlData`.
 *   - The caller decides what to do with it (write to workspace, pass to
 *     the planner, ...).
 *
 * Slice A exposes `runCrawlAndWrite()` for the daemon path and `crawl()`
 * for the integration test. Both share the same BFS loop.
 */
import { mkdir, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import type { BrowserContext, Page } from 'playwright';
import type {
  RAW_CRAWL_SCHEMA_ID,
  CrawlOptions,
  CrawlTallies,
  RawCrawlData,
  RawPage,
} from './types.ts';
import {
  RAW_CRAWL_SCHEMA_ID as SCHEMA_ID,
  RAW_CRAWL_VERSION,
} from './types.ts';
import { extractPage } from './extract.ts';
import { fetchRobotsPolicy, permissivePolicy, type RobotsPolicy } from './robots.ts';
import { collapsePaths } from './dedup.ts';
import { nullProgressSink, progressSnapshot, type ProgressSink } from './events.ts';

export interface CrawlInput {
  baseUrl: string;
  /** BFS depth from baseUrl. 0 means "only the baseUrl". Default 3. */
  depth?: number;
  /** Maximum pages to visit. Default 50. */
  maxPages?: number;
  /** Honour robots.txt. Default true. */
  respectRobots?: boolean;
  /** Per-page navigation timeout in ms. Default 15 000. */
  pageTimeoutMs?: number;
  /** Progress sink. Default drops every event. */
  progress?: ProgressSink;
}

export interface CrawlContext {
  /** Already-launched Playwright browser context to borrow. */
  context: BrowserContext;
  /** `true` when the caller wants raw data, `false` to skip persistence. */
  destination?: { workspaceDir: string; fileName?: string };
}

const DEFAULTS: Required<Pick<CrawlInput, 'depth' | 'maxPages' | 'respectRobots' | 'pageTimeoutMs'>> = {
  depth: 3,
  maxPages: 50,
  respectRobots: true,
  pageTimeoutMs: 15_000,
};

/**
 * Crawl the app under test and write `raw-crawl@1.json` into the run's
 * workspace. The output path is `<workspaceDir>/<fileName>`; the file's
 * directory is created if it does not exist.
 */
export async function crawlAndWrite(input: CrawlInput, ctx: CrawlContext): Promise<RawCrawlData> {
  const data = await crawl(input, ctx);
  const fileName = ctx.destination?.fileName ?? 'raw-crawl@1.json';
  if (ctx.destination !== undefined) {
    const fullPath = join(ctx.destination.workspaceDir, fileName);
    await mkdir(dirname(fullPath), { recursive: true });
    await writeFile(fullPath, `${JSON.stringify(data, null, 2)}\n`, 'utf8');
  }
  return data;
}

/**
 * Crawl the app under test and return the raw crawl data. Does not write
 * anything to disk; the integration test calls this directly to keep its
 * own scratch dir under its control.
 */
export async function crawl(input: CrawlInput, ctx: CrawlContext): Promise<RawCrawlData> {
  const opts: Required<Omit<CrawlInput, 'progress' | 'baseUrl' | 'destination'>> & { baseUrl: string } = {
    baseUrl: input.baseUrl,
    depth: input.depth ?? DEFAULTS.depth,
    maxPages: input.maxPages ?? DEFAULTS.maxPages,
    respectRobots: input.respectRobots ?? DEFAULTS.respectRobots,
    pageTimeoutMs: input.pageTimeoutMs ?? DEFAULTS.pageTimeoutMs,
  };
  const progress = input.progress ?? nullProgressSink;

  progress.emit(progressSnapshot({
    phase: 'starting',
    pagesDiscovered: 0,
    elementsObserved: 0,
    formsObserved: 0,
    actionsObserved: 0,
  }));

  const base = new URL(opts.baseUrl);
  const baseOrigin = base.origin;

  // One page, reused: a fresh page per visited URL is slow and unnecessary
  // because we `goto()` from scratch each time. The context outlives this
  // function so cookies/storage set up by the caller survive the crawl.
  const page = await ctx.context.newPage();

  const robots: RobotsPolicy = opts.respectRobots
    ? await fetchRobotsPolicy(opts.baseUrl)
    : permissivePolicy;

  // BFS frontier: each entry is a (path, depth) pair we still need to visit.
  const queue: Array<{ path: string; depth: number }> = [{ path: base.pathname + base.search, depth: 0 }];
  const visited = new Set<string>();
  const concretePages: RawPage[] = [];
  let formsSeen = 0;
  let actionsSeen = 0;

  while (queue.length > 0 && concretePages.length < opts.maxPages) {
    const next = queue.shift()!;
    if (visited.has(next.path)) continue;
    if (next.depth > opts.depth) continue;
    if (!robots.isAllowed(next.path)) {
      visited.add(next.path);
      continue;
    }

    visited.add(next.path);

    progress.emit(progressSnapshot({
      phase: 'fetching',
      pagesDiscovered: concretePages.length,
      elementsObserved: tallyElements(concretePages),
      formsObserved: formsSeen,
      actionsObserved: actionsSeen,
    }));

    const url = new URL(next.path, opts.baseUrl).toString();
    try {
      await page.goto(url, { waitUntil: 'load', timeout: opts.pageTimeoutMs });
    } catch {
      // A timeout / network error on one page is not a reason to abort
      // the crawl; record nothing and move on. The dedup step will skip
      // anything we did not visit.
      continue;
    }

    progress.emit(progressSnapshot({
      phase: 'extracting',
      pagesDiscovered: concretePages.length + 1,
      elementsObserved: tallyElements(concretePages),
      formsObserved: formsSeen,
      actionsObserved: actionsSeen,
    }));

    let extracted;
    try {
      extracted = await extractPage({ url, page });
    } catch {
      continue;
    }

    formsSeen += extracted.formCount;
    actionsSeen += extracted.actionCount;

    const discovered = await collectLinks(page, baseOrigin);

    concretePages.push({
      path: extracted.path,
      pattern: null,
      depth: next.depth,
      title: extracted.title,
      authRequired: extracted.authRequired,
      elements: extracted.elements,
    });

    if (next.depth < opts.depth) {
      for (const link of discovered) {
        if (!visited.has(link.path)) {
          queue.push({ path: link.path, depth: next.depth + 1 });
        }
      }
    }
  }

  progress.emit(progressSnapshot({
    phase: 'deduplicating',
    pagesDiscovered: concretePages.length,
    elementsObserved: tallyElements(concretePages),
    formsObserved: formsSeen,
    actionsObserved: actionsSeen,
  }));

  const deduped = dedupPages(concretePages);
  const finalised = deduped.map((p) => attachPattern(p));

  progress.emit(progressSnapshot({
    phase: 'finalising',
    pagesDiscovered: finalised.length,
    elementsObserved: tallyElements(finalised),
    formsObserved: formsSeen,
    actionsObserved: actionsSeen,
  }));

  const data: RawCrawlData = {
    schemaId: SCHEMA_ID,
    version: RAW_CRAWL_VERSION,
    baseUrl: opts.baseUrl,
    generatedAt: new Date().toISOString(),
    options: {
      depth: opts.depth,
      maxPages: opts.maxPages,
      respectRobots: opts.respectRobots,
    },
    pages: finalised,
    tallies: { formCount: formsSeen, actionCount: actionsSeen },
  };

  progress.emit(progressSnapshot({
    phase: 'done',
    pagesDiscovered: finalised.length,
    elementsObserved: tallyElements(finalised),
    formsObserved: formsSeen,
    actionsObserved: actionsSeen,
  }));

  return data;
}

function tallyElements(pages: readonly RawPage[]): number {
  let n = 0;
  for (const p of pages) n += p.elements.length;
  return n;
}

/**
 * Group pages by shape, then collapse each group. We group by the
 * sequence of "static vs variable" positions in the path: pages in the
 * same group are guaranteed to collapse to the same template.
 *
 * This is O(n^2) over page count, which is fine: maxPages defaults to
 * 50 and paths are short.
 */
function dedupPages(pages: readonly RawPage[]): RawPage[] {
  if (pages.length <= 1) return pages.slice();
  // Group by shape vector (a string of `S` for static positions and `P`
  // for positions that vary within the group).
  const shapeKey = (path: string): string => {
    const segs = path.split('/').filter((s) => s.length > 0);
    return segs.map((s) => (/^[A-Za-z][\w-]*$/.test(s) ? 'S' : 'P')).join('/');
  };
  const groups = new Map<string, RawPage[]>();
  for (const page of pages) {
    const key = shapeKey(page.path);
    const list = groups.get(key) ?? [];
    list.push(page);
    groups.set(key, list);
  }

  const out: RawPage[] = [];
  for (const group of groups.values()) {
    const collapsed = collapsePaths(group.map((p) => p.path));
    if (collapsed.length === 1 && group.length > 1) {
      const representative = group[0]!;
      out.push({
        ...representative,
        path: collapsed[0]!.path,
      });
    } else {
      out.push(...group);
    }
  }
  // Preserve a stable order: by path then depth.
  out.sort((a, b) => a.path.localeCompare(b.path) || a.depth - b.depth);
  return out;
}

function attachPattern(p: RawPage): RawPage {
  return { ...p, pattern: p.path };
}

async function collectLinks(page: Page, baseOrigin: string): Promise<Array<{ path: string }>> {
  const hrefs = await page.$$eval('a[href]', (els) => els.map((e) => e.getAttribute('href') ?? ''));
  const out: Array<{ path: string }> = [];
  const seen = new Set<string>();
  for (const raw of hrefs) {
    const path = normaliseLink(raw, baseOrigin);
    if (path === null) continue;
    if (seen.has(path)) continue;
    seen.add(path);
    out.push({ path });
  }
  return out;
}

function normaliseLink(href: string, baseOrigin: string): string | null {
  const trimmed = href.trim();
  if (trimmed === '') return null;
  if (trimmed.startsWith('javascript:')) return null;
  if (trimmed.startsWith('mailto:')) return null;
  if (trimmed.startsWith('#')) return null;
  try {
    const u = new URL(trimmed, `${baseOrigin}/`);
    if (u.origin !== baseOrigin) return null;
    // Drop the trailing slash if it's just the origin root — otherwise
    // `/login` and `/login/` collide differently in dedup.
    return u.pathname + u.search;
  } catch {
    return null;
  }
}

// Re-export the schema id so callers (the daemon) can check the version
// they read back from disk without re-declaring it.
export type { RAW_CRAWL_SCHEMA_ID };
export type { CrawlOptions, CrawlTallies };
