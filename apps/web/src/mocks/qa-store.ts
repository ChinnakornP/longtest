import { randomUUID } from 'node:crypto';

import type {
  ApplicationMap,
  Artifact,
  ArtifactKind,
  ExecutionResult,
  Finding,
  RunEventPayload,
  RunResultPayload,
  TestCase as TestCasePayload,
} from '@qa/schema';

import type {
  CoverageReport,
  CreateProjectRequest,
  CreateRunRequest,
  Fixture,
  Project,
  ReportResponse,
  Run,
  RunCounters,
  RunEventRecord,
  RunMode,
  Runtime,
  TestCaseRecord,
  TestCaseStatus,
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
  testCases = new Map<string, TestCaseRecord>();
  fixturesByProject = new Map<string, Fixture[]>();
  seededProjectTestCases = new Set<string>();
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

// --- Test cases -------------------------------------------------------

/** Seeds a fixed demo suite for a project on first read — there is no real T14 planner run to have generated one yet. */
function seedTestCasesOnce(projectId: string): void {
  if (qaMockStore.seededProjectTestCases.has(projectId)) return;
  qaMockStore.seededProjectTestCases.add(projectId);

  const now = new Date().toISOString();
  const seeds: Array<{
    ref: string;
    name: string;
    priority: TestCaseRecord['priority'];
    category: TestCaseRecord['category'];
    status: TestCaseStatus;
    payload: TestCasePayload;
  }> = [
    {
      ref: 'TC-001',
      name: 'Login with valid credentials',
      priority: 'critical',
      category: 'functional',
      status: 'approved',
      payload: {
        version: 1,
        id: 'TC-001',
        name: 'Login with valid credentials',
        description: 'A user with a valid account can sign in and reach the dashboard.',
        priority: 'critical',
        category: 'functional',
        preconditions: ['fixture:logged_in_as_admin'],
        steps: [
          { action: 'navigate', url: '/login' },
          { action: 'fill', target: { ref: 'login.email' }, value: 'admin@example.com' },
          { action: 'fill', target: { ref: 'login.password' }, value: 'REDACTED' },
          { action: 'click', target: { ref: 'login.submit' } },
        ],
        assertions: [{ type: 'urlMatches', value: '^/dashboard' }, { type: 'visible', target: { ref: 'dashboard.heading' } }],
      },
    },
    {
      ref: 'TC-002',
      name: 'Reject invalid password',
      priority: 'high',
      category: 'validation',
      status: 'approved',
      payload: {
        version: 1,
        id: 'TC-002',
        name: 'Reject invalid password',
        priority: 'high',
        category: 'validation',
        steps: [
          { action: 'navigate', url: '/login' },
          { action: 'fill', target: { ref: 'login.email' }, value: 'admin@example.com' },
          { action: 'fill', target: { ref: 'login.password' }, value: 'wrong-password' },
          { action: 'click', target: { ref: 'login.submit' } },
        ],
        assertions: [{ type: 'textContains', target: { ref: 'login.error' }, value: 'Invalid credentials' }],
      },
    },
    {
      ref: 'TC-003',
      name: 'Create a new record',
      priority: 'high',
      category: 'functional',
      status: 'draft',
      payload: {
        version: 1,
        id: 'TC-003',
        name: 'Create a new record',
        priority: 'high',
        category: 'functional',
        preconditions: ['fixture:logged_in_as_admin'],
        steps: [
          { action: 'navigate', url: '/records/new' },
          { action: 'fill', target: { ref: 'records.name' }, value: 'Test record' },
          { action: 'click', target: { ref: 'records.save' } },
        ],
        assertions: [{ type: 'textContains', target: { ref: 'records.list' }, value: 'Test record' }],
      },
    },
    {
      ref: 'TC-004',
      name: 'Edit an existing record',
      priority: 'medium',
      category: 'functional',
      status: 'draft',
      payload: {
        version: 1,
        id: 'TC-004',
        name: 'Edit an existing record',
        priority: 'medium',
        category: 'functional',
        preconditions: ['fixture:logged_in_as_admin'],
        steps: [
          { action: 'navigate', url: '/records/1/edit' },
          { action: 'fill', target: { ref: 'records.name' }, value: 'Updated record' },
          { action: 'click', target: { ref: 'records.save' } },
        ],
        assertions: [{ type: 'textContains', target: { ref: 'records.list' }, value: 'Updated record' }],
      },
    },
    {
      ref: 'TC-005',
      name: 'Delete a record',
      priority: 'medium',
      category: 'functional',
      status: 'draft',
      payload: {
        version: 1,
        id: 'TC-005',
        name: 'Delete a record',
        priority: 'medium',
        category: 'functional',
        preconditions: ['fixture:logged_in_as_admin'],
        steps: [
          { action: 'navigate', url: '/records/1' },
          { action: 'click', target: { ref: 'records.delete' } },
        ],
        assertions: [{ type: 'hidden', target: { ref: 'records.row_1' } }],
      },
    },
    {
      ref: 'TC-006',
      name: 'Search filters the list',
      priority: 'low',
      category: 'ui_behavior',
      status: 'archived',
      payload: {
        version: 1,
        id: 'TC-006',
        name: 'Search filters the list',
        priority: 'low',
        category: 'ui_behavior',
        steps: [
          { action: 'navigate', url: '/records' },
          { action: 'fill', target: { ref: 'records.search' }, value: 'Test' },
        ],
        assertions: [{ type: 'elementCount', target: { ref: 'records.row' }, value: 1, operator: 'gte' }],
      },
    },
  ];

  for (const seed of seeds) {
    const id = randomUUID();
    const record: TestCaseRecord = {
      id,
      projectId,
      ref: seed.ref,
      name: seed.name,
      priority: seed.priority,
      category: seed.category,
      status: seed.status,
      version: 1,
      payload: seed.payload,
      createdAt: now,
      updatedAt: now,
    };
    qaMockStore.testCases.set(id, record);
  }
}

export function listTestCases(
  orgId: string,
  projectId: string,
  status?: string,
): { testCases: TestCaseRecord[]; total: number } | null {
  const project = getProject(orgId, projectId);
  if (!project) return null;
  seedTestCasesOnce(projectId);

  const all = [...qaMockStore.testCases.values()]
    .filter((tc) => tc.projectId === projectId)
    .filter((tc) => !status || tc.status === status)
    .sort((a, b) => a.ref.localeCompare(b.ref));
  return { testCases: all, total: all.length };
}

export function getTestCase(orgId: string, testCaseId: string): TestCaseRecord | null {
  const record = qaMockStore.testCases.get(testCaseId);
  if (!record) return null;
  return getProject(orgId, record.projectId) ? record : null;
}

const NEXT_STATUSES: Record<TestCaseStatus, TestCaseStatus[]> = {
  draft: ['approved', 'archived'],
  approved: ['archived', 'draft'],
  archived: ['draft'],
};

export function setTestCaseStatus(
  orgId: string,
  testCaseId: string,
  status: string,
): TestCaseRecord | { errorCode: 'NOT_FOUND' | 'INVALID_TRANSITION' } {
  const record = getTestCase(orgId, testCaseId);
  if (!record) return { errorCode: 'NOT_FOUND' };
  if (!NEXT_STATUSES[record.status].includes(status as TestCaseStatus) && status !== record.status) {
    return { errorCode: 'INVALID_TRANSITION' };
  }
  record.status = status as TestCaseStatus;
  record.updatedAt = new Date().toISOString();
  return record;
}

// --- Coverage -----------------------------------------------------------

/** A canned but internally-consistent report: `partial` is deliberately distinct from `covered`, never a lower percentage of the same color. */
export function getCoverage(orgId: string, projectId: string): CoverageReport | null {
  const project = getProject(orgId, projectId);
  if (!project) return null;
  seedTestCasesOnce(projectId);

  const approvedCount = [...qaMockStore.testCases.values()].filter(
    (tc) => tc.projectId === projectId && tc.status === 'approved',
  ).length;

  return {
    projectId,
    approvedCases: approvedCount,
    workflows: [
      {
        ref: 'wf.login',
        name: 'Sign in',
        expectedOutcome: 'User reaches the dashboard',
        status: 'covered',
        coverageRatio: 1,
        coveringCaseRefs: ['TC-001'],
        authRequired: false,
        risk: 'high',
        suggestedTests: 0,
      },
      {
        ref: 'wf.record_lifecycle',
        name: 'Create, edit and delete a record',
        expectedOutcome: 'A record can be created, edited and removed',
        status: 'partial',
        coverageRatio: 0.33,
        coveringCaseRefs: ['TC-003'],
        missingRefs: ['records.edit', 'records.delete'],
        authRequired: true,
        risk: 'medium',
        suggestedTests: 2,
        suggestions: [
          { category: 'functional', reason: 'No approved case walks edit through to a saved change.' },
          { category: 'error_handling', reason: 'No case checks what happens when delete is cancelled.' },
        ],
      },
    ],
    pages: [
      {
        ref: 'page.settings',
        path: '/settings',
        title: 'Settings',
        status: 'uncovered',
        authRequired: true,
        risk: 'medium',
        suggestedTests: 1,
        suggestions: [{ category: 'functional', reason: 'No case reaches the settings page at all.' }],
      },
    ],
    categories: [
      { category: 'functional', approved: approvedCount, suggestedTests: 2 },
      { category: 'validation', approved: 1, suggestedTests: 0 },
      { category: 'navigation', approved: 0, suggestedTests: 1 },
      { category: 'ui_behavior', approved: 0, suggestedTests: 0 },
      { category: 'error_handling', approved: 0, suggestedTests: 1 },
    ],
    suggestedTestCount: 4,
    summary: `${approvedCount} approved case(s) cover 1 of 2 workflows and 0 of 1 other discovered page; 4 more tests are suggested to close the gap.`,
  };
}

// --- Fixtures -------------------------------------------------------------

export function listFixtures(orgId: string, projectId: string): Fixture[] | null {
  const project = getProject(orgId, projectId);
  if (!project) return null;
  return qaMockStore.fixturesByProject.get(projectId) ?? [];
}

export function registerFixture(
  orgId: string,
  projectId: string,
  name: string,
  description: string,
): Fixture | { errorCode: 'NOT_FOUND' } {
  const project = getProject(orgId, projectId);
  if (!project) return { errorCode: 'NOT_FOUND' };

  const existing = qaMockStore.fixturesByProject.get(projectId) ?? [];
  const already = existing.find((f) => f.name === name);
  const fixture: Fixture = {
    name,
    reference: `fixture:${name}`,
    description: description || undefined,
    createdAt: already?.createdAt ?? new Date().toISOString(),
  };
  qaMockStore.fixturesByProject.set(projectId, [...existing.filter((f) => f.name !== name), fixture]);
  return fixture;
}

export function deleteFixture(orgId: string, projectId: string, name: string): boolean {
  const project = getProject(orgId, projectId);
  if (!project) return false;
  const existing = qaMockStore.fixturesByProject.get(projectId) ?? [];
  qaMockStore.fixturesByProject.set(projectId, existing.filter((f) => f.name !== name));
  return true;
}

// --- Application map -------------------------------------------------------

/** Just enough elements to give the seeded test cases' target refs a human label — not a stand-in for the T13/T14 crawler. */
export function getApplicationMap(orgId: string, projectId: string): ApplicationMap | null {
  const project = getProject(orgId, projectId);
  if (!project) return null;

  return {
    version: 1,
    baseUrl: project.baseUrl,
    projectId,
    pages: [
      {
        id: 'page.login',
        path: '/login',
        title: 'Sign in',
        elements: [
          { ref: 'login.email', type: 'input', label: 'Email address', locators: [], lastSeenRunId: 'seed' },
          { ref: 'login.password', type: 'input', label: 'Password', locators: [], lastSeenRunId: 'seed' },
          { ref: 'login.submit', type: 'button', label: 'Sign in', locators: [], lastSeenRunId: 'seed' },
          { ref: 'login.error', type: 'other', label: 'Sign-in error message', locators: [], lastSeenRunId: 'seed' },
        ],
      },
      {
        id: 'page.dashboard',
        path: '/dashboard',
        title: 'Dashboard',
        authRequired: true,
        elements: [{ ref: 'dashboard.heading', type: 'other', label: 'Dashboard heading', locators: [], lastSeenRunId: 'seed' }],
      },
      {
        id: 'page.records',
        path: '/records',
        title: 'Records',
        authRequired: true,
        elements: [
          { ref: 'records.name', type: 'input', label: 'Record name', locators: [], lastSeenRunId: 'seed' },
          { ref: 'records.save', type: 'button', label: 'Save record', locators: [], lastSeenRunId: 'seed' },
          { ref: 'records.delete', type: 'button', label: 'Delete record', locators: [], lastSeenRunId: 'seed' },
          { ref: 'records.list', type: 'other', label: 'Records list', locators: [], lastSeenRunId: 'seed' },
          { ref: 'records.row_1', type: 'other', label: 'First record row', locators: [], lastSeenRunId: 'seed' },
          { ref: 'records.row', type: 'other', label: 'Record row', locators: [], lastSeenRunId: 'seed' },
          { ref: 'records.search', type: 'input', label: 'Search records', locators: [], lastSeenRunId: 'seed' },
        ],
      },
    ],
    workflows: [
      { id: 'wf.login', name: 'Sign in', path: ['page.login', 'login.submit', 'page.dashboard'], expectedOutcome: 'User reaches the dashboard' },
      {
        id: 'wf.record_lifecycle',
        name: 'Create, edit and delete a record',
        path: ['page.records', 'records.save', 'page.records', 'records.delete'],
        expectedOutcome: 'A record can be created, edited and removed',
        authRequired: true,
      },
    ],
  };
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
