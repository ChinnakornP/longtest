import { NextRequest, NextResponse } from 'next/server';

import type { CreateProjectRequest } from '@/lib/api/qa-types';
import { requireOrgHeader } from '@/mocks/http';
import { createProject, listProjects } from '@/mocks/qa-store';

export async function GET(request: NextRequest): Promise<NextResponse> {
  const access = requireOrgHeader(request);
  if (access instanceof NextResponse) return access;

  return NextResponse.json(listProjects(access.orgId), { status: 200 });
}

export async function POST(request: NextRequest): Promise<NextResponse> {
  const access = requireOrgHeader(request, ['member', 'admin', 'owner']);
  if (access instanceof NextResponse) return access;

  const body = (await request.json()) as Partial<CreateProjectRequest>;
  if (!body.name || !body.baseUrl) {
    return NextResponse.json(
      { error: { code: 'VALIDATION_FAILED', message: 'name and baseUrl are required.' } },
      { status: 422 },
    );
  }
  try {
    new URL(body.baseUrl);
  } catch {
    return NextResponse.json(
      { error: { code: 'VALIDATION_FAILED', message: 'baseUrl must be an absolute URL.' } },
      { status: 422 },
    );
  }

  const project = createProject(access.orgId, { name: body.name, baseUrl: body.baseUrl });
  return NextResponse.json(project, { status: 201 });
}
