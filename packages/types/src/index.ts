/**
 * Shared, non-wire TypeScript types.
 *
 * Stage-1 placeholder. Wire contracts live in `@qa/schema`.
 */

/** Lifecycle of a test run, mirrored from the backend. */
export type RunStatus = 'queued' | 'assigned' | 'running' | 'succeeded' | 'failed' | 'cancelled';

/** Pipeline phase a run is currently in. */
export type RunPhase = 'discover' | 'analyze' | 'plan' | 'execute' | 'report';

/** Mode requested when a run is created. */
export type RunMode = 'discover' | 'plan' | 'execute' | 'full';
