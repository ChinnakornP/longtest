import { NextRequest, NextResponse } from 'next/server';

import type { RunEventsPage } from '@/lib/api/qa-types';
import { requireOrgHeader } from '@/mocks/http';
import { getStoredRun, listRunEventsSince } from '@/mocks/qa-store';

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

  const sinceParam = request.nextUrl.searchParams.get('since');
  const since = sinceParam ? Number.parseInt(sinceParam, 10) : 0;
  const { events, nextSince } = listRunEventsSince(stored, Number.isFinite(since) ? since : 0);

  const body: RunEventsPage = { events, nextSince };
  return NextResponse.json(body, { status: 200 });
}
