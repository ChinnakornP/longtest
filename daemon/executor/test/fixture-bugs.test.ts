/**
 * @fileoverview The fixture app's injected defects behave as documented.
 *
 * The Failure Analyst is scored against these: a classifier can only be
 * measured on failures whose true cause somebody already knows. That makes the
 * defects themselves a contract — if `create-500` quietly stops returning 500,
 * every analyst benchmark built on it goes green for the wrong reason and
 * nothing says so.
 *
 * Driven over HTTP rather than through a browser. What is being asserted is
 * the app's behaviour, and requiring chromium would skip this on every machine
 * that has not run `playwright install`.
 */

import { afterEach, describe, expect, it } from 'vitest';

import { FixtureApp, FIXTURE_PASSWORD, FIXTURE_USER, fixtureAvailable } from './fixture-app.ts';

/** Signs in and returns the session cookie header. */
async function signIn(app: FixtureApp): Promise<string> {
  const response = await fetch(app.url('/login'), {
    method: 'POST',
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ email: FIXTURE_USER, password: FIXTURE_PASSWORD }),
    redirect: 'manual',
  });
  expect(response.status).toBe(302);
  const cookie = response.headers.get('set-cookie');
  expect(cookie).not.toBeNull();
  return (cookie as string).split(';')[0] as string;
}

async function createEmployee(
  app: FixtureApp,
  cookie: string,
  fields: { firstName: string; lastName: string; email: string },
): Promise<Response> {
  return fetch(app.url('/employees'), {
    method: 'POST',
    headers: { cookie, 'content-type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams(fields),
    redirect: 'manual',
  });
}

async function listPage(app: FixtureApp, cookie: string): Promise<string> {
  const response = await fetch(app.url('/employees'), { headers: { cookie } });
  expect(response.status).toBe(200);
  return response.text();
}

describe.skipIf(!fixtureAvailable())('fixture app: injected defects', () => {
  let app: FixtureApp | undefined;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
  });

  async function start(bugs: readonly string[]): Promise<FixtureApp> {
    app = new FixtureApp({ bugs, requiresBrowser: false });
    await app.start();
    return app;
  }

  it('is honest with no bugs injected', async () => {
    const fixture = await start([]);
    const cookie = await signIn(fixture);

    const created = await createEmployee(fixture, cookie, {
      firstName: 'Ada',
      lastName: 'Lovelace',
      email: 'ada@example.test',
    });
    expect(created.status).toBe(302);
    expect(await listPage(fixture, cookie)).toContain('Ada Lovelace');
  });

  it('create-500 answers 500 and stores nothing', async () => {
    const fixture = await start(['create-500']);
    const cookie = await signIn(fixture);

    const created = await createEmployee(fixture, cookie, {
      firstName: 'Ada',
      lastName: 'Lovelace',
      email: 'ada@example.test',
    });

    // A 5xx in the network log is what makes this the loud case: the analyst
    // does not have to reason about the page to find the cause.
    expect(created.status).toBe(500);
    expect(await listPage(fixture, cookie)).not.toContain('Ada Lovelace');
  });

  it('create-500 fails after validation, not instead of it', async () => {
    // The defect must not swallow the validation path, or a validation test
    // case fails for the injected reason and the benchmark measures nothing.
    const fixture = await start(['create-500']);
    const cookie = await signIn(fixture);

    const empty = await createEmployee(fixture, cookie, { firstName: '', lastName: '', email: '' });
    expect(empty.status).toBe(422);
    expect(await empty.text()).toContain('All fields are required');
  });

  it('edit-not-synced saves the write and shows the list a stale one', async () => {
    const fixture = await start(['edit-not-synced']);
    const cookie = await signIn(fixture);

    const created = await createEmployee(fixture, cookie, {
      firstName: 'Ada',
      lastName: 'Lovelace',
      email: 'ada@example.test',
    });
    const location = created.headers.get('location');
    expect(location).not.toBeNull();

    const updated = await fetch(fixture.url(location as string), {
      method: 'POST',
      headers: { cookie, 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ firstName: 'Augusta', lastName: 'King', email: 'augusta@example.test' }),
      redirect: 'manual',
    });
    // Every request in this flow is a success. That is the whole difficulty:
    // there is no status code to read the cause off, and the only evidence is
    // one assertion on the list disagreeing with what was just saved.
    expect(updated.status).toBe(302);

    const detail = await fetch(fixture.url(location as string), { headers: { cookie } });
    expect(await detail.text()).toContain('Augusta King');

    const list = await listPage(fixture, cookie);
    expect(list).toContain('Ada Lovelace');
    expect(list).not.toContain('Augusta King');
  });

  it('edit stays in step when the bug is off', async () => {
    const fixture = await start([]);
    const cookie = await signIn(fixture);

    const created = await createEmployee(fixture, cookie, {
      firstName: 'Ada',
      lastName: 'Lovelace',
      email: 'ada@example.test',
    });
    const location = created.headers.get('location') as string;
    await fetch(fixture.url(location), {
      method: 'POST',
      headers: { cookie, 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ firstName: 'Augusta', lastName: 'King', email: 'augusta@example.test' }),
      redirect: 'manual',
    });

    const list = await listPage(fixture, cookie);
    expect(list).toContain('Augusta King');
    expect(list).not.toContain('Ada Lovelace');
  });
});
