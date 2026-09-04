/**
 * Shared UI primitives.
 *
 * Stage-1 placeholder — see README.md.
 */

/** Design tokens the run view and the report share. */
export const statusTone = {
  pass: 'emerald',
  fail: 'rose',
  skipped: 'slate',
  error: 'amber',
} as const;

export type StatusTone = (typeof statusTone)[keyof typeof statusTone];
