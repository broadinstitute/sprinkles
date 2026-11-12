# Retry and Resubmission

How Sprinkles handles submitting a job that has already been run (fully, partially, or not at all), and why it works this way. For the endpoint and field reference see [`design/dashboard-api.md`](design/dashboard-api.md) and [`design/datamodel.md`](design/datamodel.md).

## The problem

Firestore records are transient: jobs, tasks and summaries expire after 7 days. GCS is the only durable record of what ran. People routinely want to "run this job again" and expect:

- tasks that already succeeded, or are still in flight, are **not** redone;
- tasks that errored, failed or were killed **are** redone;
- this works whether or not the original job's Firestore records still exist.

## Core idea: identity is the result path

Instead of inventing a cache key (a hash of the inputs, or a user-supplied token), the GCS result path is the identity of both jobs and tasks.

- A job's `result_path` defaults to `<gcs_prefix>/<job name>` (or can be set explicitly with `resultPath`).
- A task's `result_path` is `<job result_path>/<task index>` (an explicit task `destination` still wins).
- Two submissions with the same result path are "the same job". Two tasks writing to the same result path are "the same task".

**Why:** the result path is already where outputs live, so it is the one thing that is both durable (survives Firestore expiry) and already unique per piece of work. A hash of inputs would not be recoverable from GCS after the Firestore records are gone, and would add a second identity to keep consistent with the output location. Before this change task paths were already derived from the job name, so the name was the effective identity (and two same-named jobs silently overwrote each other); this makes that explicit and extends it to jobs.

**Consequence:** submitting a new job under a name whose outputs still exist in GCS will skip tasks that already succeeded there, even if their commands changed. Use a different name or `resultPath` for genuinely different work. Nothing tries to detect "safe" versus "unsafe" differences; that is the submitter's responsibility.

## Two cases

### Phase 1: the job still exists in Firestore (retry)

`POST /api/v1/job` looks for an unexpired job with the same `result_path`. If found, the submission is a retry:

1. **Compatibility check.** The number of tasks and each task's `command` must match the existing task at the same index, otherwise `409 CONFLICT`. Nothing else is compared. Differences in workpool, resources, labels, image or files are ignored on a retry (the existing records win).
2. **Reset.** Tasks in `error`, `failed` or `killed` go back to `pending`. `success` tasks are never touched, and neither are `pending` or in-flight (`claimed`/`running`/`writing`) tasks, so retrying a running job is safe: it returns the job's ID and resets any failed tasks.
3. **Housekeeping.** `job_summary.json` is deleted from GCS, expiry of the job and all its tasks is extended by 7 days, and `job_created` is published so the monitor provisions workers.
4. The existing job's ID is returned (`retried: true`, `resetTasks: N`).

The same logic is available directly as `POST /api/v1/job/{job_id}/retry` (and the "Retry failed tasks" button on the job page).

### Phase 2: the job no longer exists (resume)

If no live job matches, a new job is created as usual. Avoiding repeated work then happens per task, in the worker:

1. After claiming a task, before downloading any inputs, the worker reads `<task result_path>/task_summary.json`.
2. If it exists, parses, and has `exit_code == 0`, the worker does not run the task. It moves it `claimed` → `success` and sets `found_prior_result`.
3. Anything else (missing, unreadable, corrupt, non-zero exit code) means the task runs normally. The check never fails a task.

`task_summary.json` (`{"exit_code": N, "finished_at": ...}`) is written by the worker as the **last** upload of a run, after the log and all output files, so its presence means the outputs are complete. A task killed or failed mid-upload has no summary and is rerun. It is written for any run that reached an exit code (so GCS also records failures), but not for infrastructure failures or kills. Success is defined purely as `exit_code == 0`.

## Implementation details

### Data model

- `Job.result_path` is always populated for jobs created through the API (it was previously optional, and only controlled where `job_summary.json` was written; a trailing `/` is trimmed). `job_summary.json` is therefore now written for every job. Lookup is a single-field query on `result_path`, which needs no composite index; expiry is filtered in memory, and if several live jobs share a path the newest `created_at` wins.
- `Task.found_prior_result` (bool) is the only new task field.
- The worker's `task_summary.json` is `{"exit_code": <int>, "finished_at": "<RFC3339>"}`. If the container was killed or the docker command failed without an exit code (an infrastructure failure), nothing is written.
- New state transitions: `claimed` → `success` (prior result found) and `error`/`failed`/`killed` → `pending` (retry). Both are documented in `datamodel.md`.

A **deleted** job (`JobSummary.state = deleted`; see `datamodel.md`) is never retried. Both entry points refuse it before anything is reset: `POST /api/v1/job/{job_id}/retry` returns `409`, and `POST /api/v1/job` ignores deleted jobs when looking for a live job with the same result path, so the same submission creates a **new** job instead (which then finds and skips any tasks whose `task_summary.json` already reports success). This is what makes "delete" mean something different from "hide": it cannot be undone, and resubmitting never revives it.

### `retryJob`, in order

1. List the job's tasks and select those in `error`, `failed` or `killed`.
2. If any, delete `<job result_path>/job_summary.json` (a missing object is fine; a real GCS error fails the request). This is done **before** touching any task: if it fails nothing has changed, and if a later step fails a repeat call deletes it again. Retrying a job with nothing to reset leaves its summary alone, since it is still accurate.
3. Reset the tasks in transactions of at most 200. Each transaction re-reads the tasks and skips any whose status is no longer retryable (a worker may have changed it). A reset sets `status=pending` and clears `failure_reason`, `exit_code`, `owning_worker_id` and `found_prior_result`. A `task_state_update` event is published for each task after its chunk commits.
4. If any task was reset: extend expiry on the job and on **all** of its tasks (not just the reset ones, so successful tasks don't expire out from under a revived job), then publish `job_created`.
5. If the job's workpool is `halted`, return a `warning`: the tasks are queued but won't run until the pool is reset.

Zero tasks reset is a success, not an error. Orphan recovery (the monitor resetting active tasks of dead workers) is separate and unchanged.

### Worker

The prior-result check runs at the start of `executeTaskBody`, before files are localized, so a skipped task costs one small GCS read and no downloads. It runs for every task in every job (there is no per-task "resume" flag, since with result-path identity any new job is a potential resume). The skip is reported to the main loop with a sentinel error so the completion handler records `claimed` → `success` and `found_prior_result` through `RecordPriorResultSuccess`, instead of the normal `writing` → `success`. `task_summary.json` is uploaded after `uploadResults` completes without error. A task with an empty `ResultPath` never uses the check.

The dashboard backend creates its GCS client lazily on first retry, so it can run (and serve everything else) without GCS credentials.

## Decisions and their reasons

| Decision | Reason |
| --- | --- |
| Identity is the result path, not a hash or explicit key | Durable, already unique, nothing new to keep consistent (see above). |
| Task paths nest under the job path | One identity scheme; `job_summary.json` and task outputs live together. |
| Phase 2 is done by the worker, per task | Smallest change; reuses the normal claim/execute path. The cost is that a VM may be provisioned just to discover no work is needed. Short-circuiting whole completed jobs at submit is a possible later optimisation (using `job_summary.json`). |
| Phase 2 always creates a new job and new ID | There is no old record to reuse. The work avoidance is at task granularity. |
| Retry resets `killed` as well as `error`/`failed` | Resubmitting a job you killed should bring it back to life. There is no "recently killed" guard. |
| Deleted jobs are never retried or matched by resubmission | Deleting is final. A resubmission after a delete starts a fresh job rather than reviving the one the user removed, and the delete endpoint's task check means a job can't be deleted while a retry's tasks are still pending. |
| No force-rerun flag | To rerun everything, kill the job and delete its GCS files. Keeps the API small; the destructive step is explicit. |
| Compatibility check is only task count + commands, and only on retry | Cheap and catches the obvious mistake (a different job with the same name). Anything finer-grained is the submitter's problem. |
| Success is recorded as an exit code, not a status word | One simple, unambiguous rule, and the file also records failures. |
| `task_summary.json` written last | Makes "file exists" imply "outputs complete" without needing atomic directory uploads. |
| Only `found_prior_result` (a bool), no provenance | Task IDs are transient, so recording which run produced the result isn't useful. |
| `job_summary.json` deleted on retry | So GCS never says "complete" for a job Firestore says is running (and so future whole-job short-circuiting can trust it). |
| Expiry extended on retry (for all of the job's tasks) | A revived job must not be garbage-collected mid-run, and its successful tasks must outlive it. |
| Resets chunked (200 per transaction), request blocks | Firestore transactions are capped at 500 writes and jobs can exceed that, but jobs are expected to stay under about 2,000 tasks, so blocking is fine. Resets are idempotent: if a retry fails part-way, repeating it finishes the job. |
| One `task_state_update` event per reset task | Consistent with every other transition; the volume is acceptable. |
| Any API key may retry any job | No per-user ownership model exists today. |
| Concurrent identical submissions are not serialized | Accepted race: both could create a job. Locking was judged not worth the complexity. |
| Backend needs GCS delete access on the prefix | Required to delete `job_summary.json`; granted to the dashboard backend's service account. |

## Who owns what

`JobSummary` is written only by the submit path (at creation) and the **monitor**; the retry code deliberately does not touch it. After resetting tasks the backend publishes `job_created`; on that event the monitor recomputes the job's summary if it is terminal, bringing it back to `pending`/`in_progress` (the regular summary poll only looks at non-terminal summaries, so a terminal one would otherwise never be revisited). The job then terminates again normally, publishing a second `job_terminated` and rewriting `job_summary.json`.

**Why not have retry flip the summary itself:** to keep a single writer for `JobSummary` and keep reasoning about it simple. The trade-off is that summaries may be briefly stale, and if the monitor misses the (non-durable) event a retried job's summary stays terminal while its tasks run, until another `job_created` for it arrives (for example by retrying again). That staleness was judged acceptable.

## Known limitations

- A same-named new job can skip tasks based on leftover outputs, even if the work changed (see Core idea).
- A user task that writes its own `task_summary.json` has it overwritten by the worker's.
- Retrying ignores any change in workpool, resources or other settings; to change those, use a new name or result path.
- Phase 2 may provision workers only for them to skip every task.
- A retried job's `JobSummary` can lag or, if the monitor misses the event, stay stale.
- Retry only works inside the Firestore expiry window; after that, resubmission is Phase 2.

## Testing

- Unit (`cli/worker_retry_test.go`): `task_summary.json` is written on success and on a non-zero exit, and not on an infrastructure failure; a prior summary with exit 0 skips execution and sets `found_prior_result`; a prior summary with a non-zero exit code, or a corrupt one, runs the task normally.
- Functional, emulator-based (`cli/functest/retry_test.go`): the retry endpoint resets only error/failed/killed tasks, clears their failure fields, extends expiry, deletes `job_summary.json`, leaves `JobSummary` untouched, and is a no-op when repeated; resubmitting the same name retries the same job; a changed command or task count is a `409`; a retry for a nonexistent job is a `404`.
- Functional (`cli/monitor/functest`, `TestReviveJobSummary`): a terminal summary whose tasks were reset is recomputed to `pending` with its expiry extended; one whose tasks are still terminal stays terminal; a missing summary is a no-op.

## Future work

- **Whole-job short circuit.** Before creating anything, test whether the job already completed in full (using `job_summary.json`) and, if so, create a synthetic completed job with no tasks; or create tasks only for those that did not finish. This avoids provisioning workers just to skip every task. It relies on `job_summary.json` being trustworthy, which is why retry deletes it.
- Mid-task checkpoint/resume, which is separate from this (`task_summary.json` is a prerequisite for it, not a substitute).
- Migrating tasks between workpools on retry. Today a retry keeps the existing tasks and workpool.

## Code map

- Submit/retry endpoints, compatibility check: `cli/dev/dashboard_backend.go` (`handleSubmitJob`, `retryJob`, `handleRetryJob`).
- Task reset and expiry extension: `cli/retry.go`.
- `task_summary.json` writing and the prior-result check: `cli/worker.go` (`uploadTaskSummary`, `priorResultSucceeded`); `RecordPriorResultSuccess` in `cli/task_queue.go`.
- Summary revival: `reviveJobSummary` in `cli/monitor/job_summary_poll.go`, called from `cli/monitor/monitor.go`.
- `job_summary.json` deletion: `DeleteJobSummary` in `cli/job_result_writer.go`.
