import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { Invite, InviteRequest, Member } from '@/lib/api/types';

export function membersQueryKey(orgId: string | null) {
  return ['members', orgId] as const;
}

export function useMembers(orgId: string | null) {
  return useQuery({
    queryKey: membersQueryKey(orgId),
    queryFn: () => apiFetch<Member[]>(`/api/v1/orgs/${orgId}/members`),
    enabled: orgId !== null,
  });
}

export function useInviteMember(orgId: string | null) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (body: InviteRequest) => {
      if (orgId === null) {
        return Promise.reject(new Error('No active org selected.'));
      }
      return apiFetch<Invite>(`/api/v1/orgs/${orgId}/invites`, { method: 'POST', body });
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: membersQueryKey(orgId) });
    },
  });
}
