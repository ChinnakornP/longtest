import { NextRequest, NextResponse } from 'next/server';

import type { LoginRequest } from '@/lib/api/types';
import { errorResponse, setSessionCookie } from '@/mocks/http';
import { createSession, mockStore, verifyPassword } from '@/mocks/store';

export async function POST(request: NextRequest): Promise<NextResponse> {
  const body = (await request.json().catch(() => null)) as Partial<LoginRequest> | null;
  if (!body?.email || !body.password) {
    return errorResponse(400, 'INVALID_INPUT', 'email and password are required.');
  }

  const userId = mockStore.usersByEmail.get(body.email);
  const user = userId ? mockStore.users.get(userId) : undefined;
  if (!user || !verifyPassword(body.password, user.passwordHash)) {
    return errorResponse(401, 'INVALID_CREDENTIALS', 'Invalid email or password.');
  }

  const session = createSession(user.id);
  const response = new NextResponse(null, { status: 204 });
  setSessionCookie(response, session.token, session.expiresAt);
  return response;
}
