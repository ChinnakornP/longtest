import { NextRequest, NextResponse } from 'next/server';

import { requireOrgHeader } from '@/mocks/http';
import { cancelRun } from '@/mocks/qa-store';

export async function POST(
  request: NextRequest,
  { params }: { params: Promise<{ runId: string }> },
): Promise<NextResponse> {
  const access = requireOrgHeader(request, ['member', 'admin', 'owner']);
  if (access instanceof NextResponse) return access;

  const { runId } = await params;
  const run = cancelRun(access.orgId, runId);
  if (!run) {
    return NextResponse.json({ error: { code: 'NOT_FOUND', message: 'Run not found.' } }, { status: 404 });
  }
  return NextResponse.json(run, { status: 200 });
}
