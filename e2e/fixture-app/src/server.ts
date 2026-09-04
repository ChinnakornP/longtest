/**
 * Fixture web app — a deliberately tiny CRUD app used as the target of the
 * qa-executor integration tests.
 *
 * It exposes:
 *   GET  /login          form (email + password)
 *   POST /login          sets a cookie and redirects to /employees
 *   GET  /employees      list with search + Add Employee button
 *   POST /employees      create a row
 *   GET  /employees/new  form
 *   GET  /employees/:id  read-only row detail
 *   GET  /employees/:id/edit  edit form
 *   POST /employees/:id       update (PUT-style field on the form)
 *   POST /employees/:id/delete  delete
 *
 * State lives in memory. Every restart is a clean state.
 *
 * Why Node's http module and not Express: the executor does not care about
 * the framework, and we want zero `node_modules` between this app and CI.
 */
import { createServer, IncomingMessage, ServerResponse } from 'node:http';
import { URL } from 'node:url';
import { randomUUID } from 'node:crypto';

interface Employee {
  id: string;
  firstName: string;
  lastName: string;
  email: string;
  createdAt: string;
}

interface Session {
  cookies: Record<string, string>;
}

const PORT = Number(process.env['FIXTURE_PORT'] ?? 0);
const USERNAME = process.env['FIXTURE_USER'] ?? 'admin@example.test';
const PASSWORD = process.env['FIXTURE_PASSWORD'] ?? 'letmein';
const COOKIE_NAME = 'fixture_sid';

const employees: Employee[] = [];
const sessions = new Map<string, Session>();

function parseCookies(header: string | undefined): Record<string, string> {
  if (header === undefined) return {};
  const out: Record<string, string> = {};
  for (const part of header.split(';')) {
    const eq = part.indexOf('=');
    if (eq === -1) continue;
    const k = part.slice(0, eq).trim();
    const v = part.slice(eq + 1).trim();
    if (k.length > 0) out[k] = decodeURIComponent(v);
  }
  return out;
}

function isAuthenticated(req: IncomingMessage): boolean {
  const cookies = parseCookies(req.headers.cookie);
  const sid = cookies[COOKIE_NAME];
  if (sid === undefined) return false;
  return sessions.has(sid);
}

function send(res: ServerResponse, status: number, body: string, headers: Record<string, string> = {}): void {
  res.writeHead(status, headers);
  res.end(body);
}

function html(body: string): string {
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Fixture</title></head>
<body>
${body}
</body></html>`;
}

function readBody(req: IncomingMessage): Promise<URLSearchParams> {
  return new Promise((resolve) => {
    let buf = '';
    req.on('data', (chunk) => { buf += chunk.toString('utf8'); });
    req.on('end', () => resolve(new URLSearchParams(buf)));
  });
}

function escapeHtml(value: string): string {
  return value.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

const server = createServer(async (req, res) => {
  if (req.url === undefined || req.method === undefined) {
    send(res, 400, 'bad request');
    return;
  }
  const url = new URL(req.url, `http://localhost`);

  try {
    if (req.method === 'GET' && url.pathname === '/') {
      send(res, 200, html('<p>Fixture app</p><a href="/login">Sign in</a>'));
      return;
    }
    if (req.method === 'GET' && url.pathname === '/login') {
      send(res, 200, html(`
<h1>Sign in</h1>
<form method="POST" action="/login">
  <p><label>Email <input name="email" type="email" data-testid="login-email" required></label></p>
  <p><label>Password <input name="password" type="password" data-testid="login-password" required></label></p>
  <p><button type="submit" data-testid="login-submit">Sign in</button></p>
</form>
`), { 'content-type': 'text/html; charset=utf-8' });
      return;
    }
    if (req.method === 'POST' && url.pathname === '/login') {
      const body = await readBody(req);
      const email = body.get('email') ?? '';
      const password = body.get('password') ?? '';
      if (email !== USERNAME || password !== PASSWORD) {
        send(res, 401, html(`<p data-testid="login-error" role="alert">Invalid credentials</p>`), { 'content-type': 'text/html; charset=utf-8' });
        return;
      }
      const sid = randomUUID();
      sessions.set(sid, { cookies: {} });
      send(res, 302, '', {
        location: '/employees',
        'set-cookie': `${COOKIE_NAME}=${encodeURIComponent(sid)}; Path=/; HttpOnly`,
      });
      return;
    }
    if (!isAuthenticated(req)) {
      send(res, 302, '', { location: '/login' });
      return;
    }

    if (req.method === 'POST' && url.pathname === '/logout') {
      const cookies = parseCookies(req.headers.cookie);
      const sid = cookies[COOKIE_NAME];
      if (sid !== undefined) sessions.delete(sid);
      send(res, 302, '', { location: '/login', 'set-cookie': `${COOKIE_NAME}=; Path=/; Max-Age=0` });
      return;
    }

    if (req.method === 'GET' && url.pathname === '/employees') {
      const q = url.searchParams.get('q') ?? '';
      const filtered = q === '' ? employees : employees.filter((e) => `${e.firstName} ${e.lastName}`.toLowerCase().includes(q.toLowerCase()));
      const rows = filtered.map((e) => `
<tr data-testid="employee-row" data-id="${escapeHtml(e.id)}">
  <td>${escapeHtml(e.firstName)} ${escapeHtml(e.lastName)}</td>
  <td>${escapeHtml(e.email)}</td>
  <td>
    <a href="/employees/${escapeHtml(e.id)}/edit" data-testid="employee-edit">Edit</a>
    <form method="POST" action="/employees/${escapeHtml(e.id)}/delete" style="display:inline">
      <button type="submit" data-testid="employee-delete">Delete</button>
    </form>
  </td>
</tr>`).join('');
      send(res, 200, html(`
<h1>Employees</h1>
<form method="GET"><input name="q" value="${escapeHtml(q)}" placeholder="Search" data-testid="employee-search"><button data-testid="employee-search-go" type="submit">Search</button></form>
<p><a href="/employees/new" role="button" data-testid="add-emp">Add Employee</a></p>
<table data-testid="employee-table">
  <thead><tr><th>Name</th><th>Email</th><th>Actions</th></tr></thead>
  <tbody>${rows}</tbody>
</table>
<p><a href="/logout" data-testid="logout">Sign out</a></p>
`), { 'content-type': 'text/html; charset=utf-8' });
      return;
    }
    if (req.method === 'GET' && url.pathname === '/employees/new') {
      send(res, 200, html(`
<h1>Add Employee</h1>
<form method="POST" action="/employees">
  <p><label>First name <input name="firstName" data-testid="employee-first-name" required></label></p>
  <p><label>Last name <input name="lastName" data-testid="employee-last-name" required></label></p>
  <p><label>Email <input name="email" type="email" data-testid="employee-email" required></label></p>
  <p><button type="submit" data-testid="employee-save">Save</button> <a href="/employees">Cancel</a></p>
</form>
`), { 'content-type': 'text/html; charset=utf-8' });
      return;
    }
    if (req.method === 'POST' && url.pathname === '/employees') {
      const body = await readBody(req);
      const firstName = (body.get('firstName') ?? '').trim();
      const lastName = (body.get('lastName') ?? '').trim();
      const email = (body.get('email') ?? '').trim();
      if (firstName === '' || lastName === '' || email === '') {
        send(res, 422, html(`<p data-testid="employee-error" role="alert">All fields are required</p>`), { 'content-type': 'text/html; charset=utf-8' });
        return;
      }
      if (employees.some((e) => e.email.toLowerCase() === email.toLowerCase())) {
        send(res, 422, html(`<p data-testid="employee-error" role="alert">Email already exists</p>`), { 'content-type': 'text/html; charset=utf-8' });
        return;
      }
      const created: Employee = { id: randomUUID(), firstName, lastName, email, createdAt: new Date().toISOString() };
      employees.push(created);
      send(res, 302, '', { location: `/employees/${created.id}` });
      return;
    }
    const editMatch = /^\/employees\/([\w-]+)\/edit$/.exec(url.pathname);
    if (req.method === 'GET' && editMatch !== null) {
      const id = editMatch[1] as string;
      const emp = employees.find((e) => e.id === id);
      if (emp === undefined) {
        send(res, 404, html('<p>Not found</p>'), { 'content-type': 'text/html; charset=utf-8' });
        return;
      }
      send(res, 200, html(`
<h1>Edit Employee</h1>
<form method="POST" action="/employees/${escapeHtml(emp.id)}">
  <p><label>First name <input name="firstName" value="${escapeHtml(emp.firstName)}" data-testid="employee-first-name" required></label></p>
  <p><label>Last name <input name="lastName" value="${escapeHtml(emp.lastName)}" data-testid="employee-last-name" required></label></p>
  <p><label>Email <input name="email" type="email" value="${escapeHtml(emp.email)}" data-testid="employee-email" required></label></p>
  <p><button type="submit" data-testid="employee-save">Save</button> <a href="/employees">Cancel</a></p>
</form>
`), { 'content-type': 'text/html; charset=utf-8' });
      return;
    }
    const updateMatch = /^\/employees\/([\w-]+)$/.exec(url.pathname);
    if (req.method === 'POST' && updateMatch !== null) {
      const id = updateMatch[1] as string;
      const emp = employees.find((e) => e.id === id);
      if (emp === undefined) {
        send(res, 404, html('<p>Not found</p>'), { 'content-type': 'text/html; charset=utf-8' });
        return;
      }
      const body = await readBody(req);
      const firstName = (body.get('firstName') ?? '').trim();
      const lastName = (body.get('lastName') ?? '').trim();
      const email = (body.get('email') ?? '').trim();
      if (firstName !== '') emp.firstName = firstName;
      if (lastName !== '') emp.lastName = lastName;
      if (email !== '') emp.email = email;
      send(res, 302, '', { location: `/employees/${emp.id}` });
      return;
    }
    const deleteMatch = /^\/employees\/([\w-]+)\/delete$/.exec(url.pathname);
    if (req.method === 'POST' && deleteMatch !== null) {
      const id = deleteMatch[1] as string;
      const i = employees.findIndex((e) => e.id === id);
      if (i === -1) {
        send(res, 404, html('<p>Not found</p>'), { 'content-type': 'text/html; charset=utf-8' });
        return;
      }
      employees.splice(i, 1);
      send(res, 302, '', { location: '/employees' });
      return;
    }
    const detailMatch = /^\/employees\/([\w-]+)$/.exec(url.pathname);
    if (req.method === 'GET' && detailMatch !== null) {
      const id = detailMatch[1] as string;
      const emp = employees.find((e) => e.id === id);
      if (emp === undefined) {
        send(res, 404, html('<p>Not found</p>'), { 'content-type': 'text/html; charset=utf-8' });
        return;
      }
      send(res, 200, html(`
<h1>${escapeHtml(emp.firstName)} ${escapeHtml(emp.lastName)}</h1>
<p data-testid="employee-email">${escapeHtml(emp.email)}</p>
<p><a href="/employees/${escapeHtml(emp.id)}/edit" data-testid="employee-edit">Edit</a></p>
`), { 'content-type': 'text/html; charset=utf-8' });
      return;
    }

    send(res, 404, 'not found');
  } catch (error) {
    send(res, 500, `server error: ${error instanceof Error ? error.message : String(error)}`);
  }
});

server.listen(PORT, () => {
  const address = server.address();
  if (address === null || typeof address === 'string') {
    console.error(`fixture-app: unexpected address ${String(address)}`);
    process.exit(2);
  }
  // Print in a parseable form so the test harness can pick the port back
  // up without parsing `listen` log lines.
  process.stdout.write(`FIXTURE_PORT=${address.port}\n`);
});
