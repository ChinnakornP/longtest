import { render, screen } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useInviteMember, useMembers } from '@/lib/api/hooks/use-members';
import type { OrgRole } from '@/lib/api/types';

import MembersPage from './page';

vi.mock('@/lib/api/hooks/use-active-org');
vi.mock('@/lib/api/hooks/use-members');

const mockedUseActiveOrg = vi.mocked(useActiveOrg);
const mockedUseMembers = vi.mocked(useMembers);
const mockedUseInviteMember = vi.mocked(useInviteMember);

function setup(role: OrgRole) {
  mockedUseActiveOrg.mockReturnValue({
    activeOrg: { id: 'org-1', name: 'Acme', slug: 'acme', role },
    orgs: [],
    isLoading: false,
  });
  mockedUseMembers.mockReturnValue({
    data: [{ id: 'm1', userId: 'u1', email: 'owner@acme.test', name: 'Owner', role: 'owner' }],
    isLoading: false,
    isError: false,
  } as unknown as ReturnType<typeof useMembers>);
  mockedUseInviteMember.mockReturnValue({
    mutate: vi.fn(),
    isPending: false,
  } as unknown as ReturnType<typeof useInviteMember>);
}

describe('MembersPage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('hides write controls for a viewer', () => {
    setup('viewer');
    render(<MembersPage />);
    expect(screen.queryByRole('button', { name: /send invite/i })).not.toBeInTheDocument();
  });

  it('shows the invite form for an owner', () => {
    setup('owner');
    render(<MembersPage />);
    expect(screen.getByRole('button', { name: /send invite/i })).toBeInTheDocument();
  });
});
