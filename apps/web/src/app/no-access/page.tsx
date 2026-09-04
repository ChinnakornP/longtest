import Link from 'next/link';

import { Button } from '@/components/ui/button';

export default function NoAccessPage() {
  return (
    <main className="mx-auto flex min-h-screen max-w-md flex-col items-center justify-center gap-4 px-6 text-center">
      <h1 className="text-2xl font-semibold tracking-tight">No access</h1>
      <p className="text-muted-foreground text-sm">
        You don&apos;t have access to this organization, or it no longer exists. Switch
        organizations or sign in again.
      </p>
      <Button asChild>
        <Link href="/">Back to dashboard</Link>
      </Button>
    </main>
  );
}
