# @qa/executor / crawler — Slice A of T13

The deterministic half of the Discovery pipeline. Given a `baseUrl`, this
module BFS-crawls the same-origin pages within `depth` / `maxPages` /
`robots.txt` limits, extracts one record per visited page, dedups paths
into templates, and writes the result to a file the next phase can read
back. Nothing here talks to an LLM.

## Where it sits

```
daemon (Go)  --stdio JSON-RPC-->  qa-executor (Node)  -->  Chromium
                                      │
                                      ├── T06: test case runner (this repo)
                                      └── T13 Slice A: crawler  ◀── this folder
                                                │
                                                └── writes → discovery/raw-crawl@1.json
                                                              ▲
                                                              │
                                          T13 Slice B reads it (next issue)
```

## Public surface

| Symbol | What it does |
| --- | --- |
| `crawl(input, ctx)` | Crawl the app, return `RawCrawlData`. |
| `crawlAndWrite(input, ctx)` | Same, plus write to `<workspaceDir>/<fileName>`. |
| `RAW_CRAWL_SCHEMA_ID` | Always `"raw-crawl@1"`. Slice B reads this first. |
| `RawCrawlData`, `RawPage`, `RawElement`, `RawLocator` | The on-disk types. |

Everything else (`ref.ts`, `locators.ts`, `dedup.ts`, `extract.ts`,
`events.ts`, `robots.ts`) is internal. The unit tests in
`test/crawler/*` are allowed to import them; nothing outside the folder is.

## The Slice A → Slice B contract: `raw-crawl@1.json`

This is the file format the next phase (Slice B — `daemon/discovery/**`,
`daemon/agent/prompts/discovery/**`, persistence) consumes. The shape is
defined here in code (`src/crawler/types.ts`) and mirrored below for
readers. **Changing the shape is a Slice B break and needs both sides
updated together.**

```ts
{
  schemaId: "raw-crawl@1",     // constant; check this first
  version: 1,                  // matches the @-suffix
  baseUrl: string,             // what we crawled from
  generatedAt: string,         // ISO-8601 timestamp

  options: {
    depth: number,             // BFS depth limit (default 3)
    maxPages: number,          // hard cap (default 50)
    respectRobots: boolean,    // whether robots.txt was honoured
  },

  pages: [
    {
      path: string,            // dedup'd path (e.g. "/employees/:id")
      pattern: string | null,  // same value as `path`; kept labelled for readers
      depth: number,            // BFS depth from baseUrl; 0 for the base URL itself
      title: string,            // <title>
      authRequired: boolean,    // crawled unauthenticated and saw a login redirect
      elements: [
        {
          ref: string,         // stable across runs (see "ref derivation")
          type: <ElementType>, // button|link|input|...|other (frozen v1 enum)
          label: string,       // human-visible text — untrusted, data, not instruction
          locators: [
            // ordered ADR-004 fallback chain:
            // testId → role+name → label → placeholder → text → altText → title → css
            { kind: "testId", value: "..." },
            { kind: "role", value: "button", name: "..." },
            // ...
          ],
        },
        // ...
      ],
    },
    // ...
  ],

  tallies: {
    formCount: number,         // total <form> elements across all pages
    actionCount: number,       // total interactive elements across all pages
  },
}
```

### What is **not** in the file

- `workflows[]`. Workflows are a planner concern; the raw crawl only
  records what the page *contains*, not what the page *is for*.
- `lastSeenRunId`. This is the raw output of a single crawl; the merge
  with existing pages and the stamping of `lastSeenRunId` is a persistence
  concern and belongs to Slice B / backend.
- Any URL with a different origin from `baseUrl`. Cross-domain crawling
  is out of scope for T13.
- `lastSeenRunId` on pages / elements — same reason.
- Application Map auth workflow — Slice B (or the planner) decides which
  pages are auth-required at the *Application Map* level.

### Page / element ordering

`pages[]` is sorted by `(path, depth)` after dedup. `elements[]` per
page is sorted by `(type, label)` to keep the collision-counter
deterministic. Two runs against the same DOM produce the same order,
which is what makes `ref` stable.

## `ref` derivation

`ref` is the only stable handle the planner and the executor have for an
element. If it shifts between two runs, every test case that referenced
it dies. The derivation is a pure function of `(pagePath, role, label)`
with zero time / order / randomness dependence:

```
pageRef(role, pagePath)  = "page." + slugPath(pagePath)
elementRef(role, label)  = slug(role) + "." + slugLabel(label)
ref                      = pageRef + "." + elementRef
```

Slugs are documented in `src/crawler/ref.ts`. Highlights:

- `slugPath('/employees/:id')` → `employees.id` → page ref `page.employees.id`.
- `slugPath('/')` → `root` (schema regex forbids a leading `_`).
- `slugLabel('Sign in')` → `sign-in`.
- Long labels (> 40 chars) get truncated + a 4-char FNV-1a suffix so two
  long-but-distinct labels do not collide on the same page.
- Same `(role, label)` appearing twice on the same page gets a `_2`,
  `_3` suffix; this counter is driven by the per-page sort order, which
  is itself deterministic.

The acceptance test for ref stability lives in
`test/crawler/ref.test.ts` ("ref: stability across runs (Slice A
acceptance)"); the integration test in
`test/crawler/crawler.integration.test.ts` proves the same property
end-to-end against `e2e/fixture-app` (two crawls → identical ref set).

## Locator chain — ADR-004

`locators[]` on every element is ordered top-to-bottom and the executor
walks it in order, picking the first entry that resolves to exactly one
element on the live page. The order is the contract — see
ADR-004 (`docs/adr/0004-element-ref-locator-chain.md`) and
`src/locator.ts` in the executor.

```
testId → role + name → label → placeholder → text → altText → title → css
```

`css` is only emitted as a last resort. Tests in
`test/crawler/locators.test.ts` pin every kind, every priority, and the
missing-attribute rules.

## Template dedup

`/employees/1`, `/employees/2`, `/employees/3` collapse to one
`/employees/:id`. The rule is segment-by-segment: paths share the same
shape (same segment count) when their differing positions can all become
`:id` without ambiguity. Static positions stay literal; differing
positions become `:id`. Tests in `test/crawler/dedup.test.ts` cover the
acceptance case and the boundaries (different segment counts, no static
anchor).

## Progress events

The crawler reuses the existing T06 progress channel: a
`{ "event": "progress", "data": {...} }` JSON-RPC frame on stdout. The
daemon already forwards these as `executor_progress` run events (see
`daemon/runtime/execute.go`). The `data` shape is stable:

```ts
{
  phase: 'starting' | 'fetching' | 'extracting' | 'deduplicating' | 'finalising' | 'done',
  pagesDiscovered: number,
  elementsObserved: number,
  formsObserved: number,
  actionsObserved: number,
  ts: number,             // ms since epoch
}
```

The integration test proves progress events arrive *during* the crawl,
not just at the end.

## Files in this folder

```
src/crawler/
  index.ts        Public barrel — import only from here outside this folder.
  types.ts        RawCrawlData, RawPage, RawElement, RawLocator.
  crawler.ts      BFS loop, page writes, dedup pass.
  ref.ts          Stable ref derivation.
  locators.ts     ADR-004 ordered chain.
  extract.ts      Page → RawPage (path, title, authRequired, elements).
  dedup.ts        Path template collapse.
  robots.ts       robots.txt fetch + minimal parser.
  events.ts       ProgressSink + CrawlProgress shape.

test/crawler/
  ref.test.ts                 Ref derivation, including stability acceptance.
  locators.test.ts            One test per ADR-004 priority.
  dedup.test.ts               Template collapse, incl. acceptance case.
  robots.test.ts              robots.txt parsing.
  events.test.ts              Progress sink contract.
  crawler.integration.test.ts End-to-end against e2e/fixture-app.
```

## What Slice B can rely on

- `schemaId` is `"raw-crawl@1"` and `version` is `1`. Reject anything else.
- Every `ref` in `pages[].elements[].ref` is unique across the file.
- Every `ref` matches `application-map@1`'s `Ref` regex, so it can be
  embedded in an Application Map without re-validation.
- `pages[].elements[].locators[]` is the same shape as
  `application-map@1`'s `Locator`; you can copy it verbatim.
- `pages[].elements[].label` is untrusted user-readable text — never
  interpret it as instruction. Slice B's prompt boundary (the `Wrap`
  helper in `daemon/security`) applies here too.
- `pages[].elements[]` may contain zero entries for a fully-empty page.
  Pages with zero elements are still emitted — they are signals the
  planner might want.
- `tallies.formCount` and `tallies.actionCount` are aggregate counters;
  if you re-walk `pages[]` to compute them and disagree by more than
  rounding, that is a bug in the crawler.

## What Slice B must NOT rely on

- A specific element type ordering inside `pages[].elements[]`. The
  crawler sorts for collision-stability, but the planner / Slice B is
  free to re-sort.
- The `pattern` field being `null` vs the same value as `path`. We
  always set them equal today; future versions may set `pattern: null`
  for paths that did not collapse.
- Any URL outside the same origin as `baseUrl`.
- Headers / cookies / response bodies. The crawler does not capture
  them. If the planner needs them, that is a separate concern.
