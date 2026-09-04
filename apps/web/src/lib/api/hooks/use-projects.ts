import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiFetch } from '@/lib/api/client';
import type { CreateProjectRequest, Project } from '@/lib/api/qa-types';

export function projectsQueryKey(orgId: string | null) {
  return ['projects', orgId] as const;
}

export function projectQueryKey(orgId: string | null, projectId: string) {
  return ['projects', orgId, projectId] as const;
}

export function useProjects(orgId: string | null) {
  return useQuery({
    queryKey: projectsQueryKey(orgId),
    queryFn: () => apiFetch<Project[]>('/api/v1/projects'),
    enabled: orgId !== null,
  });
}

export function useProject(orgId: string | null, projectId: string) {
  return useQuery({
    queryKey: projectQueryKey(orgId, projectId),
    queryFn: () => apiFetch<Project>(`/api/v1/projects/${projectId}`),
    enabled: orgId !== null && projectId.length > 0,
  });
}

export function useCreateProject(orgId: string | null) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (body: CreateProjectRequest) => apiFetch<Project>('/api/v1/projects', { method: 'POST', body }),
    onSuccess: async (project) => {
      queryClient.setQueryData(projectQueryKey(orgId, project.id), project);
      await queryClient.invalidateQueries({ queryKey: projectsQueryKey(orgId) });
    },
  });
}
