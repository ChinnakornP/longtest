/**
 * @fileoverview robots.txt policy parsing.
 */

import { describe, expect, it } from 'vitest';
import { parseRobotsTxt, permissivePolicy } from '../../src/crawler/robots.ts';

describe('robots: parsing', () => {
  it('returns the permissive policy for empty input', () => {
    const policy = parseRobotsTxt('');
    expect(policy.isAllowed('/anything')).toBe(true);
  });

  it('honours a single Disallow rule', () => {
    const policy = parseRobotsTxt('User-agent: *\nDisallow: /admin\n');
    expect(policy.isAllowed('/')).toBe(true);
    expect(policy.isAllowed('/login')).toBe(true);
    expect(policy.isAllowed('/admin')).toBe(false);
    expect(policy.isAllowed('/admin/users')).toBe(false);
  });

  it('respects the most-specific match (longest prefix wins)', () => {
    const policy = parseRobotsTxt('User-agent: *\nDisallow: /admin\nAllow: /admin/public\n');
    expect(policy.isAllowed('/admin/secret')).toBe(false);
    expect(policy.isAllowed('/admin/public')).toBe(true);
  });

  it('prefers qa-crawler block over the wildcard block', () => {
    const body = [
      'User-agent: *',
      'Disallow: /',
      '',
      'User-agent: qa-crawler',
      'Allow: /',
    ].join('\n');
    const policy = parseRobotsTxt(body);
    expect(policy.isAllowed('/anything')).toBe(true);
  });

  it('skips comment lines', () => {
    const policy = parseRobotsTxt('# this is a comment\nUser-agent: *\nDisallow: /private\n');
    expect(policy.isAllowed('/private')).toBe(false);
    expect(policy.isAllowed('/public')).toBe(true);
  });

  it('treats an empty Disallow value as "allow everything"', () => {
    const policy = parseRobotsTxt('User-agent: *\nDisallow:\n');
    expect(policy.isAllowed('/anything')).toBe(true);
  });

  it('falls back to permissive when no matching block', () => {
    const policy = parseRobotsTxt('User-agent: googlebot\nDisallow: /secret\n');
    // googlebot block is ignored, no wildcard block either → permissive.
    expect(policy).toBe(permissivePolicy);
  });
});
