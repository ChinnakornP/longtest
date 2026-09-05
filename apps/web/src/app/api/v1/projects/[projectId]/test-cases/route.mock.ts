import { NextRequest, NextResponse } from 'next/server';

import { requireOrgHeader } from '@/mocks/http';
import { listTestCases } from '@/mocks/qa-store';

export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<NextResponse> {
  const access = requireOrgHeader(request);
  if (access instanceof NextResponse) return access;

  const { projectId } = await params;
  const status = request.nextUrl.searchParams.get('status') ?? undefined;
  const limit = Number(request.nextUrl.searchParams.get('limit') ?? 50);
  const offset = Number(request.nextUrl.searchParams.get('offset') ?? 0);
  const result = listTestCases(access.orgId, projectId, status, limit, offset);
  if (!result) {
    return NextResponse.json({ error: { code: 'NOT_FOUND', message: 'Project not found.' } }, { status: 404 });
  }
  return NextResponse.json(result, { status: 200 });
}
