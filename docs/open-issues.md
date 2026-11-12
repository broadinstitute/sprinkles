Instead of having a seperate "hidden" property, add "deleted" status. Only jobs which are terminated can transition to deleted.

When a poller writes out a problem no where to see it from the UI (partially fixed)

No way to terminate a workpool completely (Need a "kill" workpool switch)

  Notes:
  - Existing pieces: `sprinkles kill <job-id>` / `handleCancelJob` (kill.go `KillJobWithClients`) kills one job;
    `TerminateJob` in monitor/batch_api.go cancels a Batch job but is only used internally by the monitor;
    `POST /workpool/{id}/reset` only clears a halted state. A `halted` pool stops provisioning but does not
    cancel existing batches or kill any jobs.
  - Needed:
    1. Stop provisioning first so the monitor doesn't create replacement batches: a new sticky workpool state
       (e.g. `terminated`/`disabled`), similar to `halted` (provision.go already skips halted pools).
    2. Cancel every non-terminal Batch job for the pool (list batches, call `TerminateJob`).
    3. Kill every non-terminal job assigned to the pool (query jobs by workpool, call `KillJobWithClients`).
       Open question: make this step optional, so a misconfigured pool can be torn down while its jobs stay
       pending and can be re-pointed at a fixed pool.
    4. Expose as `POST /api/v1/workpool/{id}/terminate`, a CLI command, and a dashboard button with confirmation.
  - Manual workaround today: `sprinkles kill` per job, and cancel Batch jobs via `gcloud batch jobs delete` or the console.

Job page is still awkward. Something like "show active jobs and jobs which recently completed. Recent could be the last 20 jobs, or last hour" would be a better default

The re-try job thing is a nuisance in development. Maybe only enable for prod runs? 

finding job which is re-tryable is not checking to see if job is visible.

Need a "retry" button the UI for re-running failed/complete with error tasks. (And corresponding API endpoint) (may have been done? need to check)

Add flag to control streaming of logs. (Check on firestore costs)

Would be nice to have a "delete" job rather than hide. (Should only work on jobs in a terminal state)

## Low priority

Phase 3 of retry: (Optimization) We can short circuit a few steps. Primarily we can test to see if the job was run in its entirity. If so, just create a syntentic job was no tasks and report success. Also could put a check of GCS files into re-submission logic and only create new tasks for tasks which did not complete successfully.

Add support for custom metrics? (probably best to skip... but could be handy. Need motivating usecase to prototype)


Would enable jobs to be able to periodically checkpoint. (Checkpointing directly to GCS would probably be better...) Regardless, this mechanism would be helpful for detecting expired tasks which actually already have been run.
