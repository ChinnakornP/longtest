'use client';

import Link from 'next/link';
import { useParams, usePathname } from 'next/navigation';
import type { ReactNode } from 'react';

import { cn } from '@/lib/utils';

export default function ProjectLayout({ children }: { children: ReactNode }) {
  const params = useParams<{ projectId: string }>();
  const pathname = usePathname();
  const base = `/projects/${params.projectId}`;

  const tabs = [
    { href: base, label: 'Overview' },
    { href: `${base}/test-cases`, label: 'Test cases' },
    { href: `${base}/coverage`, label: 'Coverage' },
    { href: `${base}/fixtures`, label: 'Fixtures' },
  ];

  return (
    <div className="space-y-6">
      <nav className="border-border flex items-center gap-1 border-b pb-2">
        {tabs.map((tab) => {
          const active = tab.href === base ? pathname === base : pathname.startsWith(tab.href);
          return (
            <Link
              key={tab.href}
              href={tab.href}
              className={cn(
                'rounded-md px-3 py-1.5 text-sm transition-colors',
                active ? 'bg-muted text-foreground' : 'text-muted-foreground hover:text-foreground',
              )}
            >
              {tab.label}
            </Link>
          );
        })}
      </nav>
      {children}
    </div>
  );
}
