import { NextRequest, NextResponse } from 'next/server';

import type { SetTestCaseStatusRequest } from '@/lib/api/qa-types';
import { requireOrgHeader } from '@/mocks/http';
import { getTestCase, setTestCaseStatus } from '@/mocks/qa-store';

export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ testCaseId: string }> },
): Promise<NextResponse> {
  const access = requireOrgHeader(request);
  if (access instanceof NextResponse) return access;

  const { testCaseId } = await params;
  const record = getTestCase(access.orgId, testCaseId);
  if (!record) {
    return NextResponse.json({ error: { code: 'NOT_FOUND', message: 'Test case not found.' } }, { status: 404 });
  }
  return NextResponse.json(record, { status: 200 });
}

export async function PATCH(
  request: NextRequest,
  { params }: { params: Promise<{ testCaseId: string }> },
): Promise<NextResponse> {
  const access = requireOrgHeader(request, ['member', 'admin', 'owner']);
  if (access instanceof NextResponse) return access;

  const { testCaseId } = await params;
  const body = (await request.json()) as Partial<SetTestCaseStatusRequest>;
  if (!body.status) {
    return NextResponse.json(
      { error: { code: 'VALIDATION_FAILED', message: 'status is required.' } },
      { status: 422 },
    );
  }

  const result = setTestCaseStatus(access.orgId, testCaseId, body.status);
  if ('errorCode' in result) {
    if (result.errorCode === 'NOT_FOUND') {
      return NextResponse.json({ error: { code: 'NOT_FOUND', message: 'Test case not found.' } }, { status: 404 });
    }
    return NextResponse.json(
      { error: { code: 'VALIDATION_FAILED', message: `Cannot move to "${body.status}" from its current status.` } },
      { status: 422 },
    );
  }
  return NextResponse.json(result, { status: 200 });
}
