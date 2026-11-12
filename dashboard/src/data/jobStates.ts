/** Job-level state values: monitor.JobStatus on the Go side. (Distinct from
 * the per-task states, which are success/error/failed/killed/etc.) */
export type JobState =
  | "pending"
  | "in_progress"
  | "in_progress_with_error"
  | "in_progress_with_failure"
  | "success"
  | "error"
  | "failed"
  | "killed"
  | "deleted";

/** A job explicitly deleted by a user. Final: it never changes state again,
 * and it is hidden from the job list unless "Show deleted jobs" is checked. */
export const DELETED_JOB_STATE = "deleted";

// Mirrors monitor.IsTerminalJobStatus on the Go side. Deleted is terminal:
// it's what a finished job becomes when it is deleted.
export const TERMINAL_JOB_STATES: ReadonlySet<string> = new Set([
  "success",
  "error",
  "failed",
  "killed",
  DELETED_JOB_STATE,
]);

/** Mirrors monitor.CanDeleteJob: only a job that finished on its own can be
 * deleted (not one still running, and not one already deleted). */
export function canDeleteJob(state: string | undefined): boolean {
  return (
    state !== undefined &&
    TERMINAL_JOB_STATES.has(state) &&
    state !== DELETED_JOB_STATE
  );
}
