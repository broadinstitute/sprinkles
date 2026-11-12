package monitor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsTerminalJobStatus(t *testing.T) {
	terminal := []JobStatus{JobStatusSuccess, JobStatusError, JobStatusFailed, JobStatusKilled, JobStatusDeleted}
	nonTerminal := []JobStatus{JobStatusPending, JobStatusInProgress, JobStatusInProgressWithError, JobStatusInProgressWithFailure}
	for _, s := range terminal {
		assert.True(t, IsTerminalJobStatus(s), "%s should be terminal", s)
	}
	for _, s := range nonTerminal {
		assert.False(t, IsTerminalJobStatus(s), "%s should not be terminal", s)
	}
}

// Only a job that finished by itself can be deleted; deleted can't be deleted
// again, and anything still running must finish first.
func TestCanDeleteJob(t *testing.T) {
	for _, s := range []JobStatus{JobStatusSuccess, JobStatusError, JobStatusFailed, JobStatusKilled} {
		assert.True(t, CanDeleteJob(s), "%s may be deleted", s)
	}
	for _, s := range []JobStatus{JobStatusPending, JobStatusInProgress, JobStatusInProgressWithError, JobStatusInProgressWithFailure, JobStatusDeleted, ""} {
		assert.False(t, CanDeleteJob(s), "%q may not be deleted", s)
	}
}

// computeJobStatus derives state from task counts; deleted is only ever set
// explicitly, so it must never be a computed result.
func TestComputeJobStatus_NeverReturnsDeleted(t *testing.T) {
	for _, counts := range []map[string]int{
		{}, {"success": 3}, {"error": 1}, {"failed": 1}, {"killed": 1},
		{"pending": 1}, {"running": 1, "failed": 1}, {"pending": 2, "error": 1},
	} {
		assert.NotEqual(t, JobStatusDeleted, computeJobStatus(counts), "counts %v", counts)
	}
}

// recordingJobSummaries is an in-memory JobSummaryStore that records saves.
type recordingJobSummaries struct {
	byID  map[string]*JobSummary
	saves int
}

func (r *recordingJobSummaries) Create(_ context.Context, s *JobSummary) error {
	r.byID[s.JobID] = s
	return nil
}
func (r *recordingJobSummaries) Get(_ context.Context, id string) (*JobSummary, error) {
	return r.byID[id], nil
}
func (r *recordingJobSummaries) ListNonTerminal(context.Context) ([]*JobSummary, error) {
	var out []*JobSummary
	for _, s := range r.byID {
		if !IsTerminalJobStatus(s.State) {
			out = append(out, s)
		}
	}
	return out, nil
}
func (r *recordingJobSummaries) Save(_ context.Context, s *JobSummary) error {
	r.saves++
	r.byID[s.JobID] = s
	return nil
}
func (r *recordingJobSummaries) SaveHistory(context.Context, *JobSummaryHistory) error { return nil }

// A job_created event for a deleted job (e.g. a retry racing the delete) must
// not revive it, even though it has pending tasks.
func TestReviveJobSummary_DeletedJobStaysDeleted(t *testing.T) {
	w := newWorld()
	store := &recordingJobSummaries{byID: map[string]*JobSummary{
		"job-1": {JobID: "job-1", WorkpoolID: "pool-1", State: JobStatusDeleted},
	}}
	w.A.SetJobSummaryStore(store)
	w.Tasks.Add(&Task{TaskID: "t1", JobID: "job-1", WorkpoolID: "pool-1", Status: TaskStatusPending})

	require.NoError(t, w.A.reviveJobSummary(context.Background(), "job-1"))

	assert.Equal(t, JobStatusDeleted, store.byID["job-1"].State)
	assert.Zero(t, store.saves, "a deleted summary must not be rewritten")
}

// Contrast: the same situation with a merely terminal job does revive it.
func TestReviveJobSummary_TerminalJobWithPendingTasksIsRevived(t *testing.T) {
	w := newWorld()
	store := &recordingJobSummaries{byID: map[string]*JobSummary{
		"job-1": {JobID: "job-1", WorkpoolID: "pool-1", State: JobStatusError},
	}}
	w.A.SetJobSummaryStore(store)
	w.Tasks.Add(&Task{TaskID: "t1", JobID: "job-1", WorkpoolID: "pool-1", Status: TaskStatusPending})

	require.NoError(t, w.A.reviveJobSummary(context.Background(), "job-1"))

	assert.Equal(t, JobStatusPending, store.byID["job-1"].State)
}

// The regular poll never looks at deleted jobs (they aren't non-terminal),
// and updating one directly is a no-op.
func TestJobSummaryPoll_LeavesDeletedJobsAlone(t *testing.T) {
	w := newWorld()
	store := &recordingJobSummaries{byID: map[string]*JobSummary{
		"deleted": {JobID: "deleted", WorkpoolID: "pool-1", State: JobStatusDeleted},
	}}
	w.A.SetJobSummaryStore(store)
	w.Tasks.Add(&Task{TaskID: "t1", JobID: "deleted", WorkpoolID: "pool-1", Status: TaskStatusPending})

	require.NoError(t, w.A.runJobSummaryPoll(context.Background()))
	require.NoError(t, w.A.updateJobSummary(context.Background(), store.byID["deleted"]))

	assert.Equal(t, JobStatusDeleted, store.byID["deleted"].State)
	assert.Zero(t, store.saves)
}
