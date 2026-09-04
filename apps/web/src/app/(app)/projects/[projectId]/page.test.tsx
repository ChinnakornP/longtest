import { render, screen } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { useActiveOrg } from '@/lib/api/hooks/use-active-org';
import { useProject } from '@/lib/api/hooks/use-projects';
import { useCreateRun } from '@/lib/api/hooks/use-runs';
import { useRuntimes } from '@/lib/api/hooks/use-runtimes';

import ProjectDetailPage from './page';

vi.mock('next/navigation', () => ({
  useParams: () => ({ projectId: 'proj-1' }),
  useRouter: () => ({ push: vi.fn() }),
}));
vi.mock('@/lib/api/hooks/use-active-org');
vi.mock('@/lib/api/hooks/use-projects');
vi.mock('@/lib/api/hooks/use-runs');
vi.mock('@/lib/api/hooks/use-runtimes');

const mockedUseActiveOrg = vi.mocked(useActiveOrg);
const mockedUseProject = vi.mocked(useProject);
const mockedUseCreateRun = vi.mocked(useCreateRun);
const mockedUseRuntimes = vi.mocked(useRuntimes);

function setup(runtimes: Array<{ online: boolean }>) {
  mockedUseActiveOrg.mockReturnValue({
    activeOrg: { id: 'org-1', name: 'Acme', slug: 'acme', role: 'member' },
    orgs: [],
    isLoading: false,
  });
  mockedUseProject.mockReturnValue({
    data: { id: 'proj-1', orgId: 'org-1', name: 'Acme site', baseUrl: 'https://acme.test', createdAt: 't' },
    isLoading: false,
    isError: false,
  } as unknown as ReturnType<typeof useProject>);
  mockedUseCreateRun.mockReturnValue({ mutate: vi.fn(), isPending: false } as unknown as ReturnType<
    typeof useCreateRun
  >);
  mockedUseRuntimes.mockReturnValue({
    data: runtimes.map((r, i) => ({
      id: `rt-${i}`,
      name: `runtime-${i}`,
      online: r.online,
      lastSeenAt: null,
      browsers: ['chromium'],
      agents: [],
    })),
    isLoading: false,
    isError: false,
  } as unknown as ReturnType<typeof useRuntimes>);
}

describe('ProjectDetailPage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
  });

  it('explains that no runtime is paired instead of leaving a stuck spinner', () => {
    setup([]);
    render(<ProjectDetailPage />);
    expect(screen.getByText(/no runtime is paired/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /start testing/i })).toBeDisabled();
  });

  it('explains that every paired runtime is offline', () => {
    setup([{ online: false }, { online: false }]);
    render(<ProjectDetailPage />);
    expect(screen.getByText(/every paired runtime is offline/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /start testing/i })).toBeDisabled();
  });

  it('lets an online runtime be picked and enables Start Testing', () => {
    setup([{ online: true }]);
    render(<ProjectDetailPage />);
    expect(screen.queryByText(/offline/i)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /start testing/i })).toBeEnabled();
  });
});
