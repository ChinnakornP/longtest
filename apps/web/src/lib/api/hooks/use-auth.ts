import { useMutation, useQueryClient } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { LoginRequest, SignupRequest, SignupResponse } from '@/lib/api/types';
import { useOrgStore } from '@/lib/stores/org-store';

import { meQueryKey } from './use-me';

export function useSignup() {
  const queryClient = useQueryClient();
  const setActiveOrgId = useOrgStore((s) => s.setActiveOrgId);

  return useMutation({
    mutationFn: (body: SignupRequest) =>
      apiFetch<SignupResponse>('/api/v1/auth/signup', {
        method: 'POST',
        body,
        skipOrgHeader: true,
      }),
    onSuccess: async (data) => {
      setActiveOrgId(data.org.id);
      await queryClient.invalidateQueries({ queryKey: meQueryKey });
    },
  });
}

export function useLogin() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (body: LoginRequest) =>
      apiFetch<void>('/api/v1/auth/login', { method: 'POST', body, skipOrgHeader: true }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: meQueryKey });
    },
  });
}

export function useLogout() {
  const queryClient = useQueryClient();
  const setActiveOrgId = useOrgStore((s) => s.setActiveOrgId);

  return useMutation({
    mutationFn: () =>
      apiFetch<void>('/api/v1/auth/logout', { method: 'POST', skipOrgHeader: true }),
    onSuccess: () => {
      setActiveOrgId(null);
      queryClient.clear();
    },
  });
}
