'use client';

import { useParams } from 'next/navigation';
import type { FormEvent } from 'react';
import { useState } from 'react';
import { toast } from 'sonner';

import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { ApiError } from '@/lib/api/client';
import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useDeleteFixture, useFixtures, useRegisterFixture } from '@/lib/api/hooks/use-fixtures';
import { canWrite } from '@/lib/auth/role';

const NAME_PATTERN = /^[a-z][a-z0-9_]{0,63}$/;

export default function FixturesPage() {
  const params = useParams<{ projectId: string }>();
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id ?? null;
  const canManage = canWrite(activeOrg?.role);

  const fixtures = useFixtures(orgId, params.projectId);
  const registerFixture = useRegisterFixture(orgId, params.projectId);
  const deleteFixture = useDeleteFixture(orgId, params.projectId);

  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const nameError = name.length > 0 && !NAME_PATTERN.test(name) ? 'lowercase letters, numbers and underscores only, starting with a letter' : null;

  const handleSubmit = (event: FormEvent) => {
    event.preventDefault();
    if (!NAME_PATTERN.test(name)) return;
    registerFixture.mutate(
      { name, description: description || undefined },
      {
        onSuccess: () => {
          setName('');
          setDescription('');
          toast.success('Fixture registered.');
        },
        onError: (error) => {
          toast.error(error instanceof ApiError ? error.message : 'Could not register the fixture.');
        },
      },
    );
  };

  const handleDelete = (fixtureName: string) => {
    deleteFixture.mutate(fixtureName, {
      onSuccess: () => toast.success(`Removed fixture "${fixtureName}".`),
      onError: (error) => {
        toast.error(error instanceof ApiError ? error.message : 'Could not remove the fixture.');
      },
    });
  };

  return (
    <div className="space-y-6">
      <div className="space-y-1">
        <h1 className="text-2xl font-semibold tracking-tight">Fixtures</h1>
        <p className="text-muted-foreground text-sm">
          Named starting states a test case can reference as <code>fixture:&lt;name&gt;</code>. Credentials are never
          stored here — they live in the daemon&apos;s sealed store on the operator&apos;s own machine.
        </p>
      </div>

      {canManage && (
        <Card>
          <CardHeader>
            <CardTitle>Register a fixture</CardTitle>
            <CardDescription>Only a name and a description — no username or password field, on purpose.</CardDescription>
          </CardHeader>
          <CardContent>
            <form onSubmit={handleSubmit} className="grid gap-4 sm:grid-cols-[1fr_2fr_auto] sm:items-end">
              <div className="space-y-1.5">
                <Label htmlFor="fixture-name">Name</Label>
                <Input
                  id="fixture-name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="logged_in_as_admin"
                  required
                />
                {nameError && <p className="text-xs text-red-600">{nameError}</p>}
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="fixture-description">Description</Label>
                <Input
                  id="fixture-description"
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                  placeholder="Signed in as an admin user"
                />
              </div>
              <Button type="submit" disabled={registerFixture.isPending || !NAME_PATTERN.test(name)}>
                {registerFixture.isPending ? 'Registering…' : 'Register'}
              </Button>
            </form>
          </CardContent>
        </Card>
      )}

      {fixtures.isLoading && <p className="text-muted-foreground text-sm">Loading fixtures…</p>}
      {fixtures.isError && <p className="text-sm text-red-600">Could not load fixtures. Try refreshing the page.</p>}

      {fixtures.data && fixtures.data.fixtures.length === 0 && (
        <p className="text-muted-foreground text-sm">No fixtures registered yet.</p>
      )}

      {fixtures.data && fixtures.data.fixtures.length > 0 && (
        <ul className="divide-border border-border divide-y rounded-lg border">
          {fixtures.data.fixtures.map((fixture) => (
            <li key={fixture.name} className="flex items-center justify-between gap-3 px-4 py-3">
              <div className="space-y-0.5">
                <p className="font-mono text-sm">{fixture.reference}</p>
                {fixture.description && <p className="text-muted-foreground text-sm">{fixture.description}</p>}
                <p className="text-muted-foreground text-xs">
                  Registered {new Date(fixture.createdAt).toLocaleString()}
                </p>
              </div>
              {canManage && (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => handleDelete(fixture.name)}
                  disabled={deleteFixture.isPending}
                >
                  Remove
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
