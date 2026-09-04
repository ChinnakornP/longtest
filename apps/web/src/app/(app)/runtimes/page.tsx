'use client';

import { toast } from 'sonner';

import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { ApiError } from '@/lib/api/client';
import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useRequestPairingCode } from '@/lib/api/hooks/use-runtimes';
import { canManage } from '@/lib/auth/role';

export default function RuntimesPage() {
  const { activeOrg } = useActiveOrg();
  const pairing = useRequestPairingCode(activeOrg?.id ?? null);
  const canRequest = canManage(activeOrg?.role);

  const handleRequestCode = () => {
    pairing.mutate(undefined, {
      onError: (error) => {
        toast.error(error instanceof ApiError ? error.message : 'Could not request a pairing code.');
      },
    });
  };

  return (
    <div className="space-y-6">
      <div className="space-y-1">
        <h1 className="text-2xl font-semibold tracking-tight">Runtimes</h1>
        <p className="text-muted-foreground text-sm">
          Pair a machine so its daemon can run tests for {activeOrg?.name ?? 'this organization'}.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Pair a new runtime</CardTitle>
          <CardDescription>
            The code is one-time use and expires 15 minutes after it&apos;s issued.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {canRequest && (
            <Button onClick={handleRequestCode} disabled={pairing.isPending}>
              {pairing.isPending ? 'Requesting…' : 'Request pairing code'}
            </Button>
          )}
          {!canRequest && (
            <p className="text-muted-foreground text-sm">
              Only an admin or owner can pair a new runtime.
            </p>
          )}

          {pairing.data && (
            <div className="border-border space-y-3 rounded-lg border p-4">
              <div>
                <p className="text-muted-foreground text-xs">Pairing code</p>
                <p className="font-mono text-2xl tracking-wider">{pairing.data.pairingCode}</p>
                <p className="text-muted-foreground text-xs">
                  Expires at {new Date(pairing.data.expiresAt).toLocaleTimeString()}
                </p>
              </div>
              <div className="space-y-1.5">
                <p className="text-muted-foreground text-xs">On the target machine, run:</p>
                <pre className="bg-muted overflow-x-auto rounded-md p-3 text-xs">
                  {`qa-agent setup --pairing-code ${pairing.data.pairingCode}`}
                </pre>
              </div>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
