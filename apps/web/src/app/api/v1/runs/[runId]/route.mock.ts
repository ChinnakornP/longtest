import { NextRequest, NextResponse } from 'next/server';

import { requireOrgHeader } from '@/mocks/http';
import { getStoredRun, snapshotRun } from '@/mocks/qa-store';

export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ runId: string }> },
): Promise<NextResponse> {
  const access = requireOrgHeader(request);
  if (access instanceof NextResponse) return access;

  const { runId } = await params;
  const stored = getStoredRun(access.orgId, runId);
  if (!stored) {
    return NextResponse.json({ error: { code: 'NOT_FOUND', message: 'Run not found.' } }, { status: 404 });
  }
  return NextResponse.json(snapshotRun(stored), { status: 200 });
}
