import { NextRequest, NextResponse } from 'next/server';

import { findArtifactUnscoped } from '@/mocks/qa-store';

const TRANSPARENT_PNG_BASE64 =
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=';

/**
 * Serves placeholder bytes for a mock artifact - a real presigned URL points
 * straight at S3/MinIO (see T08), which does not exist in this dev mock.
 * Not authenticated, same as a real presigned URL would not be.
 */
export async function GET(
  _request: NextRequest,
  { params }: { params: Promise<{ artifactId: string }> },
): Promise<NextResponse> {
  const { artifactId } = await params;
  const artifact = findArtifactUnscoped(artifactId);
  if (!artifact) {
    return new NextResponse('Not found', { status: 404 });
  }

  if (artifact.kind === 'screenshot') {
    const bytes = Buffer.from(TRANSPARENT_PNG_BASE64, 'base64');
    return new NextResponse(bytes, {
      status: 200,
      headers: { 'Content-Type': 'image/png', 'Content-Disposition': 'inline' },
    });
  }

  if (artifact.kind === 'trace' || artifact.kind === 'video') {
    return new NextResponse('mock artifact placeholder - no real object storage in dev', {
      status: 200,
      headers: {
        'Content-Type': artifact.contentType ?? 'application/octet-stream',
        'Content-Disposition': `attachment; filename="${artifact.key.split('/').pop()}"`,
      },
    });
  }

  const placeholder =
    artifact.kind === 'console'
      ? [{ level: 'error', text: 'TypeError: Cannot read properties of undefined', ts: new Date().toISOString() }]
      : artifact.kind === 'network'
        ? [{ method: 'POST', url: '/api/mock/example', status: 500, durationMs: 812 }]
        : { note: 'mock artifact placeholder' };

  return NextResponse.json(placeholder, { status: 200 });
}
