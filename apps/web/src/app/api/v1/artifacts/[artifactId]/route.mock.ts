import { NextRequest, NextResponse } from 'next/server';

import type { ArtifactUrlResponse } from '@/lib/api/qa-types';
import { requireOrgHeader } from '@/mocks/http';
import { findArtifact } from '@/mocks/qa-store';

/**
 * `GET /api/v1/artifacts/{artifactId}` -> presigned download URL is a
 * frontend assumption, not part of the published T08 contract - flagged for
 * Architect. This mock issues a URL to the equally-mock `/raw` route below,
 * which serves a small placeholder blob per artifact kind since there is no
 * real object storage behind the dev mock.
 */
export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ artifactId: string }> },
): Promise<NextResponse> {
  const access = requireOrgHeader(request);
  if (access instanceof NextResponse) return access;

  const { artifactId } = await params;
  const artifact = findArtifact(access.orgId, artifactId);
  if (!artifact) {
    return NextResponse.json({ error: { code: 'NOT_FOUND', message: 'Artifact not found.' } }, { status: 404 });
  }

  const expiresAt = new Date(Date.now() + 10 * 60 * 1000).toISOString();
  const body: ArtifactUrlResponse = {
    url: `/api/v1/artifacts/${artifactId}/raw`,
    expiresAt,
  };
  return NextResponse.json(body, { status: 200 });
}
