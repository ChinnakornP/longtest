import { randomUUID } from 'node:crypto';

import { NextRequest, NextResponse } from 'next/server';

import type { SignupRequest, SignupResponse } from '@/lib/api/types';
import { errorResponse, setSessionCookie } from '@/mocks/http';
import { createSession, hashPassword, mockStore, slugify } from '@/mocks/store';

export async function POST(request: NextRequest): Promise<NextResponse> {
  const body = (await request.json().catch(() => null)) as Partial<SignupRequest> | null;
  if (!body?.email || !body.password || !body.name || !body.orgName) {
    return errorResponse(400, 'INVALID_INPUT', 'email, password, name and orgName are required.');
  }

  if (mockStore.usersByEmail.has(body.email)) {
    return errorResponse(409, 'EMAIL_TAKEN', 'An account with this email already exists.');
  }

  const user = {
    id: randomUUID(),
    email: body.email,
    name: body.name,
    passwordHash: hashPassword(body.password),
  };
  mockStore.users.set(user.id, user);
  mockStore.usersByEmail.set(user.email, user.id);

  const org = { id: randomUUID(), name: body.orgName, slug: slugify(body.orgName) };
  mockStore.orgs.set(org.id, org);
  mockStore.memberships.push({ userId: user.id, orgId: org.id, role: 'owner' });

  const session = createSession(user.id);

  const responseBody: SignupResponse = {
    user: { id: user.id, email: user.email, name: user.name },
    org,
  };
  const response = NextResponse.json(responseBody, { status: 201 });
  setSessionCookie(response, session.token, session.expiresAt);
  return response;
}
