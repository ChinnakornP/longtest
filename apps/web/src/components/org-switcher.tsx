'use client';

import { ChevronsUpDown } from 'lucide-react';

import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useOrgStore } from '@/lib/stores/org-store';

export function OrgSwitcher() {
  const { activeOrg, orgs } = useActiveOrg();
  const setActiveOrgId = useOrgStore((s) => s.setActiveOrgId);

  if (!activeOrg) return null;

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="outline" className="justify-between gap-2">
          <span className="max-w-40 truncate">{activeOrg.name}</span>
          <ChevronsUpDown className="size-4 shrink-0 opacity-60" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start">
        <DropdownMenuLabel>Switch organization</DropdownMenuLabel>
        <DropdownMenuSeparator />
        {orgs.map((org) => (
          <DropdownMenuItem
            key={org.id}
            onSelect={() => setActiveOrgId(org.id)}
            className="justify-between"
          >
            <span className="truncate">{org.name}</span>
            <Badge variant="outline">{org.role}</Badge>
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
