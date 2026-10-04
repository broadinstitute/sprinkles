# Retry / Resume of Previously Submitted Jobs

Status: v3, ready to implement. All review questions are resolved (section 8).

## 1. Decisions (from review)

1. **Identity = result path.** A job's identity is its `ResultPath`. A task's identity is its task `ResultPath`. If the submitter doesn't give a job `resultPath`, it defaults to `<config.GCSPrefix>/<job name>`. This matches how task result paths are already built (`<GCSPrefix>/<job name>/<task index>`, `cli/dev/dashboard_backend.go:1022`). There is no separate cache key and no input hash.
2. **Worker does the Phase 2 check.**
3. **Retry resets `error`, `failed` and `killed` tasks.** A killed job comes back to life when the same job is resubmitted; there is no "recently killed" guard. It never touches `success`, `pending`, `claimed`, `running` or `writing`.
4. **Compatibility check on resubmission.** Reject with `409` if the number of tasks differs, or if any task's `command` differs from the existing task at the same index. Nothing else is checked. Differences in workpool, resources, image, files and labels are the submitter's problem, and on a retry they are ignored (the existing records are kept).
5. **No force flag.** To force a full rerun, the user kills the existing job and deletes the GCS files.
6. **Success is `exit_code == 0` in a `task_summary.json` written by the task** (see 3.3).
7. **`FoundPriorResult` is a bare bool.** No provenance.
8. **Retry deletes `job_summary.json`** so GCS matches Firestore.
9. **Retrying a running or pending job is allowed.** It resets any terminal-failure tasks and returns the existing job ID.
10. **Any API key may retry any job.**
11. **Jobs may have more than 500 tasks**, so resets must be chunked.
12. **Retry extends `expiry`** on the Job, its Tasks and the JobSummary.
13. **Task result path default nests under the job's `ResultPath`**: `<job ResultPath>/<task index>`. An explicit task `destination` always wins.
14. **The retry request blocks until all resets are done.** Realistic job size is under ~2,000 tasks. Chunking is still required (the transaction limit is 500 writes).
15. **One `task_state_update` event per reset task** is acceptable.

## 2. Current-state facts the design relies on

- `Job.ResultPath` already exists (`cli/task_queue.go:159`, firestore `result_path`) and is accepted by the API as `resultPath`. Today it is optional, and only controls where the monitor writes `job_summary.json` (`cli/job_result_writer.go`); empty means no summary. `docs/design/datamodel.md` doesn't list the field; fix that.
- Task result paths are always set at submit. A task's `destination` overrides the default.
- Today the worker uploads the task's working-dir files plus the log (`uploadResults`, `cli/worker.go:698`). It writes **no file recording the exit code**. Exit status is only in Firestore (`RecordError`, the `writing` -> `success` transition), which expires. So there is nothing for Phase 2 to read yet (answer to your Q7: no such file exists today; section 3.3 adds one).
- The monitor's `runJobSummaryPoll` only recomputes jobs whose `JobSummary` is non-terminal (`ListNonTerminal`, `cli/monitor/adapters.go:649`). `datamodel.md` says only submit (at creation) and the monitor write `JobSummary`.
- Orphan recovery (`monitor/requeue_orphaned.go`) already resets active tasks of dead workers to `pending`. Retry does not duplicate that.
- All documents carry a 7-day `expiry`, and the expiry cleaner deletes them.
- `handleSubmitJob` always rewrites the `WorkPools` document and creates a new Job, tasks, JobSummary and a `job_created` event.

## 3. Design

### 3.1 Data model changes

`Job`:
- `ResultPath` becomes always populated for jobs created through the API: the supplied `resultPath`, trimmed of a trailing `/`, or `<config.GCSPrefix>/<name>`. Because every job now has one, `job_summary.json` is now always written.
- Needs a Firestore query on `result_path`. A single-field index is automatic.
- Task result path default changes from `<GCSPrefix>/<name>/<index>` to `<job ResultPath>/<index>` (in `handleSubmitJob`, `dashboard_backend.go:1022`). When the job `resultPath` is not supplied the two are identical, so existing behavior is unchanged. A task's explicit `destination` still overrides it, and `LogPath` stays `<task ResultPath>/stdout.txt`.

`Task`:
- `found_prior_result bool` (`firestore:"found_prior_result" json:"found_prior_result,omitempty"`): true only when the worker adopted an existing `task_summary.json` instead of running the task.

No other new fields.

### 3.2 Submit endpoint (`handleSubmitJob`)

After the existing validation, and before the workpool is written:

1. Compute `resultPath` as in 3.1.
2. Query `Jobs` for `result_path == resultPath` and `expiry > now`. If there is more than one match, take the one with the latest `created_at`, sorting in memory.
3. **Found (Phase 1, retry):**
   - List the job's tasks ordered by `task_index`. Return `409` (code `CONFLICT`) with a message naming the mismatch if the count differs from `len(req.Tasks)` or any command differs by index.
   - Call `retryJob` (3.4).
   - **Do not** write the `WorkPools` document, a new Job, new tasks or a new JobSummary.
   - Return `{"id": <existing job id>, "retried": true, "resetTasks": N}`.
4. **Not found (Phase 2, resume):** create the job as today, with `ResultPath` set. Nothing else differs at submit time. The work avoidance happens in the worker (3.5).

Update `submitJobResponse`, `docs/openapi.yaml` (new response fields, the `409`, and the `resultPath` default), and the CLI output in `cli/submit_cmd.go` (e.g. "Retrying existing job <id>: N tasks reset").

### 3.3 `task_summary.json` (written by the task run)

The worker writes `<task ResultPath>/task_summary.json` as the **last** upload in `uploadResults`, after the log and every working-dir file. Written for every completed run (any exit code), so it can't appear before the outputs are complete.

```json
{ "exit_code": 0, "finished_at": "<RFC3339>" }
```

- `uploadResults` needs the exit code as a new parameter. Take it from the docker execution result (`ru.ExitCode` or the `*exec.ExitError`, see worker.go around lines 336-350).
- If the container was killed or the docker command failed without an exit code (infrastructure failure), do not write `task_summary.json`.
- A user task could also produce a file named `task_summary.json` in its working dir. The worker's file is uploaded last and overwrites it. Flagged as Q4.

### 3.4 `retryJob(ctx, job)`

Shared by resubmission and a new endpoint `POST /api/v1/job/{id}/retry` (this also covers the "retry button" item in `open-issues.md`; `404` if the job doesn't exist or has expired).

1. Query the job's tasks with `status in [error, failed, killed]`.
2. Reset them in chunks of at most 200 tasks per Firestore transaction (the transaction limit is 500 writes). Within each transaction, re-read each task, skip it if its status is no longer in the retry set, and otherwise set: `status=pending`, `failure_reason=""`, `exit_code=0`, `owning_worker_id=""`, `found_prior_result=false`, `last_updated=now`, `expiry=now+7d`. Chunks are independent, so if one fails, return an error; calling retry again completes the remainder (idempotent).
3. After each chunk commits, publish a `task_state_update` event per reset task (`old_state` = previous status, `new_state` = `pending`). Add this transition to the table in `datamodel.md`.
4. Extend `expiry` (`now+7d`) on the Job and the JobSummary.
5. Delete `<job.ResultPath>/job_summary.json` (the dashboard backend needs a GCS client with delete permission on `GCSPrefix`: add to its service account's IAM and to the deploy docs; ignore "not found"; a real GCS error fails the request, as leaving a stale summary defeats the purpose).
6. Make the JobSummary reflect the reset (3.6).
7. Publish a `job_created` event so the monitor polls and provisions workers promptly. If the workpool is `halted`, include a warning in the response; the tasks are queued but won't run until the pool is unhalted.
8. Return the number of tasks reset. If zero, that's success (e.g. a running job), not an error.

### 3.5 Phase 2: worker-side prior-result check

In `executeTaskBody` (`cli/worker.go`), right after the task is claimed and **before** `resolveFilesToLocalize`/`prepareWorkDir`:

1. Try to read `<t.ResultPath>/task_summary.json` through the `TransferClient` (add a read/exists method if there isn't one).
2. If it exists, parses, and `exit_code == 0`: set `found_prior_result=true` and move the task `claimed -> success` through a new `TaskQueue` method (e.g. `RecordPriorResultSuccess`), which also clears `owning_worker_id` and publishes the `task_state_update` event. Return without running anything.
3. Otherwise (missing, unreadable, corrupt, or non-zero exit code), log the reason and run the task normally. Never fail the task because of the check.

The check runs for every task, in every job. There is no `resume` flag, because with result-path identity every freshly created job is a potential resume. Cost is one small GCS read per task.

`claimed -> success` is a new transition; add it to `datamodel.md` and make sure the dashboard and monitor don't assume `success` always follows `writing`.

### 3.6 Reviving the JobSummary

When tasks are reset, the JobSummary for a terminal job must become non-terminal, or the monitor will never recompute it. **As implemented**, this is entirely the monitor's job; the dashboard backend never writes `JobSummary` (decision from review: keep the "only the monitor writes JobSummary" rule, and accept that summaries may be briefly stale). `retryJob` publishes `job_created` after the task resets. On that event the monitor (`reviveJobSummary`, `monitor/job_summary_poll.go`, called from the job-event router in `monitor.go`) loads the job's `JobSummary`; if it is terminal it extends its expiry and recomputes it from the current task counts via `updateJobSummary`, which brings it back to `pending`/`in_progress`. The job then re-terminates normally later, publishing a second `job_terminated` event, rewriting `job_summary.json`, and appending to `JobSummaryHistory`. A missing or already non-terminal summary (the normal case for a brand-new job) is left to the regular poll. Known limitation: the event is not durable, so if the monitor misses it (down, or Pub/Sub drop) the summary of a retried job stays terminal (stale) even though its tasks run, until another `job_created` for that job is received, e.g. by retrying again.

Other implementation notes: `job_summary.json` is deleted only when at least one task will be reset (so a retry that finds nothing to do on a successful job leaves its summary alone), and before any task is touched; task `expiry` is extended for *all* of the job's tasks, not just the reset ones, so a revived job isn't left with expired successful tasks. The GCS client in the dashboard backend is created lazily on first retry.

### 3.7 Dashboard and docs

- `dashboard/src/types.ts` and the task detail page: show `found_prior_result`.
- Job detail page: "Retry" button calling the new endpoint.
- Update `docs/openapi.yaml`, `docs/design/dashboard-api.md`, `docs/design/datamodel.md` (Jobs `result_path`, Tasks `found_prior_result`, `task_summary.json`, the two new transitions, the summary-revival rule).

## 4. Implementation order

1. Docs fixes; `found_prior_result` field; default and always-set `ResultPath`.
2. `task_summary.json` writing in the worker (must land before step 5 or Phase 2 never finds anything).
3. `retryJob`, the retry endpoint, event handling that revives the summary.
4. Submit-time lookup and the 409 check (Phase 1).
5. Worker-side prior-result check (Phase 2).
6. Dashboard, CLI output.

## 5. Testing

- `retryJob`: each starting status (`success`, `error`, `failed`, `killed`, `pending`, active); more than 200 tasks; a task changing status between the query and the transaction; calling it twice.
- Submit: same name twice -> same ID and no second Job; changed command or task count -> 409; unrelated differences -> still a retry; expired match -> new Job.
- Worker: `task_summary.json` present with exit 0 (adopted, command not run); exit non-zero, missing, corrupt (runs normally).
- Monitor: terminal summary revived by the event, then re-terminates; `job_summary.json` deleted on retry and rewritten after.
- Functional (`cli/functest`, using the GCS emulator): job with a failing task, resubmit, only that task reruns.

## 6. Known consequences (accepted per the review decisions)

- A new job with the same name as an old one whose outputs are still in GCS will skip any task whose `task_summary.json` says exit 0, even if the new command differs, because Phase 2 only trusts the path. Phase 1's command check only applies while the Firestore record exists.
- A retry silently ignores a changed workpool, resources, labels, image or files. Consider echoing "ignored differences" in the response later.
- Any job without an explicit `resultPath` now writes `job_summary.json` under `<GCSPrefix>/<name>/`.

- Concurrent submissions with the same result path can race and both create a job (accepted; no locking).
- A user task that writes its own `task_summary.json` is overwritten by the worker's file (accepted).

## 7. Out of scope

Mid-task checkpoint/resume; whole-job short-circuit and creating only unfinished tasks (Phase 3); cross-workpool migration.

## 8. Open questions

None remaining. Resolved in the last review round and reflected above: task result paths nest under the job's `ResultPath` (3.1); the concurrent-submit race is accepted (6); the dashboard backend gets delete access on `GCSPrefix` (3.4, 6); the `task_summary.json` name collision is accepted (3.3, 6); per-task event volume is acceptable (3.4); the retry request blocks (3.4); killed jobs are revived by a resubmit (1, 3.4).
