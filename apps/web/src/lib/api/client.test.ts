import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useOrgStore } from '@/lib/stores/org-store';

import { authEvents } from './auth-events';
import { apiFetch, ApiError } from './client';

function jsonResponse(status: number, body: unknown, headers: HeadersInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

describe('apiFetch', () => {
  beforeEach(() => {
    useOrgStore.getState().setActiveOrgId(null);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('attaches X-Org-ID from the active org store', async () => {
    useOrgStore.getState().setActiveOrgId('org-1');
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { ok: true }));
    vi.stubGlobal('fetch', fetchMock);

    await apiFetch('/api/v1/orgs/org-1/members');

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = new Headers(init.headers);
    expect(headers.get('X-Org-ID')).toBe('org-1');
  });

  it('sends the new org id immediately after switching orgs', async () => {
    useOrgStore.getState().setActiveOrgId('org-1');
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(jsonResponse(200, { ok: true })));
    vi.stubGlobal('fetch', fetchMock);

    await apiFetch('/api/v1/orgs/org-1/members');
    useOrgStore.getState().setActiveOrgId('org-2');
    await apiFetch('/api/v1/orgs/org-2/members');

    const headersOfCall = (index: number) => {
      const [, init] = fetchMock.mock.calls[index] as [string, RequestInit];
      return new Headers(init.headers);
    };
    expect(headersOfCall(0).get('X-Org-ID')).toBe('org-1');
    expect(headersOfCall(1).get('X-Org-ID')).toBe('org-2');
  });

  it('omits X-Org-ID when skipOrgHeader is set', async () => {
    useOrgStore.getState().setActiveOrgId('org-1');
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { ok: true }));
    vi.stubGlobal('fetch', fetchMock);

    await apiFetch('/api/v1/me', { skipOrgHeader: true });

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(new Headers(init.headers).has('X-Org-ID')).toBe(false);
  });

  it('throws an ApiError mapped from the {error:{code,message}} contract', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(403, { error: { code: 'FORBIDDEN', message: 'admin or owner role required' } }),
      ),
    );

    await expect(apiFetch('/api/v1/orgs/org-1/runtimes/pair', { method: 'POST' })).rejects.toMatchObject({
      code: 'FORBIDDEN',
      message: 'admin or owner role required',
      status: 403,
    });
  });

  it('emits an unauthenticated event on 401', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse(401, { error: { code: 'UNAUTHENTICATED', message: 'no' } })),
    );
    const spy = vi.fn();
    const unsubscribe = authEvents.subscribe(spy);

    await expect(apiFetch('/api/v1/me', { skipOrgHeader: true })).rejects.toBeInstanceOf(ApiError);
    expect(spy).toHaveBeenCalledWith('unauthenticated');

    unsubscribe();
  });

  it('emits a forbidden event on 403 for reads but not for writes', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse(403, { error: { code: 'FORBIDDEN', message: 'no' } })),
    );
    const spy = vi.fn();
    const unsubscribe = authEvents.subscribe(spy);

    await expect(apiFetch('/api/v1/orgs/org-1/members')).rejects.toBeInstanceOf(ApiError);
    expect(spy).toHaveBeenCalledWith('forbidden');

    spy.mockClear();
    await expect(
      apiFetch('/api/v1/orgs/org-1/runtimes/pair', { method: 'POST' }),
    ).rejects.toBeInstanceOf(ApiError);
    expect(spy).not.toHaveBeenCalled();

    unsubscribe();
  });
});
