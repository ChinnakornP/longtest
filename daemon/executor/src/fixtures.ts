/**
 * @fileoverview Precondition fixtures: `fixture:<name>` resolvers.
 *
 * A test case declares `preconditions: ["fixture:logged_in_as_admin"]`. The
 * planner is forbidden from putting credentials in the payload, so this
 * module is the only thing that knows them — they come from a server-side
 * fixture store the daemon hands us at run assign time. For T6 we ship the
 * default login fixture and an `unavailable` stub for the rest, which is
 * enough for the integration tests and the schema round-trip.
 */

import type { Page } from 'playwright';
import type { Precondition } from '@qa/schema';

export interface FixtureContext {
  baseUrl: string;
  /** Credentials the daemon injected for this run. */
  credentials: Readonly<Record<string, { username: string; password: string }>>;
}

export interface FixtureHandler {
  (page: Page, ctx: FixtureContext): Promise<void>;
}

const DEFAULT_FIXTURES: Readonly<Record<string, FixtureHandler>> = {
  'logged_in_as_admin': async (page, ctx) => {
    const creds = ctx.credentials['logged_in_as_admin'];
    if (!creds) {
      throw new Error('fixture:logged_in_as_admin requires credentials to be set on the run');
    }
    await page.goto(`${ctx.baseUrl.replace(/\/+$/, '')}/login`, { waitUntil: 'load' });
    await page.getByLabel('Email', { exact: true }).fill(creds.username);
    await page.getByLabel('Password', { exact: true }).fill(creds.password);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await page.waitForURL(/\/(employees|dashboard)/, { timeout: 15_000 });
  },
};

export async function establishFixtures(
  preconditions: readonly Precondition[] | undefined,
  page: Page,
  ctx: FixtureContext,
): Promise<void> {
  if (preconditions === undefined || preconditions.length === 0) return;
  for (const name of preconditions) {
    if (!name.startsWith('fixture:')) {
      throw new Error(`precondition must be "fixture:<name>", got ${name}`);
    }
    const key = name.slice('fixture:'.length);
    const handler = DEFAULT_FIXTURES[key];
    if (handler === undefined) {
      throw new FixtureUnavailableError(name);
    }
    await handler(page, ctx);
  }
}

/** Thrown when a precondition names a fixture the executor does not know. */
export class FixtureUnavailableError extends Error {
  readonly fixtureName: string;
  constructor(fixtureName: string) {
    super(`no fixture handler registered for ${fixtureName}`);
    this.fixtureName = fixtureName;
  }
}
