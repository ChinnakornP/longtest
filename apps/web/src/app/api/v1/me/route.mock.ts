import { NextRequest, NextResponse } from 'next/server';

import type { MeResponse } from '@/lib/api/types';
import { requireSession } from '@/mocks/http';
import { orgsForUser } from '@/mocks/store';

export function GET(request: NextRequest): NextResponse {
  const user = requireSession(request);
  if (user instanceof NextResponse) return user;

  const body: MeResponse = {
    user: { id: user.id, email: user.email, name: user.name },
    orgs: orgsForUser(user.id).map((org) => ({
      id: org.id,
      name: org.name,
      slug: org.slug,
      role: org.role,
    })),
  };
  return NextResponse.json(body, { status: 200 });
}
