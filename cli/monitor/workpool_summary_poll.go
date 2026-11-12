package monitor

import (
	"context"
	"fmt"
	"log"
	"time"
)

// runWorkPoolSummaryPoll recomputes WorkPoolSummary for every workpool and
// writes both the current document and a history snapshot to Firestore.
// It also queries the Events collection for new job_created events since the
// last poll and transitions any idle workpool with a new job to ok.
func (a *Monitor) runWorkPoolSummaryPoll(ctx context.Context) error {
	// Collect workpool IDs that have received a new job since the last poll.
	poolsWithNewJob := make(map[string]bool)
	if a.events != nil {
		events, err := a.events.ListJobCreatedSince(ctx, a.lastEventTime)
		if err != nil {
			log.Printf("workpool summary poll: list job_created events: %v", err)
		} else {
			for _, e := range events {
				poolsWithNewJob[e.WorkpoolID] = true
				if e.Timestamp.After(a.lastEventTime) {
					a.lastEventTime = e.Timestamp
				}
			}
		}
	}

	// Recompute every pool that isn't idle, plus any idle pool that just
	// received a job (which is what flips it to ok). Idle pools with no new
	// job are skipped, so the cost doesn't grow with the number of pools
	// that have ever existed.
	activePools, err := a.listActivePools(ctx)
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(activePools))
	for _, ws := range activePools {
		have[ws.Pool.WorkpoolID] = true
	}
	for id := range poolsWithNewJob {
		if have[id] {
			continue
		}
		ws, err := a.pools.Get(ctx, id)
		if err != nil {
			// e.g. the workpool has expired since the event was recorded.
			log.Printf("workpool summary poll: load workpool %s for new job: %v", id, err)
			continue
		}
		activePools = append(activePools, ws)
	}

	for _, ws := range activePools {
		if err := a.updateWorkPoolSummary(ctx, ws); err != nil {
			log.Printf("workpool summary poll: pool %s: %v", ws.Pool.WorkpoolID, err)
		}
	}
	return nil
}

func (a *Monitor) vlogfIfChanged(msg, oldValue, newValue string) {
	if oldValue != newValue {
		a.vlogf("%s: %s -> %s", msg, oldValue, newValue)
	}
}

func (a *Monitor) updateWorkPoolSummary(ctx context.Context, ws *WorkPoolWithState) error {
	pool := ws.Pool
	state := ws.State
	if state.State == "" {
		state.State = WorkPoolStatusIdle
	}

	now := time.Now()

	// Collect all batches and derive counts.
	batches, err := a.batches.ListAllByWorkpool(ctx, pool.WorkpoolID)
	if err != nil {
		return err
	}

	batchStateCounts := make(map[string]int)
	preemptibleBatchIDs := make(map[string]bool)
	var expectedPreemptible, expectedNonpreemptible, unhealthyCount, activeBatchCount int
	for _, b := range batches {
		batchStateCounts[string(b.Status)]++
		if b.Unhealthy {
			unhealthyCount++
		}
		active := b.Status == BatchStatusPending || b.Status == BatchStatusStarted
		if active {
			activeBatchCount++
		}
		if b.Preemptible {
			preemptibleBatchIDs[b.BatchID] = true
			if active {
				expectedPreemptible += b.ExpectedVMCount
			}
		} else if active {
			expectedNonpreemptible += b.ExpectedVMCount
		}
	}

	// Split workers into preemptible and non-preemptible using the batch map.
	workers, err := a.workers.ListAllForWorkpool(ctx, pool.WorkpoolID)
	if err != nil {
		return err
	}
	preemptibleWorkerCounts := make(map[string]int)
	nonpreemptibleWorkerCounts := make(map[string]int)
	for _, w := range workers {
		if preemptibleBatchIDs[w.BatchID] {
			preemptibleWorkerCounts[w.Status]++
		} else {
			nonpreemptibleWorkerCounts[w.Status]++
		}
	}

	// Task counts across all statuses for this workpool.
	taskCounts, err := a.tasks.CountByWorkpool(ctx, pool.WorkpoolID)
	if err != nil {
		return err
	}

	// idle only when no non-terminal workers, tasks or batches remain. A
	// batch that is still pending/started means VMs may still be running (or
	// about to) even once every task is done and the workers have stopped.
	nonTerminal := activeBatchCount
	for _, w := range workers {
		if w.Status != "stopped" && w.Status != "zombie" {
			nonTerminal++
		}
	}
	for s, n := range taskCounts {
		if s != "success" && s != "error" && s != "failed" && s != "killed" {
			nonTerminal += n
		}
	}

	log.Printf("pool %s has %d nonterminals", state.WorkpoolID, nonTerminal)

	// StateMessage/LastIncidentAt/IncidentCount, and the ok/unhealthy
	// decision below, are both derived from the same workpool_incident
	// events recorded in the last hour, rather than carried as persisted
	// mutable state — see recordIncident.
	var stateMessage string
	var lastIncidentAt time.Time
	incidentCount := 0
	if a.events != nil {
		incidents, err := a.events.ListRecentWorkpoolIncidents(ctx, pool.WorkpoolID, a.clock.Now().Add(-defaultHaltCheckWindow))
		if err != nil {
			return fmt.Errorf("list recent workpool incidents for workpool %s: %w", pool.WorkpoolID, err)
		}
		incidentCount = len(incidents)
		if incidentCount > 0 {
			// ListRecentWorkpoolIncidents returns most-recent-first.
			stateMessage = incidents[0].Message
			lastIncidentAt = incidents[0].Timestamp
		}
	}

	oldState := state.State
	var newState WorkPoolStatus
	switch {
	case nonTerminal == 0:
		newState = WorkPoolStatusIdle
	case oldState == WorkPoolStatusHalted:
		// Halted is sticky; only draining to zero nonTerminal work (above)
		// clears it. checkHaltThreshold owns entering this state.
		newState = WorkPoolStatusHalted
	case incidentCount > 0:
		newState = WorkPoolStatusUnhealthy
	default:
		newState = WorkPoolStatusOK
	}

	if newState != oldState {
		a.vlogfIfChanged("Updating state", string(oldState), string(newState))
		state.State = newState
		message := ""
		if newState == WorkPoolStatusUnhealthy {
			message = stateMessage
		}
		if err := a.saveState(ctx, state, message); err != nil {
			return err
		}
		a.stats.Add(PollerWorkPoolSummary, "state_transitions", 1)
		// The set of idle pools just changed, so what the other pollers last
		// learned about it is stale.
		a.activePoolCount.Store(activePoolCountUnknown)
	}

	batchCounts := taskCountsFromMap(batchStateCounts)
	preemptibleWorkers := taskCountsFromMap(preemptibleWorkerCounts)
	nonpreemptibleWorkers := taskCountsFromMap(nonpreemptibleWorkerCounts)
	tasks := taskCountsFromMap(taskCounts)

	summary := &WorkPoolSummary{
		WorkpoolID:                    pool.WorkpoolID,
		MachineType:                   pool.MachineType,
		MaxPreemptibleWorkerAttempts:  pool.MaxPreemptibleWorkerAttempts,
		Expiry:                        now.Add(7 * 24 * time.Hour),
		LastUpdated:                   now,
		State:                         state.State,
		StateMessage:                  stateMessage,
		LastIncidentAt:                lastIncidentAt,
		IncidentCount:                 incidentCount,
		ExpectedPreemptibleWorkers:    expectedPreemptible,
		ExpectedNonpreemptibleWorkers: expectedNonpreemptible,
		UnhealthyBatchCount:           unhealthyCount,
		BatchAPIRequestCounts:         batchCounts,
		PreemptibleWorkers:            preemptibleWorkers,
		NonpreemptibleWorkers:         nonpreemptibleWorkers,
		Tasks:                         tasks,
	}
	if err := a.workPoolSummaries.Save(ctx, summary); err != nil {
		return err
	}

	history := &WorkPoolSummaryHistory{
		WorkpoolID:                    pool.WorkpoolID,
		Timestamp:                     now,
		Expiry:                        now.Add(7 * 24 * time.Hour),
		State:                         state.State,
		StateMessage:                  stateMessage,
		LastIncidentAt:                lastIncidentAt,
		IncidentCount:                 incidentCount,
		ExpectedPreemptibleWorkers:    expectedPreemptible,
		ExpectedNonpreemptibleWorkers: expectedNonpreemptible,
		UnhealthyBatchCount:           unhealthyCount,
		BatchAPIRequestCounts:         batchCounts,
		PreemptibleWorkers:            preemptibleWorkers,
		NonpreemptibleWorkers:         nonpreemptibleWorkers,
		Tasks:                         tasks,
	}
	if err := a.workPoolSummaries.SaveHistory(ctx, history); err != nil {
		return err
	}
	a.stats.Add(PollerWorkPoolSummary, "pools_recomputed", 1)
	return nil
}

// RunWorkPoolSummaryPoll runs one workpool-summary pass. Exported for functional tests.
func (a *Monitor) RunWorkPoolSummaryPoll(ctx context.Context) error {
	return a.runWorkPoolSummaryPoll(ctx)
}
