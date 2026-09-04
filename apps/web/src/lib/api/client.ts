import { useOrgStore } from '@/lib/stores/org-store';

import { authEvents } from './auth-events';
import type { ApiErrorBody } from './types';

// A blank base means "same origin", which only resolves to something real in
// dev, where the T05 mock lives at that origin (apps/web/src/app/api/v1/**).
// In production apps/web has no backend of its own (ADR-008) - fail the
// build/boot instead of silently 404ing every request when the env var was
// left unset.
export function resolveApiBase(): string {
  const configured = process.env.NEXT_PUBLIC_API_BASE_URL;
  if (configured) return configured;
  if (process.env.NODE_ENV === 'production') {
    throw new Error(
      'NEXT_PUBLIC_API_BASE_URL must be set in production - apps/web has no backend of its own (see docs/adr/0008-web-ships-no-backend.md).',
    );
  }
  return '';
}

const API_BASE = resolveApiBase();

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details?: unknown;

  constructor(status: number, code: string, message: string, details?: unknown) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

export interface ApiFetchOptions extends Omit<RequestInit, 'body'> {
  body?: unknown;
  /** Explicit org id for this call. Defaults to the active org in useOrgStore. */
  orgId?: string | null;
  /** Skip X-Org-ID entirely — only for endpoints outside org scope (signup, login, /me). */
  skipOrgHeader?: boolean;
}

/**
 * Central fetch wrapper: attaches the session cookie and X-Org-ID, and maps
 * the {error:{code,message}} contract to ApiError. 401 always means "not
 * authenticated" so it always signals a login redirect; 403 only does the
 * same for reads (a write 403 is a permission error the caller should toast,
 * not a reason to bounce the whole page — the UI should not have offered
 * that write button in the first place, see lib/auth/role.ts).
 */
export async function apiFetch<T>(path: string, options: ApiFetchOptions = {}): Promise<T> {
  const { body, orgId, skipOrgHeader, headers, method, ...rest } = options;
  const activeOrgId = skipOrgHeader ? null : (orgId ?? useOrgStore.getState().activeOrgId);

  const requestHeaders = new Headers(headers);
  if (body !== undefined) requestHeaders.set('Content-Type', 'application/json');
  if (activeOrgId) requestHeaders.set('X-Org-ID', activeOrgId);

  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, {
      ...rest,
      method,
      headers: requestHeaders,
      credentials: 'include',
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new ApiError(0, 'NETWORK_ERROR', 'Could not reach the server. Check your connection.');
  }

  const isRead = method === undefined || method === 'GET';
  if (response.status === 401) {
    authEvents.emit('unauthenticated');
  } else if (response.status === 403 && isRead) {
    authEvents.emit('forbidden');
  }

  if (!response.ok) {
    const parsed = await safeParseError(response);
    throw new ApiError(
      response.status,
      parsed?.error.code ?? 'UNKNOWN_ERROR',
      parsed?.error.message ?? `Request failed with status ${response.status}`,
      parsed?.error.details,
    );
  }

  if (response.status === 204) {
    return undefined as T;
  }

  return (await response.json()) as T;
}

async function safeParseError(response: Response): Promise<ApiErrorBody | null> {
  try {
    const data: unknown = await response.json();
    if (
      typeof data === 'object' &&
      data !== null &&
      'error' in data &&
      typeof (data as ApiErrorBody).error?.code === 'string'
    ) {
      return data as ApiErrorBody;
    }
    return null;
  } catch {
    return null;
  }
}
