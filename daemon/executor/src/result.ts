/**
 * @fileoverview ExecutionResult assembly: the only path that produces an
 * `execution-result@1` envelope, plus the validation that proves it.
 *
 * The runner calls into this module as steps and assertions land; the
 * returned object is what the daemon forwards to the backend. Every value
 * we ship here goes through the QA-Schema validator before it leaves the
 * process, so a malformed result fails the test rather than reaching the
 * database.
 */

import type {
  Artifact,
  ArtifactId,
  AssertionResult,
  ExecutionResult,
  FailureClass,
  Outcome,
  StepResult,
} from '@qa/schema';
import { validate } from '@qa/schema';

export interface ResultBuilderState {
  testCaseId: string;
  runId?: string;
  attempt: number;
  startedAt: Date;
  endedAt: Date;
  steps: StepResult[];
  assertions: AssertionResult[];
  artifacts: Artifact[];
}

export class ResultBuilder {
  private readonly state: ResultBuilderState;

  constructor(input: { testCaseId: string; runId?: string; attempt?: number; startedAt: Date }) {
    this.state = {
      testCaseId: input.testCaseId,
      ...(input.runId !== undefined ? { runId: input.runId } : {}),
      attempt: input.attempt ?? 1,
      startedAt: input.startedAt,
      endedAt: input.startedAt,
      steps: [],
      assertions: [],
      artifacts: [],
    };
  }

  addStep(step: StepResult): void {
    this.state.steps.push(step);
  }

  addAssertion(assertion: AssertionResult): void {
    this.state.assertions.push(assertion);
  }

  addArtifact(artifact: Artifact): void {
    this.state.artifacts.push(artifact);
  }

  end(endedAt: Date): void {
    this.state.endedAt = endedAt;
  }

  /**
   * Compute the final result and validate it against the schema. The
   * validator runs once at the end — re-running per step would be paying
   * the same cost on every step just to catch the same bug.
   */
  finalise(input?: {
    failureClass?: FailureClass;
    message?: string;
  }): ExecutionResult {
    const effective: { failureClass?: FailureClass; message?: string } = input ?? {};
    const result = this.computeOutcome();
    const durationMs = Math.max(0, this.state.endedAt.getTime() - this.state.startedAt.getTime());
    const payload: ExecutionResult = {
      version: 1,
      testCaseId: this.state.testCaseId,
      ...(this.state.runId !== undefined ? { runId: this.state.runId } : {}),
      attempt: this.state.attempt,
      result,
      ...(effective.failureClass !== undefined ? { failureClass: effective.failureClass } : {}),
      ...(effective.message !== undefined ? { message: effective.message } : {}),
      steps: this.state.steps,
      ...(this.state.assertions.length > 0 ? { assertions: this.state.assertions } : {}),
      artifacts: this.state.artifacts,
      startedAt: this.state.startedAt.toISOString(),
      endedAt: this.state.endedAt.toISOString(),
      durationMs,
    };
    const validation = validate('execution-result@1', payload);
    if (!validation.valid) {
      // This is a programmer error: the executor produced a result that the
      // contract rejects. Surface it as a hard crash — the daemon will see
      // the exit code and treat the run as crashed, which is the right
      // answer because the data we promised to send is not safe to send.
      const lines = validation.errors
        .map((e) => `${e.instancePath || '/'}: ${e.message} [${e.keyword}]`)
        .join('\n');
      throw new Error(`executor produced an execution-result@1 that fails schema validation:\n${lines}`);
    }
    return payload;
  }

  /**
   * Outcome precedence: any `error` step ⇒ error, else any `fail` step or
   * assertion ⇒ fail, else skipped if no steps ran, else pass.
   */
  private computeOutcome(): Outcome {
    if (this.state.steps.some((s) => s.status === 'error')) return 'error';
    if (this.state.steps.some((s) => s.status === 'fail')) return 'fail';
    if (this.state.assertions.some((a) => a.status === 'fail')) return 'fail';
    if (this.state.steps.some((s) => s.status === 'skipped')) return 'skipped';
    if (this.state.steps.length === 0 && this.state.assertions.length === 0) return 'skipped';
    return 'pass';
  }

  /** Read-only view for diagnostics. */
  snapshot(): ResultBuilderState {
    return {
      ...this.state,
      steps: this.state.steps.slice(),
      assertions: this.state.assertions.slice(),
      artifacts: this.state.artifacts.slice(),
    };
  }
}

/** Convenience constructor for empty step/assertion results. */
export function emptyStepResult(index: number, action: StepResult['action'], status: Outcome): StepResult {
  return { index, action, status };
}

export function emptyAssertionResult(index: number, type: AssertionResult['type'], status: Outcome): AssertionResult {
  return { index, type, status };
}

/** Collect a list of artifact ids that have been registered so far. */
export function knownArtifactIds(artifacts: readonly Artifact[]): Set<ArtifactId> {
  return new Set(artifacts.map((a) => a.id));
}
