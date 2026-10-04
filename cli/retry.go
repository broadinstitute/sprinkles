package sprinkles

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
)

// retryTaskStatuses are the terminal task statuses a retry puts back to pending.
var retryTaskStatuses = map[string]bool{StatusError: true, StatusFailed: true, StatusKilled: true}

// retryChunkSize is the number of tasks reset per Firestore transaction (the
// transaction limit is 500 writes).
const retryChunkSize = 200

// RetryableTasks returns the tasks of jobID that a retry would reset (status
// error, failed or killed).
func RetryableTasks(ctx context.Context, fs *firestore.Client, jobID string) ([]*Task, error) {
	iter := fs.Collection(taskCollection).Where("job_id", "==", jobID).Documents(ctx)
	defer iter.Stop()
	var retryable []*Task
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			return retryable, nil
		}
		if err != nil {
			return nil, err
		}
		var t Task
		if err := doc.DataTo(&t); err != nil {
			return nil, err
		}
		if retryTaskStatuses[t.Status] {
			retryable = append(retryable, &t)
		}
	}
}

// ExtendTaskExpiry sets the expiry of every task of jobID to newExpiry, so a
// retried job's tasks aren't garbage-collected out from under it.
func ExtendTaskExpiry(ctx context.Context, fs *firestore.Client, jobID string, newExpiry time.Time) error {
	iter := fs.Collection(taskCollection).Where("job_id", "==", jobID).Select().Documents(ctx)
	defer iter.Stop()

	bw := fs.BulkWriter(ctx)
	var jobs []*firestore.BulkWriterJob
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			bw.End()
			return err
		}
		j, err := bw.Update(doc.Ref, []firestore.Update{{Path: "expiry", Value: newExpiry}})
		if err != nil {
			bw.End()
			return err
		}
		jobs = append(jobs, j)
	}
	bw.End()
	for _, j := range jobs {
		if _, err := j.Results(); err != nil {
			return err
		}
	}
	return nil
}

// ResetTasksToPending puts the given tasks back to pending, retryChunkSize per
// transaction. Within each transaction a task is skipped if it is no longer in
// a retryable status. A task_state_update event is published for each task
// reset. Idempotent: if it fails part way, calling it again with a fresh
// RetryableTasks list finishes the job. Returns the number of tasks reset.
func ResetTasksToPending(ctx context.Context, fs *firestore.Client, publisher *EventPublisher, tasks []*Task) (int, error) {
	type resetTask struct{ taskID, jobID, oldStatus string }

	reset := 0
	for start := 0; start < len(tasks); start += retryChunkSize {
		chunk := tasks[start:min(start+retryChunkSize, len(tasks))]

		var done []resetTask
		err := fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
			done = done[:0] // the transaction function may be re-run
			refs := make([]*firestore.DocumentRef, len(chunk))
			for i, t := range chunk {
				refs[i] = fs.Collection(taskCollection).Doc(t.TaskID)
			}
			snaps, err := tx.GetAll(refs)
			if err != nil {
				return err
			}
			now := time.Now()
			for i, snap := range snaps {
				if !snap.Exists() {
					continue
				}
				var cur Task
				if err := snap.DataTo(&cur); err != nil {
					return err
				}
				if !retryTaskStatuses[cur.Status] {
					continue
				}
				if err := tx.Update(refs[i], []firestore.Update{
					{Path: "status", Value: StatusPending},
					{Path: "failure_reason", Value: ""},
					{Path: "exit_code", Value: 0},
					{Path: "owning_worker_id", Value: ""},
					{Path: "found_prior_result", Value: false},
					{Path: "last_updated", Value: now},
					{Path: "expiry", Value: now.Add(7 * 24 * time.Hour)},
				}); err != nil {
					return err
				}
				done = append(done, resetTask{cur.TaskID, cur.JobID, cur.Status})
			}
			return nil
		})
		if err != nil {
			return reset, fmt.Errorf("resetting tasks: %w", err)
		}
		for _, d := range done {
			if err := publisher.PublishTaskStateUpdate(ctx, d.taskID, d.jobID, d.oldStatus, StatusPending); err != nil {
				return reset, fmt.Errorf("publishing task_state_update for %s: %w", d.taskID, err)
			}
			reset++
		}
	}
	return reset, nil
}
