import { NextRequest, NextResponse } from 'next/server';

import { requireOrgHeader } from '@/mocks/http';
import { deleteFixture } from '@/mocks/qa-store';

export async function DELETE(
  request: NextRequest,
  { params }: { params: Promise<{ projectId: string; name: string }> },
): Promise<NextResponse> {
  const access = requireOrgHeader(request, ['member', 'admin', 'owner']);
  if (access instanceof NextResponse) return access;

  const { projectId, name } = await params;
  const ok = deleteFixture(access.orgId, projectId, name);
  if (!ok) {
    return NextResponse.json({ error: { code: 'NOT_FOUND', message: 'Project not found.' } }, { status: 404 });
  }
  return new NextResponse(null, { status: 204 });
}
