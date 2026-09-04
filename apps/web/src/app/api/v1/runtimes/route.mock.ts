import { NextRequest, NextResponse } from 'next/server';

import { requireOrgHeader } from '@/mocks/http';
import { listRuntimes } from '@/mocks/qa-store';

export async function GET(request: NextRequest): Promise<NextResponse> {
  const access = requireOrgHeader(request);
  if (access instanceof NextResponse) return access;

  return NextResponse.json(listRuntimes(access.orgId), { status: 200 });
}
