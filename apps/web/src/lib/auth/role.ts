import type { OrgRole } from '@/lib/api/types';

/** member | admin | owner can create/run things; viewer is read-only. */
export function canWrite(role: OrgRole | undefined): boolean {
  return role === 'member' || role === 'admin' || role === 'owner';
}

/** Only admin | owner manage members and runtimes, per the T05 contract. */
export function canManage(role: OrgRole | undefined): boolean {
  return role === 'admin' || role === 'owner';
}
