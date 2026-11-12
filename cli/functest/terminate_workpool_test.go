package functest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/broadinstitute/sprinkles"
	"github.com/broadinstitute/sprinkles/monitor"
	"github.com/google/uuid"
)

// TestTerminateWorkpool checks that terminating a workpool halts it, kills the
// jobs that still have live tasks in it (and only those), and cancels and
// marks terminated its live batches (and only those).
func TestTerminateWorkpool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Stand-in Batch API: records which jobs were cancelled.
	var mu sync.Mutex
	var cancelled []string
	batchAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel") {
			mu.Lock()
			cancelled = append(cancelled, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/cancel"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer batchAPI.Close()
	t.Setenv("SPARKLES_BATCH_API_EMULATOR", batchAPI.URL)

	env := newRetryEnv(t, ctx)
	pool := "term-pool-" + randomSuffix()
	if _, err := env.fs.Collection("WorkPools").Doc(pool).Set(ctx, map[string]any{"workpool_id": pool}); err != nil {
		t.Fatalf("writing workpool: %v", err)
	}

	now := time.Now()
	seedTask := func(jobID, workpoolID, status string) string {
		id := uuid.New().String()
		task := sprinkles.Task{
			JobID: jobID, TaskID: id, WorkpoolID: workpoolID, Status: status,
			Command: []string{"cmd"}, DockerImage: "img", Expiry: now.Add(time.Hour),
		}
		if _, err := env.fs.Collection(sprinkles.TaskCollection).Doc(id).Set(ctx, task); err != nil {
			t.Fatalf("writing task: %v", err)
		}
		return id
	}
	liveJob, doneJob, otherPoolJob := uuid.New().String(), uuid.New().String(), uuid.New().String()
	livePending := seedTask(liveJob, pool, sprinkles.StatusPending)
	doneTask := seedTask(doneJob, pool, sprinkles.StatusSuccess)
	otherPending := seedTask(otherPoolJob, "other-pool-"+randomSuffix(), sprinkles.StatusPending)

	batches := monitor.NewFirestoreBatchRequestStore(env.fs)
	mkBatch := func(workpoolID string, status monitor.BatchStatus) *monitor.BatchAPIRequest {
		b := &monitor.BatchAPIRequest{
			BatchID: "b-" + uuid.New().String()[:8], JobID: "projects/p/locations/l/jobs/" + uuid.New().String()[:8],
			WorkpoolID: workpoolID, SubmittedAt: now, Expiry: now.Add(time.Hour), Status: status,
		}
		if err := batches.Create(ctx, b); err != nil {
			t.Fatalf("creating batch: %v", err)
		}
		return b
	}
	started := mkBatch(pool, monitor.BatchStatusStarted)
	completed := mkBatch(pool, monitor.BatchStatusCompleted)
	otherBatch := mkBatch("other-pool-"+randomSuffix(), monitor.BatchStatusStarted)

	status, body := env.post(t, "/api/v1/workpool/"+pool+"/terminate", nil)
	if status != http.StatusOK {
		t.Fatalf("terminate: status %d, body %v", status, body)
	}
	if body["killed_jobs"] != float64(1) || body["terminated_batches"] != float64(1) || body["errors"] != nil {
		t.Errorf("unexpected response: %v", body)
	}

	if got := env.taskStatus(t, ctx, livePending).Status; got != sprinkles.StatusKilled {
		t.Errorf("live pending task: status %q, want killed", got)
	}
	if got := env.taskStatus(t, ctx, doneTask).Status; got != sprinkles.StatusSuccess {
		t.Errorf("finished task: status %q, want success", got)
	}
	if got := env.taskStatus(t, ctx, otherPending).Status; got != sprinkles.StatusPending {
		t.Errorf("other pool's task: status %q, want pending", got)
	}

	for _, c := range []struct {
		b    *monitor.BatchAPIRequest
		want monitor.BatchStatus
	}{{started, monitor.BatchStatusTerminated}, {completed, monitor.BatchStatusCompleted}, {otherBatch, monitor.BatchStatusStarted}} {
		got, err := batches.Get(ctx, c.b.BatchID)
		if err != nil {
			t.Fatalf("getting batch %s: %v", c.b.BatchID, err)
		}
		if got.Status != c.want {
			t.Errorf("batch %s: status %q, want %q", c.b.BatchID, got.Status, c.want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cancelled) != 1 || !strings.HasSuffix(started.JobID, cancelled[0]) {
		t.Errorf("cancelled batch jobs = %v, want only %s", cancelled, started.JobID)
	}

	ws, err := monitor.NewFirestoreWorkPoolStore(env.fs).Get(ctx, pool)
	if err != nil {
		t.Fatalf("getting workpool: %v", err)
	}
	if ws.State.State != monitor.WorkPoolStatusHalted {
		t.Errorf("workpool state %q, want halted", ws.State.State)
	}

	status, _ = env.post(t, "/api/v1/workpool/no-such-pool-"+randomSuffix()+"/terminate", nil)
	if status != http.StatusNotFound {
		t.Errorf("unknown workpool: status %d, want 404", status)
	}
}
