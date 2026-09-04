import { NextRequest, NextResponse } from 'next/server';

import type { ApiErrorBody, OrgRole } from '@/lib/api/types';

import { getSessionUser, membershipFor, SESSION_COOKIE, type StoredUser } from './store';

export function errorResponse(status: number, code: string, message: string): NextResponse {
  const body: ApiErrorBody = { error: { code, message } };
  return NextResponse.json(body, { status });
}

/** Session-only auth: for /me. Org-scoped routes should use requireOrgAccess. */
export function requireSession(request: NextRequest): StoredUser | NextResponse {
  const user = getSessionUser(request.cookies.get(SESSION_COOKIE)?.value);
  if (!user) {
    return errorResponse(401, 'UNAUTHENTICATED', 'Not authenticated.');
  }
  return user;
}

/**
 * The T05 contract requires the active org to come from X-Org-ID, verified
 * by membership, never from the path/body. The path still carries an org id
 * for REST routing; this rejects any request where the two disagree instead
 * of silently trusting one over the other.
 */
export function requireOrgAccess(
  request: NextRequest,
  orgIdFromPath: string,
  allowedRoles?: OrgRole[],
): { user: StoredUser; role: OrgRole } | NextResponse {
  const user = requireSession(request);
  if (user instanceof NextResponse) return user;

  const headerOrgId = request.headers.get('X-Org-ID');
  if (!headerOrgId || headerOrgId !== orgIdFromPath) {
    return errorResponse(403, 'ORG_MISMATCH', 'X-Org-ID header must match the requested org.');
  }

  const membership = membershipFor(user.id, headerOrgId);
  if (!membership) {
    return errorResponse(403, 'NOT_A_MEMBER', 'You are not a member of this organization.');
  }

  if (allowedRoles && !allowedRoles.includes(membership.role)) {
    return errorResponse(403, 'FORBIDDEN', `Requires one of: ${allowedRoles.join(', ')}.`);
  }

  return { user, role: membership.role };
}

/**
 * Org-scoped auth for routes with no {orgId} path segment - the T08 contract
 * (project/run/runtime/report) puts the org purely in X-Org-ID, unlike the
 * T05 routes requireOrgAccess covers. Same membership/role check, just
 * without the path-vs-header assertion.
 */
export function requireOrgHeader(
  request: NextRequest,
  allowedRoles?: OrgRole[],
): { user: StoredUser; orgId: string; role: OrgRole } | NextResponse {
  const user = requireSession(request);
  if (user instanceof NextResponse) return user;

  const orgId = request.headers.get('X-Org-ID');
  if (!orgId) {
    return errorResponse(403, 'ORG_REQUIRED', 'X-Org-ID header is required.');
  }

  const membership = membershipFor(user.id, orgId);
  if (!membership) {
    return errorResponse(403, 'NOT_A_MEMBER', 'You are not a member of this organization.');
  }

  if (allowedRoles && !allowedRoles.includes(membership.role)) {
    return errorResponse(403, 'FORBIDDEN', `Requires one of: ${allowedRoles.join(', ')}.`);
  }

  return { user, orgId, role: membership.role };
}

const SESSION_COOKIE_ATTRS = {
  httpOnly: true,
  secure: true,
  sameSite: 'lax',
  path: '/',
} as const;

export function setSessionCookie(response: NextResponse, token: string, expiresAt: number): void {
  response.cookies.set(SESSION_COOKIE, token, {
    ...SESSION_COOKIE_ATTRS,
    expires: new Date(expiresAt),
  });
}

export function clearSessionCookie(response: NextResponse): void {
  response.cookies.set(SESSION_COOKIE, '', { ...SESSION_COOKIE_ATTRS, maxAge: 0 });
}
