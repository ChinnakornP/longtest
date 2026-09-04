import { NextRequest, NextResponse } from 'next/server';

import { clearSessionCookie } from '@/mocks/http';
import { mockStore, SESSION_COOKIE } from '@/mocks/store';

export function POST(request: NextRequest): NextResponse {
  const token = request.cookies.get(SESSION_COOKIE)?.value;
  if (token) mockStore.sessions.delete(token);

  const response = new NextResponse(null, { status: 204 });
  clearSessionCookie(response);
  return response;
}
