/**
 * Wire types for the project/run/runtime contract defined by T08
 * (server/internal/{project,run,runtime,report} — LONG-10, not yet landed).
 * Hand-written from the T08 issue contract, mirroring how lib/api/types.ts
 * covers T05. Keep in sync with the LONG-10 issue description; do not fork a
 * second copy elsewhere in web. Envelope/event/execution/finding/appmap
 * payloads are QA-domain and come from `@qa/schema` instead of being
 * redeclared here.
 *
 * `GET /api/v1/artifacts/{artifactId}` -> presigned URL is a frontend
 * assumption, not part of the published T08 contract (which never names an
 * artifact-download endpoint) - see the PR description for the open
 * question to Architect.
 */
import type {
  ApplicationMap,
  ExecutionResult,
  Finding,
  RunEventPayload,
  RunResultPayload,
  TestCase as TestCasePayload,
  TestCaseCategory,
  TestCasePriority,
} from '@qa/schema';

export interface Project {
  id: string;
  orgId: string;
  name: string;
  baseUrl: string;
  createdAt: string;
}

export interface CreateProjectRequest {
  name: string;
  baseUrl: string;
}

export const RUN_MODE_VALUES = ['discover', 'plan', 'execute', 'full'] as const;
export type RunMode = (typeof RUN_MODE_VALUES)[number];

export const RUN_STATUS_VALUES = [
  'queued',
  'running',
  'completed',
  'failed',
  'cancelled',
  'error',
] as const;
export type RunStatus = (typeof RUN_STATUS_VALUES)[number];

export const RUN_PHASE_VALUES = ['discover', 'plan', 'execute', 'analyze', 'report'] as const;
export type RunPhase = (typeof RUN_PHASE_VALUES)[number];

export interface RunCounters {
  pages?: number;
  workflows?: number;
  forms?: number;
  actions?: number;
  passed?: number;
  failed?: number;
  skipped?: number;
}

export interface Run {
  id: string;
  projectId: string;
  runtimeId: string;
  mode: RunMode;
  status: RunStatus;
  phase: RunPhase | null;
  counters: RunCounters;
  errorMessage?: string;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
}

export interface CreateRunRequest {
  projectId: string;
  runtimeId: string;
  mode: RunMode;
  testCaseIds?: string[];
}

export const RUNTIME_AGENT_NAME_VALUES = ['claude', 'opencode', 'antigravity'] as const;
export type RuntimeAgentName = (typeof RUNTIME_AGENT_NAME_VALUES)[number];

export interface RuntimeAgent {
  name: RuntimeAgentName;
  ok: boolean;
  version?: string;
  error?: string;
}

export interface Runtime {
  id: string;
  name: string;
  online: boolean;
  lastSeenAt: string | null;
  browsers: string[];
  agents: RuntimeAgent[];
}

/**
 * One row from `GET /runs/{id}/events?since={seq}`. Envelope type/seq lifted
 * out of the daemon-envelope@1 frame; `run.assign`/`run.cancel`/`hello`/
 * `heartbeat` frames are daemon<->server only and never appear here.
 */
export interface RunEventRecord {
  seq: number;
  type: 'run.event' | 'run.result';
  ts: string;
  payload: RunEventPayload | RunResultPayload;
}

export interface RunEventsPage {
  events: RunEventRecord[];
  /** Pass as `since` on the next call to resume without gap or duplicate. */
  nextSince: number;
}

export interface ReportResponse {
  executions: ExecutionResult[];
  findings: Finding[];
}

export interface ArtifactUrlResponse {
  url: string;
  expiresAt: string;
}

/**
 * Wire types for the T14 test-planner / coverage contract
 * (server/internal/{testcase,project} — LONG-16, landed at 4612b65). Mirrors
 * `docs/api/openapi.yaml` `TestCase` / `CoverageReport` / `Fixture`.
 *
 * The API has no endpoint to edit a test case's payload (steps/assertions) or
 * to list its version history — only `GET`/`PATCH .../status` exist. See the
 * LONG-18 PR description for the open question to Architect; the review UI
 * only supports the status transition until that lands.
 */

export const TEST_CASE_STATUS_VALUES = ['draft', 'approved', 'archived'] as const;
export type TestCaseStatus = (typeof TEST_CASE_STATUS_VALUES)[number];

export interface TestCaseRecord {
  id: string;
  projectId: string;
  ref: string;
  name: string;
  priority: TestCasePriority;
  category: TestCaseCategory;
  status: TestCaseStatus;
  version: number;
  sourceRunId?: string;
  /** The executable `test-case@1` document, passed through verbatim. */
  payload: TestCasePayload;
  createdAt: string;
  updatedAt: string;
}

export interface TestCaseListResponse {
  testCases: TestCaseRecord[];
  total: number;
}

export interface SetTestCaseStatusRequest {
  status: TestCaseStatus;
}

export const COVERAGE_STATUS_VALUES = ['covered', 'partial', 'uncovered'] as const;
export type CoverageStatus = (typeof COVERAGE_STATUS_VALUES)[number];

export const RISK_LEVEL_VALUES = ['high', 'medium', 'low'] as const;
export type RiskLevel = (typeof RISK_LEVEL_VALUES)[number];

export interface CoverageSuggestion {
  category: TestCaseCategory;
  /** Why this case is suggested, in one sentence. */
  reason: string;
}

export interface WorkflowCoverage {
  ref: string;
  name: string;
  expectedOutcome?: string;
  status: CoverageStatus;
  /** The best single case's share of the path, 0..1 — supplementary detail, never what decides the status color. */
  coverageRatio: number;
  coveringCaseRefs: string[];
  missingRefs?: string[];
  authRequired: boolean;
  risk: RiskLevel;
  suggestedTests: number;
  suggestions?: CoverageSuggestion[];
}

export interface PageCoverage {
  ref: string;
  path: string;
  title?: string;
  status: CoverageStatus;
  authRequired: boolean;
  risk: RiskLevel;
  suggestedTests: number;
  suggestions?: CoverageSuggestion[];
}

export interface CategoryCoverage {
  category: TestCaseCategory;
  approved: number;
  suggestedTests: number;
}

export interface CoverageReport {
  projectId: string;
  generatedAtRunId?: string;
  approvedCases: number;
  workflows: WorkflowCoverage[];
  pages: PageCoverage[];
  categories: CategoryCoverage[];
  suggestedTestCount: number;
  /** One sentence assembled from the counts by the server, never model prose — render verbatim. */
  summary: string;
}

/** No field for a username or a password, ever — the value lives in the daemon's sealed store on the operator's own machine. */
export interface Fixture {
  name: string;
  /** The exact string a test case's `preconditions` must use, e.g. `fixture:logged_in_as_admin`. */
  reference: string;
  description?: string;
  createdAt: string;
}

export interface FixtureListResponse {
  fixtures: Fixture[];
}

export interface RegisterFixtureRequest {
  name: string;
  description?: string;
}

export type { ApplicationMap };
