package monitor

import (
	"context"
	"log"
	"time"
)

// runJobSummaryPoll updates JobSummary for every job that is not yet in a
// terminal state (deleted jobs are terminal, so they are never revisited). For
// each job it counts tasks by status, recomputes the job status, writes a
// history snapshot, and — on first transition to terminal — publishes a
// JobTerminatedEvent.
func (a *Monitor) runJobSummaryPoll(ctx context.Context) error {
	summaries, err := a.jobSummaries.ListNonTerminal(ctx)
	if err != nil {
		return err
	}

	for _, summary := range summaries {
		if err := a.updateJobSummary(ctx, summary); err != nil {
			log.Printf("job summary poll: job %s: %v", summary.JobID, err)
		} else {
			a.stats.Add(PollerJobSummary, "jobs_recomputed", 1)
		}
	}
	return nil
}

// reviveJobSummary handles a job_created event for a job whose JobSummary is
// already terminal -- which happens when a retry puts tasks of a finished job
// back to pending. runJobSummaryPoll only looks at non-terminal summaries, so
// without this a retried job would never be recomputed. The summary is
// recomputed from the current task counts (and its expiry extended); if the
// retry has not reset any tasks yet it simply stays terminal. A missing or
// non-terminal summary (the normal case for a newly created job) is left to
// the regular poll.
func (a *Monitor) reviveJobSummary(ctx context.Context, jobID string) error {
	summary, err := a.jobSummaries.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if summary == nil || !IsTerminalJobStatus(summary.State) {
		return nil
	}
	if summary.State == JobStatusDeleted {
		// Deleted is final. A job_created event for a deleted job (e.g. a
		// retry that raced the delete) must not bring it back.
		log.Printf("job summary: ignoring job_created for deleted job %s", jobID)
		return nil
	}
	summary.Expiry = time.Now().Add(7 * 24 * time.Hour)
	return a.updateJobSummary(ctx, summary)
}

func (a *Monitor) updateJobSummary(ctx context.Context, summary *JobSummary) error {
	// The state computed below comes from task counts and can never be
	// deleted, so recomputing a deleted job would silently undelete it.
	if summary.State == JobStatusDeleted {
		return nil
	}
	counts, err := a.tasks.CountByJob(ctx, summary.JobID)
	if err != nil {
		return err
	}

	newState := computeJobStatus(counts)
	newTasks := taskCountsFromMap(counts)
	now := time.Now()

	summary.State = newState
	summary.Tasks = newTasks
	summary.LastUpdated = now

	if err := a.jobSummaries.Save(ctx, summary); err != nil {
		return err
	}

	history := &JobSummaryHistory{
		JobID:      summary.JobID,
		WorkpoolID: summary.WorkpoolID,
		CreatedAt:  summary.CreatedAt,
		Timestamp:  now,
		Expiry:     summary.Expiry,
		State:      newState,
		Tasks:      newTasks,
		Labels:     summary.Labels,
	}
	if err := a.jobSummaries.SaveHistory(ctx, history); err != nil {
		return err
	}

	if IsTerminalJobStatus(newState) && a.jobTerminated != nil {
		if err := a.jobTerminated.PublishJobTerminated(ctx, summary.JobID, summary.WorkpoolID); err != nil {
			log.Printf("job summary poll: publish job_terminated for job %s: %v", summary.JobID, err)
		}
	}

	if IsTerminalJobStatus(newState) && a.jobResultWriter != nil {
		if err := a.jobResultWriter.WriteJobSummary(ctx, summary.JobID); err != nil {
			log.Printf("job summary poll: writing job summary for job %s: %v", summary.JobID, err)
		}
	}
	return nil
}

// computeJobStatus derives a JobStatus from a map of task status → count.
func computeJobStatus(counts map[string]int) JobStatus {
	pending := counts["pending"]
	active := counts["claimed"] + counts["running"] + counts["writing"]
	failed := counts["failed"]
	errored := counts["error"]

	if pending > 0 || active > 0 {
		if failed > 0 {
			return JobStatusInProgressWithFailure
		}
		if errored > 0 {
			return JobStatusInProgressWithError
		}
		if active > 0 {
			return JobStatusInProgress
		}
		return JobStatusPending
	}

	// All tasks are in terminal states.
	if counts["killed"] > 0 {
		return JobStatusKilled
	}
	if failed > 0 {
		return JobStatusFailed
	}
	if errored > 0 {
		return JobStatusError
	}
	return JobStatusSuccess
}

// taskCountsFromMap converts a status→count map to the []StateCount slice
// stored in JobSummary. Only statuses with non-zero counts are included.
func taskCountsFromMap(counts map[string]int) []StateCount {
	var result []StateCount
	for state, count := range counts {
		if count > 0 {
			result = append(result, StateCount{State: state, Count: count})
		}
	}
	return result
}
