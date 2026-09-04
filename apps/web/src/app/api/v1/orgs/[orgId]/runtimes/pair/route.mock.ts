import { NextRequest, NextResponse } from 'next/server';

import type { PairingCodeResponse } from '@/lib/api/types';
import { requireOrgAccess } from '@/mocks/http';
import { generatePairingCode, mockStore, PAIRING_TTL_MS } from '@/mocks/store';

export async function POST(
  request: NextRequest,
  { params }: { params: Promise<{ orgId: string }> },
): Promise<NextResponse> {
  const { orgId } = await params;
  const access = requireOrgAccess(request, orgId, ['admin', 'owner']);
  if (access instanceof NextResponse) return access;

  const code = generatePairingCode();
  const expiresAt = Date.now() + PAIRING_TTL_MS;
  mockStore.pairings.set(code, { code, orgId, expiresAt, redeemed: false });

  const body: PairingCodeResponse = { pairingCode: code, expiresAt: new Date(expiresAt).toISOString() };
  return NextResponse.json(body, { status: 201 });
}
