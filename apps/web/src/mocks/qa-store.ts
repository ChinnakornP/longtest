import { randomUUID } from 'node:crypto';

import type { Artifact, ArtifactKind, ExecutionResult, Finding, RunEventPayload, RunResultPayload } from '@qa/schema';

import type {
  CreateProjectRequest,
  CreateRunRequest,
  Project,
  ReportResponse,
  Run,
  RunCounters,
  RunEventRecord,
  RunMode,
  Runtime,
} from '@/lib/api/qa-types';

import { applyRunEventToCounters } from '@/lib/run-events/counters';

/**
 * In-memory stand-in for the T08 backend (server/internal/{project,run,
 * runtime,report} - LONG-10, not yet landed). Implements the REST contract
 * from the T08 issue description; the WS half is stood in by
 * mocks/ws-polling-transport.ts, which polls the `events` endpoint below
 * instead of a second, separate fake. State lives only for the life of the
 * dev server process. Whoever lands T08 deletes this file (and the
 * app/api/v1/{projects,runs,runtimes,artifacts} route.mock.ts handlers) the
 * same way LONG-7 is set up to delete the T05 mock - see ADR-008.
 */

interface TimelineEntry {
  offsetMs: number;
  record: RunEventRecord;
}

interface StoredRun {
  run: Run;
  orgId: string;
  timeline: TimelineEntry[];
  totalDurationMs: number;
  startedAtMs: number;
  cancelledAtMs: number | null;
  report: ReportResponse;
}

class QaMockStore {
  projects = new Map<string, Project>();
  runtimes = new Map<string, { orgId: string; runtime: Runtime }>();
  runs = new Map<string, StoredRun>();
  seededOrgRuntimes = new Set<string>();
}

const globalForQaMock = globalThis as unknown as { __qaDomainMockStore?: QaMockStore };
export const qaMockStore = globalForQaMock.__qaDomainMockStore ?? new QaMockStore();
globalForQaMock.__qaDomainMockStore = qaMockStore;

// --- Projects ---------------------------------------------------------

export function listProjects(orgId: string): Project[] {
  return [...qaMockStore.projects.values()]
    .filter((p) => p.orgId === orgId)
    .sort((a, b) => b.createdAt.localeCompare(a.createdAt));
}

export function getProject(orgId: string, projectId: string): Project | null {
  const project = qaMockStore.projects.get(projectId);
  return project && project.orgId === orgId ? project : null;
}

export function createProject(orgId: string, body: CreateProjectRequest): Project {
  const project: Project = {
    id: randomUUID(),
    orgId,
    name: body.name,
    baseUrl: body.baseUrl,
    createdAt: new Date().toISOString(),
  };
  qaMockStore.projects.set(project.id, project);
  return project;
}

// --- Runtimes -----------------------------------------------------------

/** Demo runtimes are seeded once per org on first read - there is no real T09 daemon to pair one for real yet. */
export function listRuntimes(orgId: string): Runtime[] {
  seedRuntimesOnce(orgId);
  return [...qaMockStore.runtimes.values()]
    .filter((entry) => entry.orgId === orgId)
    .map((entry) => entry.runtime);
}

export function getRuntime(orgId: string, runtimeId: string): Runtime | null {
  seedRuntimesOnce(orgId);
  const entry = qaMockStore.runtimes.get(runtimeId);
  return entry && entry.orgId === orgId ? entry.runtime : null;
}

function seedRuntimesOnce(orgId: string): void {
  if (qaMockStore.seededOrgRuntimes.has(orgId)) return;
  qaMockStore.seededOrgRuntimes.add(orgId);

  const online: Runtime = {
    id: `rt-${orgId}-online`,
    name: 'demo-macbook',
    online: true,
    lastSeenAt: new Date().toISOString(),
    browsers: ['chromium'],
    agents: [{ name: 'claude', ok: true, version: '1.0.0' }],
  };
  const offline: Runtime = {
    id: `rt-${orgId}-offline`,
    name: 'staging-runner',
    online: false,
    lastSeenAt: new Date(Date.now() - 45 * 60 * 1000).toISOString(),
    browsers: ['chromium', 'firefox'],
    agents: [{ name: 'claude', ok: false, error: 'runtime offline' }],
  };
  qaMockStore.runtimes.set(online.id, { orgId, runtime: online });
  qaMockStore.runtimes.set(offline.id, { orgId, runtime: offline });
}

// --- Runs -----------------------------------------------------------------

export function getStoredRun(orgId: string, runId: string): StoredRun | null {
  const stored = qaMockStore.runs.get(runId);
  return stored && stored.orgId === orgId ? stored : null;
}

export function createRun(orgId: string, body: CreateRunRequest): StoredRun | { errorCode: string } {
  const project = getProject(orgId, body.projectId);
  if (!project) return { errorCode: 'PROJECT_NOT_FOUND' };
  const runtime = getRuntime(orgId, body.runtimeId);
  if (!runtime) return { errorCode: 'RUNTIME_NOT_FOUND' };
  if (!runtime.online) return { errorCode: 'RUNTIME_OFFLINE' };

  const id = randomUUID();
  const now = Date.now();
  const { timeline, report } = buildRunTimeline(id, body.mode);
  const lastEntry = timeline[timeline.length - 1];
  const totalDurationMs = lastEntry ? lastEntry.offsetMs : 0;

  const run: Run = {
    id,
    projectId: body.projectId,
    runtimeId: body.runtimeId,
    mode: body.mode,
    status: 'running',
    phase: timeline[0]?.record.type === 'run.event' ? (timeline[0].record.payload as RunEventPayload).phase : null,
    counters: {},
    createdAt: new Date(now).toISOString(),
    startedAt: new Date(now).toISOString(),
  };

  const stored: StoredRun = {
    run,
    orgId,
    timeline,
    totalDurationMs,
    startedAtMs: now,
    cancelledAtMs: null,
    report,
  };
  qaMockStore.runs.set(id, stored);
  return stored;
}

export function cancelRun(orgId: string, runId: string): Run | null {
  const stored = getStoredRun(orgId, runId);
  if (!stored) return null;
  if (stored.cancelledAtMs === null && !isRunSettled(stored)) {
    stored.cancelledAtMs = Date.now();
  }
  return snapshotRun(stored);
}

/** Folds the timeline up to "now" (or the cancellation instant) into a status/phase/counters snapshot. */
export function snapshotRun(stored: StoredRun): Run {
  const elapsed = elapsedMs(stored);
  const dueEvents = stored.timeline.filter((entry) => entry.offsetMs <= elapsed);

  let counters: RunCounters = {};
  let phase = stored.run.phase;
  for (const entry of dueEvents) {
    if (entry.record.type === 'run.event') {
      const payload = entry.record.payload as RunEventPayload;
      counters = applyRunEventToCounters(counters, payload);
      phase = payload.phase;
    }
  }

  const resultRecord = dueEvents.find((e) => e.record.type === 'run.result');
  let status: Run['status'] = 'running';
  let finishedAt: string | undefined;
  if (stored.cancelledAtMs !== null) {
    status = 'cancelled';
    finishedAt = new Date(stored.cancelledAtMs).toISOString();
  } else if (resultRecord) {
    const payload = resultRecord.record.payload as RunResultPayload;
    status = payload.status === 'completed' ? 'completed' : payload.status === 'cancelled' ? 'cancelled' : 'failed';
    finishedAt = resultRecord.record.ts;
  }

  return { ...stored.run, status, phase, counters, finishedAt };
}

export function listRunEventsSince(stored: StoredRun, since: number): { events: RunEventRecord[]; nextSince: number } {
  const elapsed = elapsedMs(stored);
  const events = stored.timeline
    .filter((entry) => entry.offsetMs <= elapsed && entry.record.seq > since)
    .map((entry) => entry.record);
  const lastEvent = events[events.length - 1];
  const nextSince = lastEvent ? lastEvent.seq : since;
  return { events, nextSince };
}

export function getRunReport(stored: StoredRun): ReportResponse {
  if (!isRunSettled(stored)) return { executions: [], findings: [] };
  return stored.report;
}

export function findArtifact(orgId: string, artifactId: string): Artifact | null {
  for (const stored of qaMockStore.runs.values()) {
    if (stored.orgId !== orgId) continue;
    for (const execution of stored.report.executions) {
      const artifact = execution.artifacts.find((a) => a.id === artifactId);
      if (artifact) return artifact;
    }
  }
  return null;
}

/** Unscoped lookup for the /raw placeholder route, which a real presigned URL would serve without a session at all - see the route's own comment. */
export function findArtifactUnscoped(artifactId: string): Artifact | null {
  for (const stored of qaMockStore.runs.values()) {
    for (const execution of stored.report.executions) {
      const artifact = execution.artifacts.find((a) => a.id === artifactId);
      if (artifact) return artifact;
    }
  }
  return null;
}

function isRunSettled(stored: StoredRun): boolean {
  return snapshotRun(stored).status !== 'running';
}

function elapsedMs(stored: StoredRun): number {
  const cap = stored.cancelledAtMs ?? Date.now();
  return Math.max(0, cap - stored.startedAtMs);
}

// --- Deterministic timeline generator --------------------------------------

/** mulberry32 - deterministic, seeded PRNG so the same runId always regenerates the same timeline (needed for since= pagination to stay stable across requests). */
function mulberry32(seed: number): () => number {
  let a = seed;
  return () => {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function seedFromString(input: string): number {
  let hash = 0;
  for (let i = 0; i < input.length; i++) {
    hash = (Math.imul(31, hash) + input.charCodeAt(i)) | 0;
  }
  return hash;
}

const TEST_CASE_TITLES = [
  'Login with valid credentials',
  'Reject invalid password',
  'Create a new record',
  'Edit an existing record',
  'Delete a record',
  'Search filters the list',
  'Paginate past the first page',
  'Session expires after logout',
];

const EVENT_GAP_MS = 220;

function buildRunTimeline(runId: string, mode: RunMode): { timeline: TimelineEntry[]; report: ReportResponse } {
  const rand = mulberry32(seedFromString(runId));
  let seq = 1;
  let offsetMs = 0;
  const timeline: TimelineEntry[] = [];

  const pushEvent = (payload: RunEventPayload) => {
    offsetMs += EVENT_GAP_MS;
    timeline.push({
      offsetMs,
      record: { seq: seq++, type: 'run.event', ts: new Date().toISOString(), payload },
    });
  };

  const runDiscovery = mode === 'discover' || mode === 'plan' || mode === 'full';
  const runPlan = mode === 'plan' || mode === 'full';
  const runExecute = mode === 'execute' || mode === 'full';

  if (runDiscovery) {
    const discoveryCodes: Array<RunEventPayload['code']> = [
      'page_discovered',
      'page_discovered',
      'workflow_discovered',
      'form_discovered',
      'page_discovered',
      'action_discovered',
      'workflow_discovered',
      'form_discovered',
      'action_discovered',
      'page_discovered',
    ];
    for (const code of discoveryCodes) {
      pushEvent({ phase: 'discover', level: 'info', code, message: discoveryMessage(code) });
    }
  }

  const testCaseIds = TEST_CASE_TITLES.map((_, i) => `TC-${String(i + 1).padStart(3, '0')}`);

  if (runPlan) {
    pushEvent({
      phase: 'plan',
      level: 'info',
      code: 'test_plan_ready',
      message: `Generated ${testCaseIds.length} test cases`,
      data: { count: testCaseIds.length },
    });
  }

  const executions: ExecutionResult[] = [];
  const findings: Finding[] = [];

  if (runExecute) {
    testCaseIds.forEach((testCaseId, index) => {
      const title = TEST_CASE_TITLES[index] ?? testCaseId;
      pushEvent({ phase: 'execute', level: 'info', code: 'test_started', message: title, testCaseId });

      // Deterministic mix: mostly pass, one skip, a couple of fails so the failed-execution / artifact-viewer path always has something to click into.
      const roll = rand();
      const outcome: ExecutionResult['result'] = index === 3 ? 'skipped' : roll < 0.25 ? 'fail' : 'pass';
      const code = outcome === 'pass' ? 'test_passed' : outcome === 'skipped' ? 'test_skipped' : 'test_failed';
      pushEvent({
        phase: 'execute',
        level: outcome === 'fail' ? 'error' : 'info',
        code,
        message: outcome === 'fail' ? `${title}: assertion failed` : title,
        testCaseId,
      });

      const artifacts = buildArtifacts(runId, testCaseId, outcome);
      const startedAt = new Date().toISOString();
      const endedAt = new Date().toISOString();
      executions.push({
        version: 1,
        testCaseId,
        runId,
        result: outcome,
        steps: [
          { index: 0, action: 'navigate', status: 'pass' },
          { index: 1, action: 'click', status: 'pass' },
          {
            index: 2,
            action: 'waitFor',
            status: outcome === 'fail' ? 'fail' : outcome === 'skipped' ? 'skipped' : 'pass',
            artifactIds: artifacts.map((a) => a.id),
          },
        ],
        assertions: [{ index: 0, type: 'textEquals', status: outcome, artifactIds: artifacts.map((a) => a.id) }],
        artifacts,
        startedAt,
        endedAt,
      });

      if (outcome === 'fail') {
        pushEvent({
          phase: 'analyze',
          level: 'warn',
          code: 'finding_created',
          message: `Likely cause identified for ${testCaseId}`,
          testCaseId,
        });
        findings.push({
          version: 1,
          testCaseId,
          stepIndex: 2,
          failureClass: rand() < 0.5 ? 'PRODUCT_BUG' : 'TEST_BUG',
          rootCause: `${title} did not reach the expected state before the assertion ran.`,
          confidence: Math.round((0.7 + rand() * 0.25) * 100) / 100,
          evidence: artifacts.map((a) => a.id),
        });
      }
    });

    pushEvent({ phase: 'report', level: 'info', code: 'report_ready', message: 'Report ready' });
  }

  offsetMs += EVENT_GAP_MS;
  const resultPayload: RunResultPayload = { status: 'completed', executions, findings };
  timeline.push({
    offsetMs,
    record: { seq: seq++, type: 'run.result', ts: new Date().toISOString(), payload: resultPayload },
  });

  return { timeline, report: { executions, findings } };
}

function discoveryMessage(code: string): string {
  switch (code) {
    case 'page_discovered':
      return 'Discovered a new page';
    case 'workflow_discovered':
      return 'Discovered a workflow';
    case 'form_discovered':
      return 'Discovered a form';
    case 'action_discovered':
      return 'Discovered an action';
    default:
      return code;
  }
}

function buildArtifacts(runId: string, testCaseId: string, outcome: ExecutionResult['result']): Artifact[] {
  const day = new Date().toISOString().slice(0, 10);
  const prefix = `orgs/demo/runs/${day}/${runId}/${testCaseId}`;
  const kinds: ArtifactKind[] =
    outcome === 'fail' || outcome === 'error' ? ['screenshot', 'console', 'network', 'trace'] : ['screenshot'];
  const extensionByKind: Record<ArtifactKind, string> = {
    screenshot: 'png',
    trace: 'zip',
    video: 'webm',
    network: 'json',
    console: 'json',
    dom: 'html',
    report: 'json',
  };
  const contentTypeByKind: Record<ArtifactKind, string> = {
    screenshot: 'image/png',
    trace: 'application/zip',
    video: 'video/webm',
    network: 'application/json',
    console: 'application/json',
    dom: 'text/html',
    report: 'application/json',
  };
  return kinds.map((kind, i) => ({
    id: `art-${testCaseId}-${i}`,
    kind,
    key: `${prefix}/${kind}-${i}.${extensionByKind[kind]}`,
    contentType: contentTypeByKind[kind],
  }));
}
