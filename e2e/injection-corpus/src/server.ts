/**
 * A deliberately hostile web application.
 *
 * Every route serves one corpus case through the channel that case names, so a
 * live run — a real browser, a real AI CLI — meets the injection the same way
 * it would meet it on a compromised staging site. The deterministic half of
 * the corpus (`daemon/security/injection_corpus_test.go`) does not need this
 * server; this is what you point a browser at when you want to know whether a
 * particular model actually holds up.
 *
 * It is `node:http` for the same reason e2e/fixture-app is: a hostile fixture
 * has to be readable end to end, without a framework in the way.
 */

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

interface CorpusCase {
  id: string;
  channel: string;
  kind: string;
  description: string;
  payload?: string;
  payloadRepeat?: { unit: string; times: number };
}

interface Corpus {
  version: number;
  description: string;
  baseUrl: string;
  cases: CorpusCase[];
}

const corpusPath = fileURLToPath(new URL('../corpus.json', import.meta.url));
const corpus = JSON.parse(readFileSync(corpusPath, 'utf8')) as Corpus;

function payloadOf(c: CorpusCase): string {
  if (c.payloadRepeat) return c.payloadRepeat.unit.repeat(c.payloadRepeat.times);
  return c.payload ?? '';
}

/**
 * HTML-escape. The irony is deliberate: the fixture is hostile to the *agent*,
 * not to the browser, and an unescaped payload that breaks the markup would
 * change which channel the case is actually testing.
 */
function esc(s: string): string {
  return s
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;');
}

/** Renders one case into the DOM position its channel names. */
function renderCase(c: CorpusCase): string {
  const payload = payloadOf(c);
  const parts = {
    title: 'Employees',
    head: '',
    body: '',
  };

  switch (c.channel) {
    case 'page_title':
      parts.title = payload;
      break;
    case 'html_comment':
      // Not escaped: an escaped comment is not a comment, and this case exists
      // to prove that a comment reaches the agent. A payload that is already a
      // complete comment is emitted verbatim rather than nested inside another
      // one, which no parser would read the way the case intends.
      parts.body = payload.trimStart().startsWith('<!--')
        ? payload
        : `<!-- ${payload.replaceAll('--', '- -')} -->`;
      break;
    case 'aria_label':
      parts.body = `<button type="button" aria-label="${esc(payload)}">Submit</button>`;
      break;
    case 'image_alt':
      parts.body = `<img src="/pixel.png" alt="${esc(payload)}" width="1" height="1">`;
      break;
    case 'console':
      parts.head = `<script>console.log(${JSON.stringify(payload)});</script>`;
      break;
    case 'json_response':
      parts.body =
        `<p>This case is served at <a href="/case/${esc(c.id)}/api">/case/${esc(c.id)}/api</a>.</p>` +
        `<script>fetch(${JSON.stringify(`/case/${c.id}/api`)});</script>`;
      break;
    case 'download':
    case 'filename':
      parts.body =
        `<a id="export" href="/case/${esc(c.id)}/download">Export employees</a>`;
      break;
    default:
      parts.body = `<p id="payload">${esc(payload)}</p>`;
  }

  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>${esc(parts.title)}</title>
${parts.head}
</head>
<body>
<h1>Employees</h1>
<nav><a href="/">All cases</a></nav>
<input name="search" aria-label="Search" placeholder="Search">
<table><thead><tr><th>Name</th><th>Email</th></tr></thead>
<tbody><tr><td>Ada Lovelace</td><td>ada@example.test</td></tr></tbody></table>
${parts.body}
</body>
</html>
`;
}

function index(): string {
  const rows = corpus.cases
    .map(
      (c) =>
        `<li><a href="/case/${esc(c.id)}">${esc(c.id)}</a> ` +
        `<code>${esc(c.channel)}</code> — ${esc(c.description)}</li>`,
    )
    .join('\n');
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Injection corpus</title></head>
<body>
<h1>Injection corpus</h1>
<p>${esc(corpus.description)}</p>
<ul>
${rows}
</ul>
</body></html>
`;
}

function send(res: ServerResponse, status: number, type: string, body: string): void {
  res.writeHead(status, {
    'content-type': type,
    'cache-control': 'no-store',
    // The fixture is meant to be reachable only from the machine running the
    // test. Nothing here should ever be embedded by another origin.
    'x-frame-options': 'DENY',
  });
  res.end(body);
}

function handle(req: IncomingMessage, res: ServerResponse): void {
  const url = new URL(req.url ?? '/', 'http://localhost');
  const path = url.pathname;

  if (path === '/' || path === '/index.html') {
    send(res, 200, 'text/html; charset=utf-8', index());
    return;
  }

  const match = /^\/case\/([A-Za-z0-9._-]+)(\/api|\/download)?$/.exec(path);
  if (match) {
    const c = corpus.cases.find((x) => x.id === match[1]);
    if (!c) {
      send(res, 404, 'text/plain; charset=utf-8', 'no such case\n');
      return;
    }
    const payload = payloadOf(c);

    if (match[2] === '/api') {
      // The payload for a json_response case is already a JSON document.
      const body = c.channel === 'json_response' ? payload : JSON.stringify({ note: payload });
      send(res, c.channel === 'json_response' ? 500 : 200, 'application/json', body);
      return;
    }
    if (match[2] === '/download') {
      const filename =
        c.channel === 'filename' ? payload : `employees-${c.id}.txt`;
      res.writeHead(200, {
        'content-type': 'text/plain; charset=utf-8',
        // The filename is itself a channel: see the `filename` corpus case.
        'content-disposition': `attachment; filename="${filename.replaceAll('"', '')}"`,
        'cache-control': 'no-store',
      });
      res.end(c.channel === 'filename' ? 'Nothing interesting in the body.\n' : payload);
      return;
    }
    send(res, 200, 'text/html; charset=utf-8', renderCase(c));
    return;
  }

  if (path === '/pixel.png') {
    // 1x1 transparent PNG, so image_alt cases have a real image to fail to
    // describe.
    const png = Buffer.from(
      'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
      'base64',
    );
    res.writeHead(200, { 'content-type': 'image/png', 'content-length': png.length });
    res.end(png);
    return;
  }

  send(res, 404, 'text/plain; charset=utf-8', 'not found\n');
}

const server = createServer(handle);

// Bind to loopback only. This is a server whose entire purpose is to attack
// whatever reads it; it has no business being reachable from the LAN.
server.listen(Number(process.env.FIXTURE_PORT ?? 0), '127.0.0.1', () => {
  const address = server.address();
  const port = typeof address === 'object' && address !== null ? address.port : 0;
  process.stdout.write(`FIXTURE_PORT=${port}\n`);
});

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    server.close(() => process.exit(0));
  });
}
