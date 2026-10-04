package sprinkles

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

const retryTestResultPath = "gs://test-bucket/results/job/0"

func makeTaskWithResultPath(taskID, jobID string) *Task {
	t := makeTask(taskID, jobID, "pool1")
	t.ResultPath = retryTestResultPath
	t.LogPath = retryTestResultPath + "/stdout.txt"
	return t
}

func readTaskSummary(t *testing.T, tc *mockTransferClient) (TaskSummary, bool) {
	t.Helper()
	data, ok := tc.files[retryTestResultPath+"/"+TaskSummaryFile]
	if !ok {
		return TaskSummary{}, false
	}
	var s TaskSummary
	require.NoError(t, json.Unmarshal(data, &s))
	return s, true
}

func runSingleTask(t *testing.T, tc *mockTransferClient, docker *recordingDockerCommand) *mockTaskQueue {
	t.Helper()
	q := &mockTaskQueue{
		tasks: []*Task{makeTaskWithResultPath("t1", "j1")},
		jobs:  map[string]*Job{"j1": makeJob("j1", 1)},
	}
	require.NoError(t, workerMainLoop(context.Background(), makeConfig(t, q, tc, docker)))
	return q
}

func TestTaskSummary_WrittenOnSuccess(t *testing.T) {
	tc := newMockTransferClient()
	q := runSingleTask(t, tc, &recordingDockerCommand{})

	require.Equal(t, StatusSuccess, q.taskByID("t1").Status)
	require.False(t, q.taskByID("t1").FoundPriorResult)
	s, ok := readTaskSummary(t, tc)
	require.True(t, ok, "task_summary.json should be uploaded")
	require.Equal(t, 0, s.ExitCode)
}

func TestTaskSummary_WrittenOnNonZeroExit(t *testing.T) {
	tc := newMockTransferClient()
	runSingleTask(t, tc, &recordingDockerCommand{err: &exec.ExitError{}})

	s, ok := readTaskSummary(t, tc)
	require.True(t, ok, "task_summary.json should be uploaded for a non-zero exit")
	require.NotEqual(t, 0, s.ExitCode)
}

func TestTaskSummary_NotWrittenOnInfrastructureFailure(t *testing.T) {
	tc := newMockTransferClient()
	runSingleTask(t, tc, &recordingDockerCommand{err: errors.New("docker daemon unavailable")})

	_, ok := readTaskSummary(t, tc)
	require.False(t, ok, "no exit code means no task_summary.json")
}

func TestPriorResult_SuccessSkipsExecution(t *testing.T) {
	tc := newMockTransferClient()
	tc.files[retryTestResultPath+"/"+TaskSummaryFile] = []byte(`{"exit_code": 0}`)

	q := runSingleTask(t, tc, &recordingDockerCommand{})

	task := q.taskByID("t1")
	require.Equal(t, StatusSuccess, task.Status)
	require.True(t, task.FoundPriorResult)
	require.NotContains(t, tc.files, task.LogPath, "task must not have been executed")
}

func TestPriorResult_NonZeroExitRunsNormally(t *testing.T) {
	tc := newMockTransferClient()
	tc.files[retryTestResultPath+"/"+TaskSummaryFile] = []byte(`{"exit_code": 1}`)

	q := runSingleTask(t, tc, &recordingDockerCommand{})

	task := q.taskByID("t1")
	require.Equal(t, StatusSuccess, task.Status)
	require.False(t, task.FoundPriorResult)
	require.Contains(t, tc.files, task.LogPath, "task should have been executed")
	s, ok := readTaskSummary(t, tc)
	require.True(t, ok)
	require.Equal(t, 0, s.ExitCode, "the prior summary is overwritten by the new run")
}

func TestPriorResult_CorruptSummaryRunsNormally(t *testing.T) {
	tc := newMockTransferClient()
	tc.files[retryTestResultPath+"/"+TaskSummaryFile] = []byte(`not json`)

	q := runSingleTask(t, tc, &recordingDockerCommand{})

	task := q.taskByID("t1")
	require.Equal(t, StatusSuccess, task.Status)
	require.False(t, task.FoundPriorResult)
	require.Contains(t, tc.files, task.LogPath)
}
