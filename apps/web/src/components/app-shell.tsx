'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import type { ReactNode } from 'react';
import { toast } from 'sonner';

import { OrgSwitcher } from '@/components/org-switcher';
import { Button } from '@/components/ui/button';
import { useLogout } from '@/lib/api/hooks/use-auth';
import { useMe } from '@/lib/api/hooks/use-me';
import { ApiError } from '@/lib/api/client';
import { cn } from '@/lib/utils';

const NAV_LINKS = [
  { href: '/', label: 'Dashboard' },
  { href: '/members', label: 'Members' },
  { href: '/runtimes', label: 'Runtimes' },
];

export function AppShell({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const { data: me } = useMe();
  const logout = useLogout();

  const handleLogout = () => {
    logout.mutate(undefined, {
      onError: (error) => {
        toast.error(error instanceof ApiError ? error.message : 'Could not log out.');
      },
    });
  };

  return (
    <div className="min-h-screen">
      <header className="border-border flex items-center justify-between border-b px-6 py-4">
        <div className="flex items-center gap-6">
          <span className="text-sm font-semibold tracking-tight">AI QA Agent</span>
          <nav className="flex items-center gap-1">
            {NAV_LINKS.map((link) => (
              <Link
                key={link.href}
                href={link.href}
                className={cn(
                  'rounded-md px-3 py-1.5 text-sm transition-colors',
                  pathname === link.href
                    ? 'bg-muted text-foreground'
                    : 'text-muted-foreground hover:text-foreground',
                )}
              >
                {link.label}
              </Link>
            ))}
          </nav>
        </div>
        <div className="flex items-center gap-3">
          <OrgSwitcher />
          {me && <span className="text-muted-foreground hidden text-sm sm:inline">{me.user.email}</span>}
          <Button variant="outline" size="sm" onClick={handleLogout} disabled={logout.isPending}>
            {logout.isPending ? 'Logging out…' : 'Log out'}
          </Button>
        </div>
      </header>
      <main className="mx-auto max-w-4xl px-6 py-8">{children}</main>
    </div>
  );
}
