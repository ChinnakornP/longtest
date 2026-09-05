/**
 * @fileoverview Minimal robots.txt policy the crawler honours.
 *
 * The contract is intentionally small: we parse a single-user-agent rule
 * block, check `Disallow` / `Allow` against a path, and otherwise default
 * to "allowed". This is *not* a full RFC 9309 parser — the fixture app
 * has no robots.txt, the policy file in the project lays out the cases we
 * do honour, and the goal is "don't crawl paths the site owner asked us
 * not to crawl", not "be a search engine".
 *
 * Anything more than prefix matching is out of scope: `*`, `$` and regex
 * quantifiers are not interpreted. A `Disallow: /admin` blocks `/admin`
 * and `/admin/users`; it does not block `/admin-archive`. The application
 * team that wants fine-grained control runs the crawler with
 * `respectRobots: false` and does the filtering themselves.
 */

import { request as httpRequest } from 'node:https';
import { request as httpRequestHttp } from 'node:http';
import { URL } from 'node:url';
import type { IncomingMessage } from 'node:http';

export interface RobotsPolicy {
  /** A path is allowed when no `Disallow` rule matches it. */
  isAllowed(path: string): boolean;
}

/** Always allow. Used when robots.txt is unreachable or `respectRobots=false`. */
export const permissivePolicy: RobotsPolicy = {
  isAllowed: () => true,
};

/**
 * Parse a robots.txt body into a policy. The body is split into rules
 * grouped by `User-agent`. We pick the most specific block whose agent
 * matches `qa-crawler` or `*`, in that order; missing or unreachable
 * robots.txt becomes `permissivePolicy`.
 *
 * `Allow` and `Disallow` rules accumulate within a block; the longest
 * matching prefix wins. Ties go to `Allow` (Google's rule).
 */
export function parseRobotsTxt(body: string): RobotsPolicy {
  // Tokenise: split on blank lines into groups, then split each group
  // into (key, value) pairs.
  const groups: Array<{ agents: string[]; rules: Array<{ kind: 'allow' | 'disallow'; value: string }> }> = [];
  for (const block of body.split(/\r?\n\r?\n+/)) {
    const lines = block.split(/\r?\n/).map((l) => stripComment(l.trim())).filter((l) => l.length > 0);
    if (lines.length === 0) continue;
    const agents: string[] = [];
    const rules: Array<{ kind: 'allow' | 'disallow'; value: string }> = [];
    for (const line of lines) {
      const colon = line.indexOf(':');
      if (colon < 0) continue;
      const key = line.slice(0, colon).trim().toLowerCase();
      const value = line.slice(colon + 1).trim();
      if (key === 'user-agent') {
        agents.push(value.toLowerCase());
      } else if (key === 'allow') {
        rules.push({ kind: 'allow', value });
      } else if (key === 'disallow') {
        rules.push({ kind: 'disallow', value });
      }
    }
    if (rules.length === 0) continue;
    groups.push({ agents, rules });
  }

  // Pick the most specific group. We treat `qa-crawler` and `*` as our
  // two agent names; everything else is ignored.
  let chosen: typeof groups[number] | undefined;
  for (const g of groups) {
    if (g.agents.includes('qa-crawler')) {
      chosen = g;
      break;
    }
  }
  if (chosen === undefined) {
    for (const g of groups) {
      if (g.agents.includes('*')) {
        chosen = g;
        break;
      }
    }
  }
  if (chosen === undefined) return permissivePolicy;

  const rules = chosen.rules;

  return {
    isAllowed(path: string): boolean {
      let bestKind: 'allow' | 'disallow' | undefined;
      let bestLen = -1;
      for (const rule of rules) {
        // Empty value means "match nothing" (Disallow:) or "allow all"
        // (Allow:). RFC 9309 says empty disallow allows everything;
        // empty allow is meaningless but we treat it as no-op.
        if (rule.value === '') {
          if (rule.kind === 'allow' && bestKind === undefined) {
            bestKind = 'allow';
            bestLen = 0;
          }
          continue;
        }
        if (!path.startsWith(rule.value)) continue;
        if (rule.value.length > bestLen) {
          bestKind = rule.kind;
          bestLen = rule.value.length;
        }
      }
      return bestKind !== 'disallow';
    },
  };
}

function stripComment(line: string): string {
  // robots.txt comments start at `#` and run to end-of-line, but only
  // when `#` is the first non-whitespace character on the line.
  if (line.startsWith('#')) return '';
  return line;
}

/**
 * Fetch robots.txt for the given base URL. Returns `permissivePolicy` when
 * the file is missing (404) or unreachable, so a missing robots.txt means
 * "crawl everything", which is the de-facto default.
 */
export async function fetchRobotsPolicy(baseUrl: string, timeoutMs = 5_000): Promise<RobotsPolicy> {
  const u = new URL('/robots.txt', baseUrl);
  return new Promise<RobotsPolicy>((resolve) => {
    const lib = u.protocol === 'https:' ? httpRequest : httpRequestHttp;
    const req = lib(
      u,
      { method: 'GET', timeout: timeoutMs },
      (res: IncomingMessage) => {
        if (res.statusCode === undefined || res.statusCode >= 400) {
          res.resume();
          resolve(permissivePolicy);
          return;
        }
        const chunks: Buffer[] = [];
        res.on('data', (c: Buffer) => chunks.push(c));
        res.on('end', () => {
          const body = Buffer.concat(chunks).toString('utf8');
          resolve(parseRobotsTxt(body));
        });
        res.on('error', () => resolve(permissivePolicy));
      },
    );
    req.on('timeout', () => {
      req.destroy();
      resolve(permissivePolicy);
    });
    req.on('error', () => resolve(permissivePolicy));
    req.end();
  });
}
