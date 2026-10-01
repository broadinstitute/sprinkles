package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPollerStats_RunRecordsOutcomes(t *testing.T) {
	w := newWorld()
	s := NewPollerStats(w.Clock)
	s.Register("p", 30*time.Second)

	require.NoError(t, s.Run("p", TriggerTimer, func() error { return nil }))
	boom := errors.New("boom")
	require.ErrorIs(t, s.Run("p", TriggerNotification, func() error { return boom }), boom)
	require.ErrorIs(t, s.Run("p", TriggerNotification, func() error { return boom }), boom)

	snap := s.Snapshot()
	require.Len(t, snap.Pollers, 1)
	p := snap.Pollers[0]
	assert.Equal(t, "p", p.Name)
	assert.Equal(t, 30*time.Second, p.MaxInterval)
	assert.EqualValues(t, 3, p.Runs)
	assert.EqualValues(t, 1, p.TimerRuns)
	assert.EqualValues(t, 2, p.NotificationRuns)
	assert.EqualValues(t, 2, p.Failures)
	assert.EqualValues(t, 2, p.ConsecutiveFailures)
	assert.Equal(t, "boom", p.LastError)
	assert.False(t, p.LastSuccess.IsZero())

	// A success resets the consecutive-failure streak but not the total.
	require.NoError(t, s.Run("p", TriggerTimer, func() error { return nil }))
	p = s.Snapshot().Pollers[0]
	assert.EqualValues(t, 0, p.ConsecutiveFailures)
	assert.EqualValues(t, 2, p.Failures)
}

func TestPollerStats_CountersAndOrder(t *testing.T) {
	w := newWorld()
	s := NewPollerStats(w.Clock)
	s.Register("b", time.Second)
	s.Register("a", time.Second)
	s.Add("a", "things", 2)
	s.Add("a", "things", 3)
	s.Add("a", "ignored", 0)

	snap := s.Snapshot()
	require.Len(t, snap.Pollers, 2)
	assert.Equal(t, "b", snap.Pollers[0].Name, "registration order is preserved")
	assert.Equal(t, map[string]int64{"things": 5}, snap.Pollers[1].Counters)
}

func TestPollerStats_NilIsNoop(t *testing.T) {
	var s *PollerStats
	s.Register("p", time.Second)
	s.Add("p", "x", 1)
	ran := false
	require.NoError(t, s.Run("p", TriggerTimer, func() error { ran = true; return nil }))
	assert.True(t, ran)
	assert.Empty(t, s.Snapshot().Pollers)
}

func TestPollerStats_TaskRecoveryCounters(t *testing.T) {
	w := newWorld()
	w.Workers.Add(&Worker{
		WorkerID:        "w1",
		WorkpoolID:      "pool-1",
		Status:          "started",
		HeartbeatExpiry: epoch.Add(-1 * time.Second),
	})
	w.Tasks.Add(&Task{TaskID: "t1", WorkpoolID: "pool-1", Status: TaskStatusRunning, OwningWorkerID: "w1"})
	w.Tasks.Add(&Task{TaskID: "t2", WorkpoolID: "pool-1", Status: TaskStatusClaimed, OwningWorkerID: "w1"})

	w.A.runPoller(PollerTaskRecovery, func() error { return w.A.runRequeueOrphanedTasks(context.Background()) })

	var got PollerSnapshot
	for _, p := range w.A.PollerStats().Snapshot().Pollers {
		if p.Name == PollerTaskRecovery {
			got = p
		}
	}
	assert.EqualValues(t, 1, got.Runs)
	assert.Equal(t, map[string]int64{"workers_marked_zombie": 1, "tasks_requeued": 2}, got.Counters)
}
