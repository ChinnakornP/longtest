import { NextRequest, NextResponse } from 'next/server';

import { requireOrgHeader } from '@/mocks/http';
import { getProject } from '@/mocks/qa-store';

export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<NextResponse> {
  const access = requireOrgHeader(request);
  if (access instanceof NextResponse) return access;

  const { projectId } = await params;
  const project = getProject(access.orgId, projectId);
  if (!project) {
    return NextResponse.json({ error: { code: 'NOT_FOUND', message: 'Project not found.' } }, { status: 404 });
  }
  return NextResponse.json(project, { status: 200 });
}
