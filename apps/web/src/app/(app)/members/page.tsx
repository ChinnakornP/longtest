'use client';

import { useState, type FormEvent } from 'react';
import { toast } from 'sonner';

import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { ApiError } from '@/lib/api/client';
import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useInviteMember, useMembers } from '@/lib/api/hooks/use-members';
import type { OrgRole } from '@/lib/api/types';
import { canManage } from '@/lib/auth/role';

const INVITE_ROLES: OrgRole[] = ['viewer', 'member', 'admin'];

export default function MembersPage() {
  const { activeOrg } = useActiveOrg();
  const orgId = activeOrg?.id ?? null;
  const members = useMembers(orgId);
  const invite = useInviteMember(orgId);

  const [email, setEmail] = useState('');
  const [role, setRole] = useState<OrgRole>('member');

  const canInvite = canManage(activeOrg?.role);

  const handleInvite = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    invite.mutate(
      { email, role },
      {
        onSuccess: () => {
          toast.success(`Invited ${email} as ${role}.`);
          setEmail('');
          setRole('member');
        },
        onError: (error) => {
          toast.error(error instanceof ApiError ? error.message : 'Could not send the invite.');
        },
      },
    );
  };

  return (
    <div className="space-y-6">
      <div className="space-y-1">
        <h1 className="text-2xl font-semibold tracking-tight">Members</h1>
        <p className="text-muted-foreground text-sm">
          {activeOrg ? `Members of ${activeOrg.name}.` : 'Select an organization.'}
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Team</CardTitle>
        </CardHeader>
        <CardContent>
          {members.isLoading && <p className="text-muted-foreground text-sm">Loading members…</p>}
          {members.isError && (
            <p className="text-destructive text-sm">Could not load members.</p>
          )}
          {members.data && members.data.length === 0 && (
            <p className="text-muted-foreground text-sm">No members yet.</p>
          )}
          {members.data && members.data.length > 0 && (
            <ul className="divide-border divide-y">
              {members.data.map((member) => (
                <li key={member.id} className="flex items-center justify-between py-3 text-sm">
                  <div>
                    <p className="font-medium">{member.name}</p>
                    <p className="text-muted-foreground">{member.email}</p>
                  </div>
                  <Badge variant="outline">{member.role}</Badge>
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>

      {canInvite && (
        <Card>
          <CardHeader>
            <CardTitle>Invite a member</CardTitle>
          </CardHeader>
          <CardContent>
            <form onSubmit={handleInvite} className="flex flex-col gap-4 sm:flex-row sm:items-end">
              <div className="flex-1 space-y-1.5">
                <Label htmlFor="invite-email">Email</Label>
                <Input
                  id="invite-email"
                  type="email"
                  required
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  placeholder="teammate@company.com"
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="invite-role">Role</Label>
                <select
                  id="invite-role"
                  value={role}
                  onChange={(e) => setRole(e.target.value as OrgRole)}
                  className="border-border h-10 rounded-lg border bg-transparent px-3 text-sm"
                >
                  {INVITE_ROLES.map((r) => (
                    <option key={r} value={r}>
                      {r}
                    </option>
                  ))}
                </select>
              </div>
              <Button type="submit" disabled={invite.isPending}>
                {invite.isPending ? 'Sending…' : 'Send invite'}
              </Button>
            </form>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
