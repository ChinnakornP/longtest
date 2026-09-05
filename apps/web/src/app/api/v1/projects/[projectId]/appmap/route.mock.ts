import { NextRequest, NextResponse } from 'next/server';

import { requireOrgHeader } from '@/mocks/http';
import { getApplicationMap } from '@/mocks/qa-store';

export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<NextResponse> {
  const access = requireOrgHeader(request);
  if (access instanceof NextResponse) return access;

  const { projectId } = await params;
  const appMap = getApplicationMap(access.orgId, projectId);
  if (!appMap) {
    return NextResponse.json({ error: { code: 'NOT_FOUND', message: 'Project not found.' } }, { status: 404 });
  }
  return NextResponse.json(appMap, { status: 200 });
}
