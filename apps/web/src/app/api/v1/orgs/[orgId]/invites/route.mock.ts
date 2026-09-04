import { randomUUID } from 'node:crypto';

import { NextRequest, NextResponse } from 'next/server';

import type { Invite, InviteRequest } from '@/lib/api/types';
import { errorResponse, requireOrgAccess } from '@/mocks/http';
import { mockStore } from '@/mocks/store';

const INVITE_TTL_MS = 7 * 24 * 60 * 60 * 1000;

export async function POST(
  request: NextRequest,
  { params }: { params: Promise<{ orgId: string }> },
): Promise<NextResponse> {
  const { orgId } = await params;
  const access = requireOrgAccess(request, orgId, ['admin', 'owner']);
  if (access instanceof NextResponse) return access;

  const body = (await request.json().catch(() => null)) as Partial<InviteRequest> | null;
  if (!body?.email || !body.role) {
    return errorResponse(400, 'INVALID_INPUT', 'email and role are required.');
  }

  const invite: Invite = {
    id: randomUUID(),
    orgId,
    email: body.email,
    role: body.role,
    createdAt: new Date().toISOString(),
    expiresAt: new Date(Date.now() + INVITE_TTL_MS).toISOString(),
  };
  mockStore.invites.set(invite.id, {
    ...invite,
    createdAt: Date.now(),
    expiresAt: Date.now() + INVITE_TTL_MS,
  });

  return NextResponse.json(invite, { status: 201 });
}
