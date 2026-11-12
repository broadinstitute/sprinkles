package functest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/broadinstitute/sprinkles"
	"github.com/broadinstitute/sprinkles/dev"
	"github.com/broadinstitute/sprinkles/monitor"
	"github.com/google/uuid"
)

type retryEnv struct {
	fs      *firestore.Client
	server  *httptest.Server
	apiKey  string
	gcsBase string
}

func newRetryEnv(t *testing.T, ctx context.Context) *retryEnv {
	t.Helper()
	startFirestoreEmulator(t)
	startPubSubEmulator(t)
	startGCSEmulator(t)

	fsClient := newFirestoreClient(t, ctx)
	psClient := newPubSubClient(t, ctx)
	ensureTopics(t, ctx, psClient)

	gcsBase := fmt.Sprintf("gs://%s/results", gcsBucket)
	if _, err := fsClient.Collection("SprinklesConfig").Doc("default").Set(ctx, dev.SprinklesConfig{GCSPrefix: gcsBase}); err != nil {
		t.Fatalf("writing SprinklesConfig/default: %v", err)
	}
	apiKey := writeAPIKey(t, ctx, fsClient, "test-user")
	handler, err := dev.NewDashboardHandler(ctx, testProject, fsClient, psClient, "", nil, nil)
	if err != nil {
		t.Fatalf("NewDashboardHandler: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &retryEnv{fs: fsClient, server: server, apiKey: apiKey, gcsBase: gcsBase}
}

func (e *retryEnv) post(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encoding body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, &buf)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	return e.do(t, req)
}

// do sends req and decodes a JSON object response.
func (e *retryEnv) do(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// seedJob writes a job at resultPath with one task per status (command
// ["cmd", index]), plus a terminal JobSummary. Returns the job ID and task IDs by index.
func (e *retryEnv) seedJob(t *testing.T, ctx context.Context, resultPath string, statuses []string, summaryState monitor.JobStatus) (string, []string) {
	t.Helper()
	jobID := uuid.New().String()
	now := time.Now()
	job := sprinkles.Job{
		JobID: jobID, Name: "retry-job", WorkpoolID: "retry-pool-" + randomSuffix(), CreatedAt: now,
		Expiry: now.Add(time.Hour), TaskCount: len(statuses), ResultPath: resultPath,
		Resources: []sprinkles.ResourceEntry{{Name: "slots", Value: 1}},
	}
	if _, err := e.fs.Collection(sprinkles.JobCollection).Doc(jobID).Set(ctx, job); err != nil {
		t.Fatalf("writing job: %v", err)
	}
	taskIDs := make([]string, len(statuses))
	for i, st := range statuses {
		taskIDs[i] = uuid.New().String()
		task := sprinkles.Task{
			JobID: jobID, TaskID: taskIDs[i], TaskIndex: i, WorkpoolID: job.WorkpoolID, Status: st,
			Command: []string{"cmd", fmt.Sprint(i)}, DockerImage: "img",
			ResultPath: fmt.Sprintf("%s/%d", resultPath, i), Expiry: now.Add(time.Hour),
			FailureReason: "boom", ExitCode: 3,
		}
		if _, err := e.fs.Collection(sprinkles.TaskCollection).Doc(taskIDs[i]).Set(ctx, task); err != nil {
			t.Fatalf("writing task: %v", err)
		}
	}
	summary := monitor.JobSummary{JobID: jobID, WorkpoolID: job.WorkpoolID, Name: job.Name, CreatedAt: now, Expiry: now.Add(time.Hour), State: summaryState}
	if _, err := e.fs.Collection("JobSummary").Doc(jobID).Set(ctx, summary); err != nil {
		t.Fatalf("writing job summary: %v", err)
	}
	return jobID, taskIDs
}

func (e *retryEnv) taskStatus(t *testing.T, ctx context.Context, taskID string) sprinkles.Task {
	t.Helper()
	snap, err := e.fs.Collection(sprinkles.TaskCollection).Doc(taskID).Get(ctx)
	if err != nil {
		t.Fatalf("getting task %s: %v", taskID, err)
	}
	var task sprinkles.Task
	if err := snap.DataTo(&task); err != nil {
		t.Fatalf("decoding task: %v", err)
	}
	return task
}

// TestRetryJob_ResetsOnlyFailedTasks checks that the retry endpoint resets
// error/failed/killed tasks, leaves success and in-flight tasks alone,
// deletes job_summary.json, leaves the JobSummary to the monitor and extends expiry.
func TestRetryJob_ResetsOnlyFailedTasks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env := newRetryEnv(t, ctx)
	bucket := newGCSClient(t, ctx).Bucket(gcsBucket)

	resultPath := env.gcsBase + "/retry-" + randomSuffix()
	statuses := []string{
		sprinkles.StatusSuccess, sprinkles.StatusError, sprinkles.StatusFailed,
		sprinkles.StatusKilled, sprinkles.StatusRunning, sprinkles.StatusPending,
	}
	jobID, taskIDs := env.seedJob(t, ctx, resultPath, statuses, monitor.JobStatusFailed)
	summaryObject := "results/" + resultPath[len(env.gcsBase)+1:] + "/job_summary.json"
	writeGCSObject(t, ctx, bucket, summaryObject, "{}")

	status, out := env.post(t, "/api/v1/job/"+jobID+"/retry", nil)
	if status != http.StatusOK {
		t.Fatalf("retry: want 200, got %d: %v", status, out)
	}
	if out["id"] != jobID || out["retried"] != true || out["resetTasks"] != float64(3) {
		t.Errorf("unexpected response: %v", out)
	}

	want := []string{
		sprinkles.StatusSuccess, sprinkles.StatusPending, sprinkles.StatusPending,
		sprinkles.StatusPending, sprinkles.StatusRunning, sprinkles.StatusPending,
	}
	for i, id := range taskIDs {
		task := env.taskStatus(t, ctx, id)
		if task.Status != want[i] {
			t.Errorf("task %d: want %s, got %s", i, want[i], task.Status)
		}
		if i >= 1 && i <= 3 && (task.FailureReason != "" || task.ExitCode != 0) {
			t.Errorf("task %d: failure_reason/exit_code not cleared: %q %d", i, task.FailureReason, task.ExitCode)
		}
		if !task.Expiry.After(time.Now().Add(24 * time.Hour)) {
			t.Errorf("task %d: expiry not extended: %v", i, task.Expiry)
		}
	}

	snap, err := env.fs.Collection("JobSummary").Doc(jobID).Get(ctx)
	if err != nil {
		t.Fatalf("getting job summary: %v", err)
	}
	// The backend never writes JobSummary; reviving it is the monitor's job
	// (covered by monitor/functest TestReviveJobSummary).
	if state, _ := snap.DataAt("state"); state != string(monitor.JobStatusFailed) {
		t.Errorf("job summary state: want unchanged (failed), got %v", state)
	}
	if _, err := bucket.Object(summaryObject).Attrs(ctx); err == nil {
		t.Errorf("job_summary.json should have been deleted")
	}

	// Retrying again is a no-op.
	status, out = env.post(t, "/api/v1/job/"+jobID+"/retry", nil)
	if status != http.StatusOK || out["resetTasks"] != nil && out["resetTasks"] != float64(0) {
		t.Errorf("second retry: want 200 with no tasks reset, got %d: %v", status, out)
	}
}

func TestRetryJob_NotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env := newRetryEnv(t, ctx)
	if status, _ := env.post(t, "/api/v1/job/no-such-job/retry", nil); status != http.StatusNotFound {
		t.Errorf("want 404, got %d", status)
	}
}

func submitBody(name string, commands [][]string) map[string]any {
	tasks := make([]map[string]any, len(commands))
	for i, c := range commands {
		tasks[i] = map[string]any{"image": "img", "command": c}
	}
	return map[string]any{
		"name":     name,
		"tasks":    tasks,
		"workpool": map[string]any{"machineType": "n2-standard-2"},
	}
}

// TestSubmitJob_ResubmissionRetriesExistingJob covers the submit endpoint:
// the same name finds the existing job (result path defaults to
// <GCSPrefix>/<name>) and retries it; a changed command or task count is a 409.
func TestSubmitJob_ResubmissionRetriesExistingJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env := newRetryEnv(t, ctx)

	name := "resubmit-" + randomSuffix()
	cmds := [][]string{{"echo", "a"}, {"echo", "b"}}

	status, out := env.post(t, "/api/v1/job", submitBody(name, cmds))
	if status != http.StatusOK {
		t.Fatalf("first submit: want 200, got %d: %v", status, out)
	}
	jobID := out["id"].(string)
	if out["retried"] == true {
		t.Fatalf("first submit should not be a retry: %v", out)
	}

	snap, err := env.fs.Collection(sprinkles.JobCollection).Doc(jobID).Get(ctx)
	if err != nil {
		t.Fatalf("getting job: %v", err)
	}
	if rp, _ := snap.DataAt("result_path"); rp != env.gcsBase+"/"+name {
		t.Errorf("job result_path: want %s/%s, got %v", env.gcsBase, name, rp)
	}
	tasks, err := sprinkles.NewFirestoreTaskQueue(env.fs, nil).ListTasksForJob(ctx, jobID)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("listing tasks: %v (%d)", err, len(tasks))
	}
	for _, task := range tasks {
		if want := fmt.Sprintf("%s/%s/%d", env.gcsBase, name, task.TaskIndex); task.ResultPath != want {
			t.Errorf("task %d result path: want %s, got %s", task.TaskIndex, want, task.ResultPath)
		}
		if _, err := env.fs.Collection(sprinkles.TaskCollection).Doc(task.TaskID).Update(ctx, []firestore.Update{{Path: "status", Value: sprinkles.StatusError}}); err != nil {
			t.Fatalf("marking task error: %v", err)
		}
	}

	// Same submission: same job, tasks reset.
	status, out = env.post(t, "/api/v1/job", submitBody(name, cmds))
	if status != http.StatusOK || out["id"] != jobID || out["retried"] != true || out["resetTasks"] != float64(2) {
		t.Fatalf("resubmit: want retry of %s with 2 tasks reset, got %d: %v", jobID, status, out)
	}

	// Changed command and changed task count are rejected.
	if status, out = env.post(t, "/api/v1/job", submitBody(name, [][]string{{"echo", "a"}, {"echo", "CHANGED"}})); status != http.StatusConflict {
		t.Errorf("changed command: want 409, got %d: %v", status, out)
	}
	if status, out = env.post(t, "/api/v1/job", submitBody(name, cmds[:1])); status != http.StatusConflict {
		t.Errorf("changed task count: want 409, got %d: %v", status, out)
	}
}
