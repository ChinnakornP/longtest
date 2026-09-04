'use client';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { useRouter } from 'next/navigation';
import { useEffect, useState, type ReactNode } from 'react';
import { Toaster } from 'sonner';

import { authEvents } from '@/lib/api/auth-events';

function AuthEventBridge({ queryClient }: { queryClient: QueryClient }) {
  const router = useRouter();

  useEffect(
    () =>
      authEvents.subscribe((event) => {
        if (event === 'unauthenticated') {
          queryClient.clear();
          router.replace('/login');
        } else if (event === 'forbidden') {
          router.replace('/no-access');
        }
      }),
    [queryClient, router],
  );

  return null;
}

export function Providers({ children }: { children: ReactNode }) {
  const [queryClient] = useState(
    () =>
      new QueryClient({
        defaultOptions: { queries: { staleTime: 30_000, refetchOnWindowFocus: false } },
      }),
  );

  return (
    <QueryClientProvider client={queryClient}>
      <AuthEventBridge queryClient={queryClient} />
      {children}
      <Toaster position="top-right" richColors />
    </QueryClientProvider>
  );
}
