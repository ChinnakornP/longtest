/**
 * @fileoverview Evidence capture: screenshot, trace, network log, console log.
 *
 * The four artifact kinds the contract mandates. The runner drives capture
 * events (on failure and on every step's tail) and this module owns the
 * file system layout, the SHA-256s, and the `Artifact` records.
 */

import { createHash } from 'node:crypto';
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { statSync, readFileSync } from 'node:fs';
import type { Artifact, ArtifactId, ArtifactKind } from '@qa/schema';
import type { ConsoleMessage, Request as PlaywrightRequest } from 'playwright';
import type { ConsoleLevel } from './session.ts';

/** Counter used to mint run-local ArtifactId values that match the schema regex. */
function mintArtifactId(kind: ArtifactKind, index: number): ArtifactId {
  // The schema pattern is ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$. We compose
  // `kind-n` so the failure analyst can read the id and know the artifact
  // shape; the index gives uniqueness within a run.
  return `${kind}-${index}`;
}

export interface CapturedNetworkEntry {
  method: string;
  url: string;
  status: number | undefined;
  durationMs: number | undefined;
  startedAt: string;
}

export interface CapturedConsoleEntry {
  level: ConsoleLevel;
  text: string;
}

/**
 * State machine for one run. The runner holds one of these and calls into it
 * as steps and assertions happen.
 */
export class EvidenceCollector {
  private readonly artifactDir: string;
  private readonly storageKeyPrefix: string;
  private readonly testCaseId: string;
  private readonly artifacts: Artifact[] = [];
  private nextIndex = 0;
  private readonly captured: CapturedNetworkEntry[] = [];
  private readonly console: CapturedConsoleEntry[] = [];
  private tracePath: string | undefined;

  constructor(opts: {
    artifactDir: string;
    storageKeyPrefix: string;
    testCaseId: string;
    tracePath?: string;
  }) {
    this.artifactDir = opts.artifactDir;
    this.storageKeyPrefix = opts.storageKeyPrefix;
    this.testCaseId = opts.testCaseId;
    this.tracePath = opts.tracePath;
  }

  async init(): Promise<void> {
    await mkdir(this.artifactDir, { recursive: true });
  }

  setTracePath(path: string): void {
    this.tracePath = path;
  }

  /** Capture a network request, normalising the fields we report. */
  async recordNetwork(req: PlaywrightRequest): Promise<void> {
    const response = await req.response();
    const status = response === null ? undefined : response.status();
    const timing = req.timing();
    const startedAt = new Date().toISOString();
    this.captured.push({
      method: req.method(),
      url: req.url(),
      status,
      durationMs: timing === undefined ? undefined : Math.round(timing.responseEnd - timing.startTime),
      startedAt,
    });
  }

  recordConsole(msg: ConsoleMessage): void {
    const type = msg.type();
    const level: CapturedConsoleEntry['level'] = normalize(type);
    this.console.push({ level, text: msg.text() });
  }

  /** Network log so far. Frozen at writeNetworkArtifact time. */
  networkSnapshot(): readonly CapturedNetworkEntry[] {
    return this.captured.slice();
  }

  consoleSnapshot(): readonly CapturedConsoleEntry[] {
    return this.console.slice();
  }

  /** Capture a screenshot to disk and register the artifact. Returns the new id. */
  async captureScreenshot(page: import('playwright').Page, opts: { fullPage?: boolean; name?: string }): Promise<ArtifactId> {
    const id = mintArtifactId('screenshot', this.nextIndex++);
    const filename = opts.name ? `${sanitizeName(opts.name)}.png` : `${id}.png`;
    const fullPath = join(this.artifactDir, filename);
    const buffer = await page.screenshot({ fullPage: opts.fullPage ?? false });
    await writeFile(fullPath, buffer);
    const sha256 = sha256Hex(buffer);
    const size = buffer.byteLength;
    this.artifacts.push({
      id,
      kind: 'screenshot',
      key: this.key(filename),
      contentType: 'image/png',
      sizeBytes: size,
      sha256,
    });
    return id;
  }

  /** Flush the network log to disk and register the artifact. */
  async writeNetworkArtifact(): Promise<ArtifactId> {
    const id = mintArtifactId('network', this.nextIndex++);
    const filename = `${id}.json`;
    const fullPath = join(this.artifactDir, filename);
    const payload = JSON.stringify(this.captured, null, 2);
    await writeFile(fullPath, payload, 'utf8');
    this.artifacts.push({
      id,
      kind: 'network',
      key: this.key(filename),
      contentType: 'application/json',
      sizeBytes: statSize(fullPath),
      sha256: sha256File(fullPath),
    });
    return id;
  }

  /** Flush the console log to disk and register the artifact. */
  async writeConsoleArtifact(): Promise<ArtifactId> {
    const id = mintArtifactId('console', this.nextIndex++);
    const filename = `${id}.json`;
    const fullPath = join(this.artifactDir, filename);
    const payload = JSON.stringify(this.console, null, 2);
    await writeFile(fullPath, payload, 'utf8');
    this.artifacts.push({
      id,
      kind: 'console',
      key: this.key(filename),
      contentType: 'application/json',
      sizeBytes: statSize(fullPath),
      sha256: sha256File(fullPath),
    });
    return id;
  }

  /**
   * Register the Playwright trace as an artifact. The trace is closed by
   * Playwright at context-close time; we only register it after the file is
   * on disk so the artifact record can carry the SHA-256.
   */
  async registerTraceArtifact(): Promise<ArtifactId | undefined> {
    if (this.tracePath === undefined) return undefined;
    const id = mintArtifactId('trace', this.nextIndex++);
    const filename = 'trace.zip';
    const fullPath = join(this.tracePath, filename);
    const stats = safeStat(fullPath);
    const artifact: Artifact = {
      id,
      kind: 'trace',
      key: this.key(filename),
      contentType: 'application/zip',
      sizeBytes: stats?.size ?? 0,
    };
    if (stats) {
      artifact.sha256 = sha256File(fullPath);
    }
    this.artifacts.push(artifact);
    return id;
  }

  /** All artifacts registered so far, in the order they were captured. */
  artifactsList(): readonly Artifact[] {
    return this.artifacts.slice();
  }

  private key(filename: string): string {
    // Storage key pattern (execution-result.schema.json):
    //   orgs/{orgId}/runs/{YYYY-MM-DD}/{runId}/{testCaseId}/{filename}
    // The prefix the daemon gives us already encodes orgs/{orgId}/runs/.../{runId}/;
    // we add the testCaseId segment and the file name.
    return `${this.storageKeyPrefix.replace(/\/+$/, '')}/${this.testCaseId}/${filename}`;
  }
}

function sanitizeName(name: string): string {
  return name.replace(/[^A-Za-z0-9._-]/g, '_').slice(0, 80);
}

function sha256Hex(buffer: Buffer): string {
  return createHash('sha256').update(buffer).digest('hex');
}

function sha256File(fullPath: string): string {
  // Synchronous read is fine here — these files are small (network/console
  // JSON, a trace.zip that we only hash once at the end) and the executor is
  // already serialised per run.
  return sha256Hex(readFileSync(fullPath));
}

function statSize(fullPath: string): number {
  return statSync(fullPath).size;
}

function safeStat(fullPath: string): import('node:fs').Stats | undefined {
  try {
    return statSync(fullPath);
  } catch {
    return undefined;
  }
}

function normalize(type: string): CapturedConsoleEntry['level'] {
  switch (type) {
    case 'warning':
      return 'warn';
    case 'verbose':
      return 'debug';
    case 'log':
    case 'info':
    case 'error':
    case 'debug':
      return type as CapturedConsoleEntry['level'];
    default:
      return 'log';
  }
}
