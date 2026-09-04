/**
 * @fileoverview One Playwright session per executor process.
 *
 * The daemon owns the executor's lifecycle, so the executor keeps a single
 * browser context open between `session.open` and `session.close`. The
 * runner borrows the `page` from here and returns it when it's done — the
 * same page is reused across `testcase.run` calls so cookies, storage and
 * the network capture survive between cases.
 */

import { chromium, type BrowserContext, type ConsoleMessage, type Page } from 'playwright';

export interface SessionOptions {
  baseUrl: string;
  viewport?: { width: number; height: number };
  storageState?: unknown;
  locale?: string;
  timezoneId?: string;
}

/** Console levels we surface to the Failure Analyst. Everything else is bucketed. */
export type ConsoleLevel = 'log' | 'warn' | 'error' | 'info' | 'debug';

export interface CapturedConsoleMessage {
  level: ConsoleLevel;
  text: string;
}

function normalizeConsoleLevel(type: string): ConsoleLevel {
  switch (type) {
    case 'warning':
      return 'warn';
    case 'verbose':
      return 'debug';
    case 'log':
    case 'info':
    case 'error':
    case 'debug':
      return type as ConsoleLevel;
    default:
      return 'log';
  }
}

export class Session {
  private context: BrowserContext | undefined;
  private page: Page | undefined;
  readonly baseUrl: string;
  /** Path to the directory Playwright writes trace.zip to. */
  readonly traceDir: string;
  /** Captured network requests, in arrival order. */
  readonly requests: import('playwright').Request[] = [];
  /** Captured console messages, in arrival order. */
  readonly consoleMessages: CapturedConsoleMessage[] = [];
  /** Storage state captured at close time so the daemon can persist it. */
  storageState: unknown;

  constructor(opts: { baseUrl: string; traceDir: string }) {
    this.baseUrl = opts.baseUrl;
    this.traceDir = opts.traceDir;
  }

  async open(opts: SessionOptions): Promise<void> {
    if (this.context !== undefined) {
      throw new Error('session already open');
    }
    // We launch a fresh browser context each time so the storageState from
    // the daemon can be applied via newContext({ storageState }). Persistent
    // context reads from disk, which we do not want: it cannot be told
    // "ignore whatever is in user-data-dir and use *this* blob instead".
    const browser = await chromium.launch({
      headless: true,
      args: ['--disable-features=Translate,PasswordManager'],
    });
    this.context = await browser.newContext({
      viewport: opts.viewport ?? { width: 1280, height: 720 },
      locale: opts.locale ?? 'en-US',
      timezoneId: opts.timezoneId ?? 'UTC',
      ...(opts.storageState !== undefined ? { storageState: opts.storageState as never } : {}),
    });
    await this.context.tracing.start({ screenshots: true, snapshots: true });

    const page = this.context.pages()[0] ?? (await this.context.newPage());
    this.page = page;
    this.attachListeners(page);
  }

  private attachListeners(page: Page): void {
    page.on('request', (req) => this.requests.push(req));
  page.on('console', (msg: ConsoleMessage) => {
    this.consoleMessages.push({ level: normalizeConsoleLevel(msg.type()), text: msg.text() });
  });
  }

  getPage(): Page {
    if (this.page === undefined) throw new Error('session not open');
    return this.page;
  }

  getRequests(): readonly import('playwright').Request[] {
    return this.requests;
  }

  getConsoleMessages(): readonly CapturedConsoleMessage[] {
    return this.consoleMessages;
  }

  currentUrl(): string {
    if (this.page === undefined) return '';
    return this.page.url();
  }

  async captureStorageState(): Promise<void> {
    if (this.context === undefined) return;
    this.storageState = await this.context.storageState();
  }

  /**
   * Stop Playwright tracing and write `trace.zip` into the session's
   * traceDir. Call this *before* `close()` so the trace file is on disk
   * while the runner still has a chance to register it as an artifact.
   * Calling `close()` afterwards tears down the browser.
   */
  async finalizeTrace(): Promise<string> {
    if (this.context === undefined) return `${this.traceDir}/trace.zip`;
    const path = `${this.traceDir}/trace.zip`;
    try {
      await this.context.tracing.stop({ path });
    } catch {
      // tracing.stop fails if start was never called; ignore.
    }
    return path;
  }

  async close(): Promise<void> {
    if (this.context === undefined) return;
    try {
      await this.captureStorageState();
    } catch {
      // Same reasoning: a failed storageState() should not prevent close.
    }
    await this.context.close();
    const browser = this.context.browser();
    await browser?.close().catch(() => undefined);
    this.context = undefined;
    this.page = undefined;
  }

  /** Whether `session.open` was called and not yet `session.close`d. */
  isOpen(): boolean {
    return this.context !== undefined;
  }
}
