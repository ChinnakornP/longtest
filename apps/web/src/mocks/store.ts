import { randomBytes, randomUUID, scryptSync, timingSafeEqual } from 'node:crypto';

import type { OrgRole } from '@/lib/api/types';

/**
 * In-memory stand-in for the T05 backend (server/internal/auth,
 * server/internal/org — not yet implemented). Implements the exact contract
 * from the T05 issue description so apps/web/src/lib/api can be built and
 * tested end to end today, and swapped for the real API later by pointing
 * NEXT_PUBLIC_API_BASE_URL at it. State lives only for the life of the dev
 * server process — it is not durable storage.
 */

export interface StoredUser {
  id: string;
  email: string;
  name: string;
  passwordHash: string;
}

export interface StoredOrg {
  id: string;
  name: string;
  slug: string;
}

export interface StoredMembership {
  userId: string;
  orgId: string;
  role: OrgRole;
}

export interface StoredSession {
  token: string;
  userId: string;
  expiresAt: number;
}

export interface StoredInvite {
  id: string;
  orgId: string;
  email: string;
  role: OrgRole;
  createdAt: number;
  expiresAt: number;
}

export interface StoredPairing {
  code: string;
  orgId: string;
  expiresAt: number;
  redeemed: boolean;
}

class MockStore {
  users = new Map<string, StoredUser>();
  usersByEmail = new Map<string, string>();
  orgs = new Map<string, StoredOrg>();
  memberships: StoredMembership[] = [];
  sessions = new Map<string, StoredSession>();
  invites = new Map<string, StoredInvite>();
  pairings = new Map<string, StoredPairing>();
}

const globalForMock = globalThis as unknown as { __qaMockStore?: MockStore };

export const mockStore = globalForMock.__qaMockStore ?? new MockStore();
globalForMock.__qaMockStore = mockStore;

// Matches auth.SessionCookieName(secure: true, domain: "") on the Go side. The
// mock sets the cookie Secure, Path=/ and Domain-less (see SESSION_COOKIE_ATTRS
// in ./http.ts), which are exactly the `__Host-` preconditions — and browsers
// treat http://localhost as a secure context, so it works in dev too. Keeping
// the name in step means swapping the mock for the real backend
// (docs/adr/0008-web-ships-no-backend.md) is not also a silent session reset.
export const SESSION_COOKIE = '__Host-qa_session';
export const SESSION_TTL_MS = 7 * 24 * 60 * 60 * 1000;
export const PAIRING_TTL_MS = 15 * 60 * 1000;

export function hashPassword(password: string): string {
  const salt = randomBytes(16).toString('hex');
  const hash = scryptSync(password, salt, 64).toString('hex');
  return `${salt}:${hash}`;
}

export function verifyPassword(password: string, stored: string): boolean {
  const [salt, hash] = stored.split(':');
  if (!salt || !hash) return false;
  const candidate = scryptSync(password, salt, 64);
  const expected = Buffer.from(hash, 'hex');
  return candidate.length === expected.length && timingSafeEqual(candidate, expected);
}

export function slugify(name: string): string {
  const base = name
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/(^-|-$)/g, '');
  return `${base || 'org'}-${randomUUID().slice(0, 6)}`;
}

export function createSession(userId: string): StoredSession {
  const session: StoredSession = {
    token: randomUUID(),
    userId,
    expiresAt: Date.now() + SESSION_TTL_MS,
  };
  mockStore.sessions.set(session.token, session);
  return session;
}

export function getSessionUser(token: string | undefined): StoredUser | null {
  if (!token) return null;
  const session = mockStore.sessions.get(token);
  if (!session || session.expiresAt < Date.now()) return null;
  return mockStore.users.get(session.userId) ?? null;
}

export function membershipFor(userId: string, orgId: string): StoredMembership | null {
  return mockStore.memberships.find((m) => m.userId === userId && m.orgId === orgId) ?? null;
}

export function orgsForUser(userId: string): Array<StoredOrg & { role: OrgRole }> {
  return mockStore.memberships
    .filter((m) => m.userId === userId)
    .map((m) => {
      const org = mockStore.orgs.get(m.orgId);
      if (!org) return null;
      return { ...org, role: m.role };
    })
    .filter((org): org is StoredOrg & { role: OrgRole } => org !== null);
}

export function generatePairingCode(): string {
  return randomBytes(6).toString('hex').toUpperCase().slice(0, 8);
}
