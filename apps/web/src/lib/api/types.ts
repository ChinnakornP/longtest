/**
 * Wire types for the auth/tenancy contract defined by T05
 * (server/internal/auth, server/internal/org). Hand-written because that
 * contract has no packages/qa-schema JSON Schema — qa-schema only covers the
 * QA domain (application map, test cases, findings, ...). Keep these in sync
 * with the T05 issue contract; do not fork a second copy elsewhere in web.
 */

export type OrgRole = 'viewer' | 'member' | 'admin' | 'owner';

export interface User {
  id: string;
  email: string;
  name: string;
}

export interface OrgSummary {
  id: string;
  name: string;
  slug: string;
  role: OrgRole;
}

export interface MeResponse {
  user: User;
  orgs: OrgSummary[];
}

export interface Org {
  id: string;
  name: string;
  slug: string;
}

export interface SignupRequest {
  email: string;
  password: string;
  name: string;
  orgName: string;
}

export interface SignupResponse {
  user: User;
  org: Org;
}

export interface LoginRequest {
  email: string;
  password: string;
}

export interface Member {
  id: string;
  userId: string;
  email: string;
  name: string;
  role: OrgRole;
}

export interface InviteRequest {
  email: string;
  role: OrgRole;
}

export interface Invite {
  id: string;
  orgId: string;
  email: string;
  role: OrgRole;
  createdAt: string;
  expiresAt: string;
}

export interface PairingCodeResponse {
  pairingCode: string;
  expiresAt: string;
}

export interface ApiErrorBody {
  error: {
    code: string;
    message: string;
    details?: unknown;
  };
}
