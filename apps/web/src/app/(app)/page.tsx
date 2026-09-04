'use client';

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useMe } from '@/lib/api/hooks/use-me';

export default function DashboardPage() {
  const { data: me } = useMe();
  const { activeOrg } = useActiveOrg();

  return (
    <div className="space-y-6">
      <div className="space-y-1">
        <h1 className="text-2xl font-semibold tracking-tight">Dashboard</h1>
        <p className="text-muted-foreground text-sm">
          {me ? `Welcome back, ${me.user.name}.` : 'Welcome back.'}
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>{activeOrg?.name ?? 'No organization selected'}</CardTitle>
          <CardDescription>
            {activeOrg
              ? `You are signed in as ${activeOrg.role}.`
              : 'Create or join an organization to get started.'}
          </CardDescription>
        </CardHeader>
        <CardContent>
          <p className="text-muted-foreground text-sm">
            Runs, test cases and reports will show up here — this stage only ships the app shell
            and organization management.
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
