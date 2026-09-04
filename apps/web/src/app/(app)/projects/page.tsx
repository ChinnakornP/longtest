'use client';

import Link from 'next/link';
import type { FormEvent } from 'react';
import { useState } from 'react';
import { toast } from 'sonner';

import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { ApiError } from '@/lib/api/client';
import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useCreateProject, useProjects } from '@/lib/api/hooks/use-projects';
import { canWrite } from '@/lib/auth/role';

export default function ProjectsPage() {
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id ?? null;
  const projects = useProjects(orgId);
  const createProject = useCreateProject(orgId);
  const [name, setName] = useState('');
  const [baseUrl, setBaseUrl] = useState('');
  const canCreate = canWrite(activeOrg?.role);

  const handleSubmit = (event: FormEvent) => {
    event.preventDefault();
    createProject.mutate(
      { name, baseUrl },
      {
        onSuccess: () => {
          setName('');
          setBaseUrl('');
          toast.success('Project created.');
        },
        onError: (error) => {
          toast.error(error instanceof ApiError ? error.message : 'Could not create the project.');
        },
      },
    );
  };

  return (
    <div className="space-y-6">
      <div className="space-y-1">
        <h1 className="text-2xl font-semibold tracking-tight">Projects</h1>
        <p className="text-muted-foreground text-sm">
          Point the AI QA agent at a web application to start testing it.
        </p>
      </div>

      {canCreate && (
        <Card>
          <CardHeader>
            <CardTitle>New project</CardTitle>
            <CardDescription>Give it a name and the URL to test.</CardDescription>
          </CardHeader>
          <CardContent>
            <form onSubmit={handleSubmit} className="grid gap-4 sm:grid-cols-[1fr_2fr_auto] sm:items-end">
              <div className="space-y-1.5">
                <Label htmlFor="project-name">Name</Label>
                <Input
                  id="project-name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="Acme QA"
                  required
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="project-url">Base URL</Label>
                <Input
                  id="project-url"
                  type="url"
                  value={baseUrl}
                  onChange={(e) => setBaseUrl(e.target.value)}
                  placeholder="https://demo.mycompany.com"
                  required
                />
              </div>
              <Button type="submit" disabled={createProject.isPending}>
                {createProject.isPending ? 'Creating…' : 'Create project'}
              </Button>
            </form>
          </CardContent>
        </Card>
      )}

      {projects.isLoading && <p className="text-muted-foreground text-sm">Loading projects…</p>}
      {projects.isError && <p className="text-sm text-red-600">Could not load projects. Try refreshing.</p>}

      {projects.data && projects.data.length === 0 && (
        <p className="text-muted-foreground text-sm">
          No projects yet. {canCreate ? 'Create one above to get started.' : 'Ask an admin to create one.'}
        </p>
      )}

      {projects.data && projects.data.length > 0 && (
        <div className="grid gap-4 sm:grid-cols-2">
          {projects.data.map((project) => (
            <Link key={project.id} href={`/projects/${project.id}`}>
              <Card className="hover:border-primary h-full transition-colors">
                <CardHeader>
                  <CardTitle>{project.name}</CardTitle>
                  <CardDescription className="truncate">{project.baseUrl}</CardDescription>
                </CardHeader>
              </Card>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}
