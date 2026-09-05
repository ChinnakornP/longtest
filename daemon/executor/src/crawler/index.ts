/**
 * @fileoverview Public surface of the crawler.
 *
 * Slice A exposes `crawl()` and `crawlAndWrite()` from this barrel. The
 * rest of the modules (`ref`, `locators`, `dedup`, `extract`, `events`,
 * `robots`, `types`) are exported too because the unit tests in
 * `test/crawler/*` exercise them in isolation; nothing outside this
 * directory should reach past the barrel.
 */
export { crawl, crawlAndWrite, type CrawlInput, type CrawlContext } from './crawler.ts';
export {
  RAW_CRAWL_SCHEMA_ID,
  RAW_CRAWL_VERSION,
  type CrawlOptions,
  type CrawlTallies,
  type RawCrawlData,
  type RawElement,
  type RawElementType,
  type RawLocator,
  type RawPage,
} from './types.ts';
export { buildElementRef, buildPageRef, slugLabel, slugPath } from './ref.ts';
export { buildLocatorChain, type RawDiscoveredElement } from './locators.ts';
export { collapsePaths, sameShape, type CollapseResult } from './dedup.ts';
export { extractPage, type ExtractInput, type ExtractOutput } from './extract.ts';
export {
  nullProgressSink,
  progressSnapshot,
  type CrawlProgress,
  type ProgressSink,
} from './events.ts';
export { fetchRobotsPolicy, parseRobotsTxt, permissivePolicy, type RobotsPolicy } from './robots.ts';
