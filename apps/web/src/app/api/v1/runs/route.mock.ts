import { NextRequest, NextResponse } from 'next/server';

import type { CreateRunRequest } from '@/lib/api/qa-types';
import { RUN_MODE_VALUES } from '@/lib/api/qa-types';
import { requireOrgHeader } from '@/mocks/http';
import { createRun, snapshotRun } from '@/mocks/qa-store';

const ERROR_STATUS: Record<string, number> = {
  PROJECT_NOT_FOUND: 404,
  RUNTIME_NOT_FOUND: 404,
  RUNTIME_OFFLINE: 409,
};

export async function POST(request: NextRequest): Promise<NextResponse> {
  const access = requireOrgHeader(request, ['member', 'admin', 'owner']);
  if (access instanceof NextResponse) return access;

  const body = (await request.json()) as Partial<CreateRunRequest>;
  if (!body.projectId || !body.runtimeId || !body.mode || !RUN_MODE_VALUES.includes(body.mode)) {
    return NextResponse.json(
      { error: { code: 'VALIDATION_FAILED', message: 'projectId, runtimeId and a valid mode are required.' } },
      { status: 422 },
    );
  }

  const result = createRun(access.orgId, {
    projectId: body.projectId,
    runtimeId: body.runtimeId,
    mode: body.mode,
    testCaseIds: body.testCaseIds,
  });

  if ('errorCode' in result) {
    const status = ERROR_STATUS[result.errorCode] ?? 400;
    return NextResponse.json(
      { error: { code: result.errorCode, message: runErrorMessage(result.errorCode) } },
      { status },
    );
  }

  return NextResponse.json(snapshotRun(result), { status: 201 });
}

function runErrorMessage(code: string): string {
  switch (code) {
    case 'PROJECT_NOT_FOUND':
      return 'Project not found.';
    case 'RUNTIME_NOT_FOUND':
      return 'Runtime not found.';
    case 'RUNTIME_OFFLINE':
      return 'That runtime is offline. Pick an online runtime and try again.';
    default:
      return 'Could not start the run.';
  }
}
