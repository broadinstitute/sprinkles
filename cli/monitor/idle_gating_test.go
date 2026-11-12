package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubJobEvents satisfies JobEventReceiver. Idle gating is only enabled when
// a job event receiver is configured (its job_created events are what wake
// the monitor).
type stubJobEvents struct{ ch chan JobNotification }

func (s stubJobEvents) JobEvents() <-chan JobNotification { return s.ch }

// gatedWorld is a world with idle gating on and every pool known idle.
func gatedWorld() *World {
	w := newWorld()
	w.A.SetJobEventReceiver(stubJobEvents{ch: make(chan JobNotification)})
	w.A.SetIdleHeartbeat(5 * time.Minute)
	return w
}

// runs reports whether a poller run actually executed fn.
func runs(w *World, name string) bool {
	ran := false
	w.A.runPoller(context.Background(), name, func(context.Context) error { ran = true; return nil })
	return ran
}

func TestIdleGating_FirstRunAlwaysExecutes(t *testing.T) {
	w := gatedWorld()
	w.A.activePoolCount.Store(0)
	assert.True(t, runs(w, PollerTaskRecovery), "a poller that has never run must run once")
}

func TestIdleGating_SkipsWhileIdleUntilHeartbeat(t *testing.T) {
	w := gatedWorld()
	w.A.activePoolCount.Store(0)
	require.True(t, runs(w, PollerTaskRecovery))

	w.Clock.Advance(30 * time.Second)
	assert.False(t, runs(w, PollerTaskRecovery), "idle: skipped inside the heartbeat")

	w.Clock.Advance(5 * time.Minute)
	assert.True(t, runs(w, PollerTaskRecovery), "idle: runs once the heartbeat has elapsed")
}

func TestIdleGating_SkipIsRecordedInStats(t *testing.T) {
	w := gatedWorld()
	w.A.activePoolCount.Store(0)
	require.True(t, runs(w, PollerTaskRecovery))
	w.Clock.Advance(time.Second)
	require.False(t, runs(w, PollerTaskRecovery))

	var got PollerSnapshot
	for _, p := range w.A.PollerStats().Snapshot().Pollers {
		if p.Name == PollerTaskRecovery {
			got = p
		}
	}
	assert.EqualValues(t, 1, got.Runs, "a skip is not a run")
	assert.EqualValues(t, 1, got.Counters["idle_skips"])
	assert.False(t, got.LastSkipped.IsZero())
}

func TestIdleGating_NotSkippedWhenAnyPoolActiveOrUnknown(t *testing.T) {
	for name, count := range map[string]int64{"active": 1, "unknown": activePoolCountUnknown} {
		t.Run(name, func(t *testing.T) {
			w := gatedWorld()
			w.A.activePoolCount.Store(0)
			require.True(t, runs(w, PollerTaskRecovery))
			w.A.activePoolCount.Store(count)
			w.Clock.Advance(time.Second)
			assert.True(t, runs(w, PollerTaskRecovery))
		})
	}
}

// A poller an event asked to run is never skipped, even if it ends up
// running as a late "timer" run after the scheduler coalesced the
// notification, and only that poller is affected.
func TestIdleGating_WakeBypassesSkipOnce(t *testing.T) {
	w := gatedWorld()
	w.A.activePoolCount.Store(0)
	require.True(t, runs(w, PollerProvisioning))
	require.True(t, runs(w, PollerJobSummary))
	w.Clock.Advance(time.Second)

	woken := false
	notify := w.A.wakeOnNotify(PollerProvisioning, func() { woken = true })
	notify()
	require.True(t, woken, "wrapper still forwards the notification")

	assert.True(t, runs(w, PollerProvisioning), "woken poller runs")
	w.Clock.Advance(time.Second)
	assert.False(t, runs(w, PollerProvisioning), "wake is consumed by that run")
	assert.False(t, runs(w, PollerJobSummary), "other pollers stay skipped")
}

func TestIdleGating_DisabledWithoutJobEventReceiver(t *testing.T) {
	w := newWorld() // no job event receiver
	w.A.activePoolCount.Store(0)
	require.True(t, runs(w, PollerTaskRecovery))
	w.Clock.Advance(time.Second)
	assert.True(t, runs(w, PollerTaskRecovery), "no wake signal, so never skip")
}

func TestIdleGating_DisabledWithZeroHeartbeat(t *testing.T) {
	w := gatedWorld()
	w.A.SetIdleHeartbeat(0)
	w.A.activePoolCount.Store(0)
	require.True(t, runs(w, PollerTaskRecovery))
	w.Clock.Advance(time.Second)
	assert.True(t, runs(w, PollerTaskRecovery))
}

func TestListActivePools_RecordsCountAndExcludesIdle(t *testing.T) {
	w := newWorld()
	w.Pools.AddActive(defaultPool("busy"))
	w.Pools.Add(defaultPool("idle"))
	w.Pools.AddState(&WorkPoolState{WorkpoolID: "idle", State: WorkPoolStatusIdle})
	w.Pools.Add(defaultPool("never-summarized"))
	w.Pools.Add(defaultPool("stopped"))
	w.Pools.AddState(&WorkPoolState{WorkpoolID: "stopped", State: WorkPoolStatusHalted})

	got, err := w.A.listActivePools(context.Background())
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, ws := range got {
		ids[ws.Pool.WorkpoolID] = true
	}
	assert.Equal(t, map[string]bool{"busy": true, "stopped": true}, ids)
	assert.EqualValues(t, 2, w.A.activePoolCount.Load())
}

// Idle pools with no new job are left alone by the summary poll; a pool that
// just received a job is picked up even though it's idle, which is what flips
// it to ok.
func TestSummaryPoll_SkipsIdlePoolsExceptThoseWithNewJobs(t *testing.T) {
	w := newWorld()
	for _, id := range []string{"quiet", "new-job"} {
		w.Pools.Add(defaultPool(id))
		w.Pools.AddState(&WorkPoolState{WorkpoolID: id, State: WorkPoolStatusIdle})
	}
	w.Tasks.Add(&Task{TaskID: "t1", WorkpoolID: "new-job", Status: TaskStatusPending})
	w.Events.AddEvent("new-job", epoch.Add(time.Minute))

	require.NoError(t, w.A.runWorkPoolSummaryPoll(context.Background()))

	assert.Equal(t, WorkPoolStatusOK, w.Pools.MustGetState("new-job").State)
	assert.Equal(t, WorkPoolStatusIdle, w.Pools.MustGetState("quiet").State)
	for _, sum := range w.WorkPoolSummaries.Summaries {
		assert.NotEqual(t, "quiet", sum.WorkpoolID, "an idle pool with no new job must not be recomputed")
	}
}

// A pool that has a job event but whose WorkPool doc is gone (expired) is
// skipped without failing the poll.
func TestSummaryPoll_NewJobForMissingPoolIsIgnored(t *testing.T) {
	w := newWorld()
	w.Events.AddEvent("gone", epoch.Add(time.Minute))
	require.NoError(t, w.A.runWorkPoolSummaryPoll(context.Background()))
}
