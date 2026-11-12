package functest_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/broadinstitute/sprinkles"
	"github.com/broadinstitute/sprinkles/monitor"
)

func (e *retryEnv) deleteJob(t *testing.T, jobID string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, e.server.URL+"/api/v1/job/"+jobID, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	return e.do(t, req)
}

func (e *retryEnv) summaryState(t *testing.T, ctx context.Context, jobID string) string {
	t.Helper()
	snap, err := e.fs.Collection("JobSummary").Doc(jobID).Get(ctx)
	if err != nil {
		t.Fatalf("getting job summary %s: %v", jobID, err)
	}
	state, _ := snap.DataAt("state")
	s, _ := state.(string)
	return s
}

// A job that finished on its own can be deleted; deleting is idempotent and
// touches nothing but the summary's state.
func deleteJobTerminalJobBecomesDeleted(t *testing.T, ctx context.Context, env *retryEnv) {

	for _, state := range []monitor.JobStatus{monitor.JobStatusSuccess, monitor.JobStatusError, monitor.JobStatusFailed, monitor.JobStatusKilled} {
		jobID, taskIDs := env.seedJob(t, ctx, env.gcsBase+"/del-"+randomSuffix(), []string{sprinkles.StatusSuccess, sprinkles.StatusFailed}, state)

		status, out := env.deleteJob(t, jobID)
		if status != http.StatusOK || out["job_id"] != jobID || out["state"] != "deleted" {
			t.Fatalf("delete of %s job: want 200 deleted, got %d: %v", state, status, out)
		}
		if got := env.summaryState(t, ctx, jobID); got != "deleted" {
			t.Errorf("summary state after delete of %s job: want deleted, got %s", state, got)
		}
		// Nothing else changes: the tasks and the job itself remain.
		if got := env.taskStatus(t, ctx, taskIDs[1]).Status; got != sprinkles.StatusFailed {
			t.Errorf("task status changed by delete: %s", got)
		}
		if _, err := env.fs.Collection(sprinkles.JobCollection).Doc(jobID).Get(ctx); err != nil {
			t.Errorf("job document should remain: %v", err)
		}

		// Deleting again is a no-op, not an error.
		if status, out := env.deleteJob(t, jobID); status != http.StatusOK || out["state"] != "deleted" {
			t.Errorf("second delete: want 200 deleted, got %d: %v", status, out)
		}
	}
}

// A job that hasn't finished must be cancelled and finish first.
func deleteJobNonTerminalJobIsRefused(t *testing.T, ctx context.Context, env *retryEnv) {

	for _, state := range []monitor.JobStatus{
		monitor.JobStatusPending, monitor.JobStatusInProgress,
		monitor.JobStatusInProgressWithError, monitor.JobStatusInProgressWithFailure,
	} {
		jobID, _ := env.seedJob(t, ctx, env.gcsBase+"/del-"+randomSuffix(), []string{sprinkles.StatusRunning}, state)

		status, out := env.deleteJob(t, jobID)
		if status != http.StatusConflict {
			t.Fatalf("delete of %s job: want 409, got %d: %v", state, status, out)
		}
		if got := env.summaryState(t, ctx, jobID); got != string(state) {
			t.Errorf("a refused delete must not change state: %s -> %s", state, got)
		}
	}
}

// The summary only catches up with a retry once the monitor revives it, so a
// summary can still say "terminal" while the job's tasks are pending again.
// Such a job must not be deletable, or it would be deleted *and* running.
func deleteJobRefusedWhileTasksAreStillActive(t *testing.T, ctx context.Context, env *retryEnv) {

	for _, active := range []string{sprinkles.StatusPending, sprinkles.StatusClaimed, sprinkles.StatusRunning, sprinkles.StatusWriting} {
		jobID, _ := env.seedJob(t, ctx, env.gcsBase+"/del-"+randomSuffix(), []string{sprinkles.StatusSuccess, active}, monitor.JobStatusFailed)

		status, out := env.deleteJob(t, jobID)
		if status != http.StatusConflict {
			t.Fatalf("delete with a %s task: want 409, got %d: %v", active, status, out)
		}
		if got := env.summaryState(t, ctx, jobID); got != string(monitor.JobStatusFailed) {
			t.Errorf("state changed by a refused delete: %s", got)
		}
	}
}

func deleteJobNotFound(t *testing.T, ctx context.Context, env *retryEnv) {
	if status, _ := env.deleteJob(t, "no-such-job"); status != http.StatusNotFound {
		t.Errorf("want 404, got %d", status)
	}
}

// There is no way back: a deleted job can't be retried.
func deleteJobDeletedJobCannotBeRetried(t *testing.T, ctx context.Context, env *retryEnv) {

	jobID, taskIDs := env.seedJob(t, ctx, env.gcsBase+"/del-"+randomSuffix(), []string{sprinkles.StatusFailed}, monitor.JobStatusFailed)
	if status, out := env.deleteJob(t, jobID); status != http.StatusOK {
		t.Fatalf("delete: want 200, got %d: %v", status, out)
	}

	status, out := env.post(t, "/api/v1/job/"+jobID+"/retry", nil)
	if status != http.StatusConflict {
		t.Fatalf("retry of deleted job: want 409, got %d: %v", status, out)
	}
	if got := env.taskStatus(t, ctx, taskIDs[0]).Status; got != sprinkles.StatusFailed {
		t.Errorf("a refused retry must not reset tasks, got %s", got)
	}
	if got := env.summaryState(t, ctx, jobID); got != "deleted" {
		t.Errorf("state after refused retry: want deleted, got %s", got)
	}
}

// Resubmitting a deleted job's name starts a fresh job instead of retrying
// the deleted one (which is what happens, unchanged, for a job that isn't
// deleted).
func deleteJobResubmissionAfterDeleteCreatesANewJob(t *testing.T, ctx context.Context, env *retryEnv) {

	name := "del-resubmit-" + randomSuffix()
	cmds := [][]string{{"echo", "a"}}
	status, out := env.post(t, "/api/v1/job", submitBody(name, cmds))
	if status != http.StatusOK {
		t.Fatalf("first submit: want 200, got %d: %v", status, out)
	}
	oldID := out["id"].(string)

	// Finish the job: mark its task failed and its summary failed.
	tasks, err := sprinkles.NewFirestoreTaskQueue(env.fs, nil).ListTasksForJob(ctx, oldID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("listing tasks: %v (%d)", err, len(tasks))
	}
	if _, err := env.fs.Collection(sprinkles.TaskCollection).Doc(tasks[0].TaskID).Update(ctx, []firestore.Update{{Path: "status", Value: sprinkles.StatusFailed}}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.fs.Collection("JobSummary").Doc(oldID).Update(ctx, []firestore.Update{{Path: "state", Value: string(monitor.JobStatusFailed)}}); err != nil {
		t.Fatal(err)
	}

	// Not deleted: the same submission retries the existing job.
	status, out = env.post(t, "/api/v1/job", submitBody(name, cmds))
	if status != http.StatusOK || out["id"] != oldID || out["retried"] != true {
		t.Fatalf("resubmit of a live job: want retry of %s, got %d: %v", oldID, status, out)
	}
	// Put it back to a finished state and delete it.
	if _, err := env.fs.Collection(sprinkles.TaskCollection).Doc(tasks[0].TaskID).Update(ctx, []firestore.Update{{Path: "status", Value: sprinkles.StatusFailed}}); err != nil {
		t.Fatal(err)
	}
	if status, out := env.deleteJob(t, oldID); status != http.StatusOK {
		t.Fatalf("delete: want 200, got %d: %v", status, out)
	}

	status, out = env.post(t, "/api/v1/job", submitBody(name, cmds))
	if status != http.StatusOK {
		t.Fatalf("resubmit after delete: want 200, got %d: %v", status, out)
	}
	if out["id"] == oldID || out["retried"] == true {
		t.Errorf("resubmit after delete must create a new job, got %v", out)
	}
	if got := env.summaryState(t, ctx, oldID); got != "deleted" {
		t.Errorf("the old job must stay deleted, got %s", got)
	}
}

// TestDeleteJob covers DELETE /api/v1/job/{job_id}. The cases share one set of
// emulators (starting them is slow) and each seeds its own jobs.
func TestDeleteJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env := newRetryEnv(t, ctx)

	for name, fn := range map[string]func(*testing.T, context.Context, *retryEnv){
		"terminal job becomes deleted":                deleteJobTerminalJobBecomesDeleted,
		"non-terminal job is refused":                 deleteJobNonTerminalJobIsRefused,
		"refused while tasks are still active":        deleteJobRefusedWhileTasksAreStillActive,
		"not found":                                   deleteJobNotFound,
		"deleted job cannot be retried":               deleteJobDeletedJobCannotBeRetried,
		"resubmission after delete creates a new job": deleteJobResubmissionAfterDeleteCreatesANewJob,
	} {
		t.Run(name, func(t *testing.T) { fn(t, ctx, env) })
	}
}
