import { NextRequest, NextResponse } from 'next/server';

import type { RegisterFixtureRequest } from '@/lib/api/qa-types';
import { requireOrgHeader } from '@/mocks/http';
import { listFixtures, registerFixture } from '@/mocks/qa-store';

export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<NextResponse> {
  const access = requireOrgHeader(request);
  if (access instanceof NextResponse) return access;

  const { projectId } = await params;
  const fixtures = listFixtures(access.orgId, projectId);
  if (fixtures === null) {
    return NextResponse.json({ error: { code: 'NOT_FOUND', message: 'Project not found.' } }, { status: 404 });
  }
  return NextResponse.json({ fixtures }, { status: 200 });
}

const NAME_PATTERN = /^[a-z][a-z0-9_]{0,63}$/;

export async function POST(
  request: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<NextResponse> {
  const access = requireOrgHeader(request, ['member', 'admin', 'owner']);
  if (access instanceof NextResponse) return access;

  const { projectId } = await params;
  const body = (await request.json()) as Partial<RegisterFixtureRequest>;
  if (!body.name || !NAME_PATTERN.test(body.name)) {
    return NextResponse.json(
      { error: { code: 'VALIDATION_FAILED', message: 'name must be lowercase letters, numbers and underscores, starting with a letter.' } },
      { status: 422 },
    );
  }

  const result = registerFixture(access.orgId, projectId, body.name, body.description ?? '');
  if ('errorCode' in result) {
    return NextResponse.json({ error: { code: 'NOT_FOUND', message: 'Project not found.' } }, { status: 404 });
  }
  return NextResponse.json(result, { status: 201 });
}
