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
import type { ExecutionResult, Finding, RunEventPayload, RunResultPayload } from '@qa/schema';

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
