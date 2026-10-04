When a poller writes out a problem no where to see it from the UI (partially fixed)

No way to terminate a workpool completely (Need a "kill" workpool switch)

Job page is still awkward. Something like "show active jobs and jobs which recently completed. Recent could be the last 20 jobs, or last hour" would be a better default

Need a "retry" button the UI for re-running failed/complete with error tasks. (And corresponding API endpoint) (may have been done? need to check)

Add flag to control streaming of logs. (Check on firestore costs)

## Low priority

Phase 3 of retry: (Optimization) We can short circuit a few steps. Primarily we can test to see if the job was run in its entirity. If so, just create a syntentic job was no tasks and report success. Also could put a check of GCS files into re-submission logic and only create new tasks for tasks which did not complete successfully.

Add support for custom metrics? (probably best to skip... but could be handy. Need motivating usecase to prototype)


Would enable jobs to be able to periodically checkpoint. (Checkpointing directly to GCS would probably be better...) Regardless, this mechanism would be helpful for detecting expired tasks which actually already have been run.
