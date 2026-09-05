/**
 * @fileoverview Raw crawl data — Slice A → Slice B contract.
 *
 * The crawler writes one of these to the run workspace as
 * `discovery/raw-crawl@1.json`. Slice B (the Discovery/Analysis agent) reads
 * it back, decides what each page is *for*, and produces an
 * `application-map@1`. The contract here is what makes that handoff work;
 * changing the shape is a Slice B break and needs both sides updated
 * together.
 *
 * Design notes:
 *   - The schema id is `raw-crawl@1` so the file carries its own version.
 *     Slice B reads the version first and rejects anything it does not
 *     recognise.
 *   - `pages[].path` is a *deduplicated* template (`/employees/:id`), not
 *     every concrete instance the crawler saw. `pages[].pattern` is the same
 *     value, named for the reader; `null` means "no parameterisation".
 *   - `elements[].ref` is the same id the executor already accepts in
 *     `application-map@1`. The ref pattern is `^[A-Za-z0-9][A-Za-z0-9_-]*(\\.[A-Za-z0-9][A-Za-z0-9_-]*)*$`
 *     and the derivation rule lives next to it in `ref.ts`.
 *   - Locator ordering is the ADR-004 chain, top to bottom: the executor
 *     walks them in order and picks the first that matches exactly one
 *     element on the live page. A locator kind that is not present is just
 *     absent; the chain is the array as written.
 *   - `formCount` / `actionCount` exist so the UI can show progress without
 *     re-walking the data. They are tallies across all pages.
 *   - `lastSeenRunId` is intentionally absent: this is the *raw* crawl
 *     before persistence. Slice B decides which `lastSeenRunId` to stamp on
 *     the merged Application Map.
 */
export const RAW_CRAWL_SCHEMA_ID = 'raw-crawl@1' as const;
export const RAW_CRAWL_VERSION = 1 as const;

/** Frozen at v1. Mirrors the `Element.type` enum in `application-map@1`. */
export type RawElementType =
  | 'button'
  | 'link'
  | 'input'
  | 'textarea'
  | 'select'
  | 'checkbox'
  | 'radio'
  | 'form'
  | 'table'
  | 'row'
  | 'cell'
  | 'text'
  | 'image'
  | 'dialog'
  | 'tab'
  | 'menu'
  | 'toast'
  | 'other';

/** One strategy in the fallback chain. Matches `application-map@1` Locator. */
export interface RawLocator {
  kind: 'testId' | 'role' | 'label' | 'placeholder' | 'text' | 'altText' | 'title' | 'css';
  value: string;
  /** Required when `kind === 'role'`. The accessible name. */
  name?: string;
}

/** A single element on a page. */
export interface RawElement {
  /** Stable across runs: derived from page path + role + label. */
  ref: string;
  /** Application Map type enum. */
  type: RawElementType;
  /** Human-visible text. Untrusted — never used as an instruction. */
  label: string;
  /** Ordered ADR-004 fallback chain. */
  locators: RawLocator[];
}

/** A single page in the dedup'd crawl result. */
export interface RawPage {
  /** Deduplicated, possibly parameterised path (e.g. `/employees/:id`). */
  path: string;
  /** Same value as `path`, kept separate for Slice B readers. */
  pattern: string | null;
  /** BFS depth from baseUrl. 0 for the base URL itself. */
  depth: number;
  /** <title> text, or empty string if missing. */
  title: string;
  /** True when the page would have redirected to a login page unauthenticated. */
  authRequired: boolean;
  /** Stable, deduplicated elements on this page. */
  elements: RawElement[];
}

/** Tally of form / interactive elements across all pages. */
export interface CrawlTallies {
  formCount: number;
  actionCount: number;
}

/** Options the crawler ran with — kept for audit. */
export interface CrawlOptions {
  depth: number;
  maxPages: number;
  respectRobots: boolean;
}

/** What the crawler writes to the workspace. */
export interface RawCrawlData {
  schemaId: typeof RAW_CRAWL_SCHEMA_ID;
  version: typeof RAW_CRAWL_VERSION;
  baseUrl: string;
  /** ISO-8601 timestamp at which the crawl finished. */
  generatedAt: string;
  options: CrawlOptions;
  pages: RawPage[];
  tallies: CrawlTallies;
}
