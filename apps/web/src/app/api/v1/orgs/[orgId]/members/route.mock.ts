import { NextRequest, NextResponse } from 'next/server';

import type { Member } from '@/lib/api/types';
import { requireOrgAccess } from '@/mocks/http';
import { mockStore } from '@/mocks/store';

export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ orgId: string }> },
): Promise<NextResponse> {
  const { orgId } = await params;
  const access = requireOrgAccess(request, orgId);
  if (access instanceof NextResponse) return access;

  const members: Member[] = mockStore.memberships
    .filter((m) => m.orgId === orgId)
    .map((m) => {
      const user = mockStore.users.get(m.userId);
      return user
        ? { id: `${orgId}:${user.id}`, userId: user.id, email: user.email, name: user.name, role: m.role }
        : null;
    })
    .filter((member): member is Member => member !== null);

  return NextResponse.json(members, { status: 200 });
}
