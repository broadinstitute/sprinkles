package monitor

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/broadinstitute/sprinkles/scheduler"
)

// Default values for WorkPool watchdog parameters.
const (
	defaultMaxWorkersPerRequest        = 100
	defaultMinTimeBetweenPolls         = 2 * time.Second
	defaultMaxTimeBetweenPolls         = 30 * time.Second
	defaultMaxTimeToStartWorker        = 5 * time.Minute
	defaultMaxTimeInQueue              = 15 * time.Minute
	defaultVMShutdownGracePeriod       = 1 * time.Minute
	defaultMaxZombiesBeforeAbort       = 3
	defaultMaxConsecutiveFailedBatches = 2

	expiryCleanerInterval = 30 * time.Minute

	// defaultIdleHeartbeat is how often each poller still runs while every
	// workpool is idle and nothing has woken it. It's the safety net for a
	// missed Pub/Sub message (or work that appeared while the monitor was
	// down); normal wake-ups come from events, not from this timer.
	defaultIdleHeartbeat = 5 * time.Minute

	// activePoolCountUnknown means the monitor doesn't currently know how many
	// workpools are non-idle, so no poller may be skipped.
	activePoolCountUnknown = -1

	// defaultHaltCheckWindow bounds how far back checkHaltThreshold looks in
	// the Events log. Kept short (not an all-time window) both to keep the
	// Firestore query cheap and because the failure mode we care about most
	// is "things never start" — a burst of failures right now — not a slow
	// drift over hours.
	defaultHaltCheckWindow = 1 * time.Hour

	// defaultPreemptionLookbackWindow bounds how far back
	// runProvisioningPollForWorkpool looks for zombie incidents (our proxy
	// for preemption) when enforcing MaxPreemptibleWorkerAttempts. A rolling
	// window (rather than a lifetime total) means the preemptible budget
	// recovers over time instead of being permanently consumed by one job.
	defaultPreemptionLookbackWindow = 1 * time.Hour
)

// Incident types recorded on workpool_incident events, distinguishing the
// kind of anomaly recordIncident was called for. IncidentTypeZombie is used
// as a (deliberately imprecise) proxy for "this worker was preempted" when
// enforcing MaxPreemptibleWorkerAttempts — see runProvisioningPollForWorkpool.
const (
	// IncidentTypeZombie: a worker's heartbeat expired without a clean
	// shutdown (runRequeueOrphanedTasks) — crashed, preempted, or otherwise
	// stopped responding.
	IncidentTypeZombie = "zombie"
	// IncidentTypeZombieTerminated: the cluster reconciler found a worker
	// whose heartbeat expired while its VM was still running (per GCP), and
	// terminated the VM.
	IncidentTypeZombieTerminated = "zombie_terminated"
	// IncidentTypeVMStartupFailure: a VM never registered a worker within
	// the startup grace period, while other VMs in the same batch did.
	IncidentTypeVMStartupFailure = "vm_startup_failure"
	// IncidentTypeOverProvisioned: more VMs were found running than the
	// batch expected; the batch was aborted.
	IncidentTypeOverProvisioned = "over_provisioned"
	// IncidentTypeNoWorkersRegistered: no worker ever registered for a
	// batch within the startup grace period; the batch was aborted.
	IncidentTypeNoWorkersRegistered = "no_workers_registered"
	// IncidentTypeTooManyZombies: the number of zombie workers in a batch
	// exceeded MaxZombiesBeforeAbort; the batch was aborted.
	IncidentTypeTooManyZombies = "too_many_zombies"
	// IncidentTypeBatchAPIFailure: GCP's Batch API itself reported the job
	// as failed.
	IncidentTypeBatchAPIFailure = "batch_api_failure"
	// IncidentTypeBatchCreateFailure: the Batch API rejected (or failed) the
	// request to create a job — typically a permissions or configuration
	// problem, e.g. the caller can't act as the workpool's service account.
	IncidentTypeBatchCreateFailure = "batch_create_failure"
)

// activeTasks is the set of task statuses that are orphaned back to pending when a worker dies.
var activeTasks = []TaskStatus{TaskStatusClaimed, TaskStatusRunning, TaskStatusWriting}

// Monitor runs provisioning, watchdog, and job-summary bookkeeping logic.
type Monitor struct {
	clock                  scheduler.Clock
	batchAPI               BatchAPIClient
	pools                  WorkPoolStore
	batches                BatchRequestStore
	workers                WorkerStore
	tasks                  TaskStore
	pubsub                 PubSubReceiver
	jobEvents              JobEventReceiver
	jobSummaries           JobSummaryStore
	jobTerminated          JobTerminatedPublisher
	jobResultWriter        JobResultWriter
	workpoolStatePublisher WorkpoolStatePublisher
	batchOutcomes          BatchOutcomePublisher
	workpoolIncidents      WorkpoolIncidentPublisher
	workerEvents           WorkerEventPublisher
	workPoolSummaries      WorkPoolSummaryStore
	events                 EventStore
	lastEventTime          time.Time
	expiry                 ExpiryStore
	verbose                bool
	dbName                 string
	lingerDuration         time.Duration
	lastActivity           time.Time
	errorLog               *ErrorLog
	stats                  *PollerStats
	// trigger records how the poller callback currently being run was
	// started (TriggerNotification or TriggerTimer). Only touched from
	// RunMonitorLoop's goroutine, which runs callbacks one at a time.
	trigger string
	// notifyWorkPoolSummary wakes the workpool summary poll early; set by
	// RunMonitorLoop (nil until then, e.g. in unit tests that call pollers
	// directly).
	notifyWorkPoolSummary func()

	// Idle gating (see runPoller). activePoolCount is how many non-idle
	// workpools the most recent ListActive found, or activePoolCountUnknown.
	activePoolCount atomic.Int64
	idleHeartbeat   time.Duration
	idleMu          sync.Mutex
	wakePending     map[string]bool      // poller -> an event asked for it to run
	lastRun         map[string]time.Time // poller -> when it last actually ran

	// lease, if set, is the lease this monitor must hold to run (see lease.go).
	lease *monitorLease
}

// ErrorLog returns the monitor's in-memory log of recent background-poller
// errors (see RunMonitorLoop).
func (a *Monitor) ErrorLog() *ErrorLog { return a.errorLog }

// PollerStats returns the monitor's in-memory record of what each background
// poller has been doing (see RunMonitorLoop).
func (a *Monitor) PollerStats() *PollerStats { return a.stats }

// runPoller runs one pass of the named poller, recording it in the poller
// stats and the error log if it fails.
func (a *Monitor) runPoller(ctx context.Context, name string, fn func(context.Context) error) {
	if a.skipWhileIdle(name) {
		a.stats.Skip(name)
		return
	}
	a.idleMu.Lock()
	a.lastRun[name] = a.clock.Now()
	delete(a.wakePending, name)
	a.idleMu.Unlock()

	// Label the context so Firestore calls made by this poller are
	// attributed to it in FirestoreCallStats.
	ctx = WithFirestoreLabel(ctx, name)
	if err := a.stats.Run(name, a.trigger, func() error { return fn(ctx) }); err != nil {
		a.errorLog.Add("%s: %v", name, err)
	}
}

// SetFirestoreCallStats attaches the counters for the monitor's Firestore
// calls (see FirestoreCallStats.NewClient) so they can be reported
// alongside the poller stats.
func (a *Monitor) SetFirestoreCallStats(s *FirestoreCallStats) { a.stats.firestore = s }

// SetIdleHeartbeat sets how often each poller runs while every workpool is
// idle and no event has woken it. Zero disables idle gating entirely, so
// every poller runs on every tick.
func (a *Monitor) SetIdleHeartbeat(d time.Duration) { a.idleHeartbeat = d }

// listActivePools returns the non-idle workpools and remembers how many there
// were, which is what lets pollers that don't list pools themselves (task
// recovery, job summary) tell that everything is idle.
func (a *Monitor) listActivePools(ctx context.Context) ([]*WorkPoolWithState, error) {
	pools, err := a.pools.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	a.activePoolCount.Store(int64(len(pools)))
	return pools, nil
}

// wakeOnNotify wraps a scheduler notify func so that calling it also records
// that the named poller was asked to run. A woken poller is never skipped for
// idleness, however it ends up being scheduled (the scheduler may run a
// notification late, as a "timer" run, when throttling coalesces it).
func (a *Monitor) wakeOnNotify(name string, notify func()) func() {
	return func() {
		a.idleMu.Lock()
		a.wakePending[name] = true
		a.idleMu.Unlock()
		notify()
	}
}

// skipWhileIdle reports whether the named poller can skip this run: only when
// idle gating is on, every workpool was last seen idle, no event has asked
// for this poller, and it has run within the idle heartbeat. Gating needs the
// job event receiver, since job_created events are what wake the monitor
// when work arrives; without one every poller always runs.
func (a *Monitor) skipWhileIdle(name string) bool {
	if a.idleHeartbeat <= 0 || a.jobEvents == nil {
		return false
	}
	if a.activePoolCount.Load() != 0 {
		return false
	}
	a.idleMu.Lock()
	defer a.idleMu.Unlock()
	if a.wakePending[name] {
		return false
	}
	last, ran := a.lastRun[name]
	return ran && a.clock.Now().Sub(last) < a.idleHeartbeat
}

// SetVerbose enables or disables verbose poll logging.
func (a *Monitor) SetVerbose(v bool) { a.verbose = v }

// SetJobEventReceiver sets an optional receiver for job_created events.
// When set, a new job submission triggers an immediate provisioning poll.
func (a *Monitor) SetJobEventReceiver(r JobEventReceiver) { a.jobEvents = r }

// SetJobSummaryStore sets the store used to read and write JobSummary documents.
func (a *Monitor) SetJobSummaryStore(s JobSummaryStore) { a.jobSummaries = s }

// SetWorkPoolSummaryStore sets the store used to write WorkPoolSummary documents.
func (a *Monitor) SetWorkPoolSummaryStore(s WorkPoolSummaryStore) { a.workPoolSummaries = s }

// SetEventStore sets the store used to query job_created events.
func (a *Monitor) SetEventStore(s EventStore) { a.events = s }

// SetJobTerminatedPublisher sets the publisher used to emit job_terminated events.
func (a *Monitor) SetJobTerminatedPublisher(p JobTerminatedPublisher) { a.jobTerminated = p }

// SetJobResultWriter sets the writer used to persist a job completion summary
// when a job reaches a terminal state.
func (a *Monitor) SetJobResultWriter(w JobResultWriter) { a.jobResultWriter = w }

// SetWorkpoolStatePublisher sets the publisher used to emit workpool_state_change events.
func (a *Monitor) SetWorkpoolStatePublisher(p WorkpoolStatePublisher) {
	a.workpoolStatePublisher = p
}

// SetBatchOutcomePublisher sets the publisher used to emit batch_failed/batch_succeeded events.
func (a *Monitor) SetBatchOutcomePublisher(p BatchOutcomePublisher) {
	a.batchOutcomes = p
}

// SetWorkpoolIncidentPublisher sets the publisher used to emit workpool_incident events.
func (a *Monitor) SetWorkpoolIncidentPublisher(p WorkpoolIncidentPublisher) {
	a.workpoolIncidents = p
}

// SetWorkerEventPublisher sets the publisher used to emit worker_stopped events.
func (a *Monitor) SetWorkerEventPublisher(p WorkerEventPublisher) {
	a.workerEvents = p
}

// SetExpiryStore sets the store used to garbage-collect expired documents.
func (a *Monitor) SetExpiryStore(s ExpiryStore) { a.expiry = s }

// SetLingerDuration sets how long the monitor runs without any workpool having
// pending or running tasks before shutting down. Zero means run forever.
func (a *Monitor) SetLingerDuration(d time.Duration) { a.lingerDuration = d }

// vlogf logs only when verbose mode is on.
func (a *Monitor) vlogf(format string, args ...any) {
	if a.verbose {
		log.Printf(format, args...)
	}
}

func New(
	clock scheduler.Clock,
	batchAPI BatchAPIClient,
	pools WorkPoolStore,
	batches BatchRequestStore,
	workers WorkerStore,
	tasks TaskStore,
	pubsub PubSubReceiver,
	dbName string,
) *Monitor {
	m := &Monitor{
		clock:    clock,
		batchAPI: batchAPI,
		pools:    pools,
		batches:  batches,
		workers:  workers,
		tasks:    tasks,
		pubsub:   pubsub,
		dbName:   dbName,
		errorLog: NewErrorLog(DefaultErrorLogCapacity),
		stats:    NewPollerStats(clock),
		trigger:  TriggerTimer,

		idleHeartbeat: defaultIdleHeartbeat,
		wakePending:   map[string]bool{},
		lastRun:       map[string]time.Time{},
	}
	m.activePoolCount.Store(activePoolCountUnknown)
	return m
}

// RunMonitorLoop runs the monitor until ctx is cancelled.
// Blocks; run in a dedicated goroutine.
func (a *Monitor) RunMonitorLoop(ctx context.Context) {
	// Stop everything started below (notably lease renewal) however the loop
	// ends.
	ctx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()

	var lastActivityPrinted time.Time

	sched := scheduler.New(a.clock)

	// Register every poller up front so the dashboard lists all of them
	// (with "never run" state) from startup, and knows how long each may
	// legitimately go between runs.
	for _, name := range []string{
		PollerClusterReconciler, PollerProvisioning, PollerTaskRecovery,
		PollerBatchStartup, PollerJobSummary, PollerWorkPoolSummary,
	} {
		a.stats.Register(name, defaultMaxTimeBetweenPolls)
	}
	if a.expiry != nil {
		a.stats.Register(PollerExpiryCleaner, expiryCleanerInterval)
	}

	if a.lingerDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		a.lastActivity = time.Now()
		sched.Add(defaultMinTimeBetweenPolls, defaultMinTimeBetweenPolls, func() {
			if lastActivityPrinted != a.lastActivity {
				a.vlogf("Last activity at %s. Will shut down if no activity before %s", a.lastActivity.Format(time.RFC3339), a.lastActivity.Add(a.lingerDuration).Format(time.RFC3339))
				lastActivityPrinted = a.lastActivity
			}
			now := time.Now()
			// if it's been too long since the monitor did anything, shutdown
			if now.Sub(a.lastActivity) > a.lingerDuration {
				cancel()
			}
		})
		defer cancel()
	}

	// Cluster reconciler: Triggered by PubSub notifications for started batches;
	// falls back to max_time_between_polls if no notification arrives.
	notifyClusterReconciler := sched.Add(defaultMinTimeBetweenPolls, defaultMaxTimeBetweenPolls, func() {
		a.vlogf("Started: Reconciling our records against google's")
		a.runPoller(ctx, PollerClusterReconciler, a.runClusterReconciler)
		a.vlogf("Completed: Reconciling our records against google's")
	})
	notifyClusterReconciler = a.wakeOnNotify(PollerClusterReconciler, notifyClusterReconciler)

	// Provisioner: Responsible for requesting VMs for pending tasks. Triggered by job_created events; falls back to 1-minute timer.
	notifyProvisioning := sched.Add(defaultMinTimeBetweenPolls, defaultMaxTimeBetweenPolls, func() {
		a.vlogf("Started: Checking for workpools which need new workers")
		a.runPoller(ctx, PollerProvisioning, a.runProvisioningPoll)
		a.vlogf("Completed: Checking for workpools which need new workers")
	})
	notifyProvisioning = a.wakeOnNotify(PollerProvisioning, notifyProvisioning)

	// Task recovery: Fixed 30-second interval; no notification trigger.
	sched.Add(defaultMinTimeBetweenPolls, defaultMaxTimeBetweenPolls, func() {
		a.vlogf("Started: Checking for ophaned jobs")
		a.runPoller(ctx, PollerTaskRecovery, a.runRequeueOrphanedTasks)
		a.vlogf("Completed: Checking for ophaned jobs")
	})

	// Batch startup monitor: Triggered by PubSub notifications for pending batches; and fallback timer
	notifyBatchStartupMonitor := sched.Add(defaultMinTimeBetweenPolls, defaultMaxTimeBetweenPolls, func() {
		a.vlogf("Started: Checking to see if workers successfully starting")
		a.runPoller(ctx, PollerBatchStartup, a.runBatchStartupMonitor)
		a.vlogf("Completed: Checking to see if workers successfully starting")
	})
	notifyBatchStartupMonitor = a.wakeOnNotify(PollerBatchStartup, notifyBatchStartupMonitor)

	// Expiry cleaner: deletes documents whose expiry field is in the past. Fixed 30-minute interval.
	if a.expiry != nil {
		sched.Add(expiryCleanerInterval, expiryCleanerInterval, func() {
			a.vlogf("Deleting expired objects from firestore...")
			a.runPoller(ctx, PollerExpiryCleaner, a.runExpiryCleaner)
			a.vlogf("Deleting expired objects from firestore complete")
		})
	}

	// Job summary poll: recomputes JobSummary for every non-terminal job.
	// Triggered by job_created and task_state_update events; falls back to max interval.
	var notifyJobSummary func()
	notifyJobSummary = sched.Add(defaultMinTimeBetweenPolls, defaultMaxTimeBetweenPolls, func() {
		a.vlogf("Starting: Computing job summaries")
		a.runPoller(ctx, PollerJobSummary, a.runJobSummaryPoll)
		a.vlogf("Completed: Computing job summaries")
	})
	notifyJobSummary = a.wakeOnNotify(PollerJobSummary, notifyJobSummary)

	// WorkPool summary poll: recomputes WorkPoolSummary for every workpool.
	// Triggered by batch notifications and job events; falls back to max interval.
	var notifyWorkPoolSummary func()
	notifyWorkPoolSummary = sched.Add(defaultMinTimeBetweenPolls, defaultMaxTimeBetweenPolls, func() {
		a.vlogf("Starting: Compute workpool summaries")
		a.runPoller(ctx, PollerWorkPoolSummary, a.runWorkPoolSummaryPoll)
		a.vlogf("Completed: Compute workpool summaries")
	})
	notifyWorkPoolSummary = a.wakeOnNotify(PollerWorkPoolSummary, notifyWorkPoolSummary)

	a.notifyWorkPoolSummary = notifyWorkPoolSummary

	// Route PubSub notifications to the appropriate pollers in a background goroutine.
	// A notification with Err set means the receive loop failed fatally; propagate it.
	fatalErrCh := make(chan error, 1)

	// Keep the monitor lease alive; losing it (another monitor took over) or
	// being unable to renew it stops the loop.
	if a.lease != nil {
		go a.runLeaseRenewal(ctx, func(err error) {
			select {
			case fatalErrCh <- err:
			default: // a fatal error is already pending
			}
		})
	}

	if a.jobEvents != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case n := <-a.jobEvents.JobEvents():
					if n.Err != nil {
						a.errorLog.Add("job events: fatal error: %v", n.Err)
						fatalErrCh <- n.Err
						return
					}
					a.vlogf("job events: received %s event for job %s", n.EventType, n.JobID)
					if n.EventType == "job_created" || n.EventType == "workpool_state_change" {
						log.Printf("Waking up provision check due to %s event", string(n.EventType))
						notifyProvisioning()
					}
					if n.EventType == "job_created" && n.JobID != "" && a.jobSummaries != nil {
						if err := a.reviveJobSummary(WithFirestoreLabel(ctx, "job event: revive job summary"), n.JobID); err != nil {
							a.errorLog.Add("reviving job summary for job %s: %v", n.JobID, err)
						}
					}
					if notifyJobSummary != nil {
						log.Printf("Waking up job summarizer due to %s event", string(n.EventType))
						notifyJobSummary()
					}
					if notifyWorkPoolSummary != nil {
						log.Printf("Waking up workpool summarizer due to %s event", string(n.EventType))
						notifyWorkPoolSummary()
					}
				}
			}
		}()
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case n := <-a.pubsub.Notifications():
				if n.Err != nil {
					a.errorLog.Add("pubsub: fatal error: %v", n.Err)
					fatalErrCh <- n.Err
					return
				}
				a.vlogf("pubsub: received notification for batch %s", n.BatchID)
				a.routeNotification(WithFirestoreLabel(ctx, "batch notification routing"), n.BatchID, notifyClusterReconciler, notifyBatchStartupMonitor)
				if notifyWorkPoolSummary != nil {
					notifyWorkPoolSummary()
				}
			}
		}
	}()

	// Main scheduler loop.
	for {
		select {
		case <-ctx.Done():
			return
		case <-fatalErrCh:
			return
		default:
		}

		timerCh, runDue := sched.GetNextCallback()
		select {
		case <-ctx.Done():
			return
		case <-fatalErrCh:
			return
		case cb := <-sched.NotifyChannel():
			a.trigger = TriggerNotification
			cb()
			a.trigger = TriggerTimer
		case <-timerCh:
			runDue()
		}
	}
}

// routeNotification looks up a batch and notifies the appropriate poller.
func (a *Monitor) routeNotification(ctx context.Context, batchID string, notifyClusterReconciler, notifyBatchStartupMonitor func()) {
	batch, err := a.batches.Get(ctx, batchID)
	if err != nil {
		a.errorLog.Add("notification: failed to look up batch %s: %v — notifying both the cluster reconciler and the batch startup monitor", batchID, err)
		notifyClusterReconciler()
		notifyBatchStartupMonitor()
		return
	}

	switch batch.Status {
	case BatchStatusPending:
		notifyBatchStartupMonitor()
	case BatchStatusStarted:
		notifyClusterReconciler()
		// Completed/failed batches need no notification-driven check.
	}
}

// saveState persists workpool state and publishes a workpool_state_change
// event carrying message. Publish errors are logged but not returned so
// they don't block state writes.
func (a *Monitor) saveState(ctx context.Context, state *WorkPoolState, message string) error {
	if err := a.pools.SaveState(ctx, state); err != nil {
		return err
	}
	if a.workpoolStatePublisher != nil {
		if err := a.workpoolStatePublisher.PublishWorkpoolStateChange(ctx, state.WorkpoolID, string(state.State), message); err != nil {
			a.errorLog.Add("saveState: publish workpool_state_change for %s: %v", state.WorkpoolID, err)
		}
	}
	return nil
}

// recordIncident publishes a workpool_incident event for a watchdog anomaly.
// It is purely a log-to-Events operation — it does not mutate WorkPoolState.
// The ok/unhealthy transition is decided by the WorkPool summary poll
// (updateWorkPoolSummary), which derives it (along with the
// StateMessage/LastIncidentAt/IncidentCount shown on WorkPoolSummary) from
// recent workpool_incident events — see EventStore.ListRecentWorkpoolIncidents.
func (a *Monitor) recordIncident(ctx context.Context, workpoolID, incidentType, message string) {
	if a.workpoolIncidents != nil {
		if err := a.workpoolIncidents.PublishWorkpoolIncident(ctx, workpoolID, incidentType, message); err != nil {
			a.errorLog.Add("recordIncident: publish workpool_incident for %s: %v", workpoolID, err)
		}
	}
}

// saveTerminalBatch saves a batch that has just reached a terminal status
// (completed, failed, deleted or terminated) and wakes the workpool summary
// poll. A pool is only idle once it has no pending/started batches (see
// updateWorkPoolSummary), so the summary must be recomputed right after a
// batch ends rather than waiting for its next timer.
func (a *Monitor) saveTerminalBatch(ctx context.Context, batch *BatchAPIRequest) error {
	if err := a.batches.Save(ctx, batch); err != nil {
		return err
	}
	if a.notifyWorkPoolSummary != nil {
		a.notifyWorkPoolSummary()
	}
	return nil
}

// markBatchFailed records batch as failed (GCP itself reported the job as
// failed), logs an incident, and publishes a batch_failed event. Use
// markBatchTerminated instead when the monitor is the one deciding to kill an
// otherwise-live job.
func (a *Monitor) markBatchFailed(ctx context.Context, ws *WorkPoolWithState, batch *BatchAPIRequest, reason string, now time.Time) error {
	return a.markBatchDone(ctx, ws, batch, BatchStatusFailed, IncidentTypeBatchAPIFailure, reason, now)
}

// markBatchTerminated records batch as terminated (the monitor's own
// bookkeeping — VM counts, worker registrations, heartbeats — found an
// anomaly and killed a job GCP hadn't reported any problem with), sets
// TerminationReason, logs an incident, and publishes a batch_failed event (the
// same outcome event type as markBatchFailed — both mean "this batch didn't
// work out" for halt-counting purposes).
func (a *Monitor) markBatchTerminated(ctx context.Context, ws *WorkPoolWithState, batch *BatchAPIRequest, incidentType, reason string, now time.Time) error {
	batch.TerminationReason = reason
	return a.markBatchDone(ctx, ws, batch, BatchStatusTerminated, incidentType, reason, now)
}

// markBatchDone centralizes the pattern repeated across
// cluster_reconciler.go and batch_startup_monitor.go's failure-detection
// call sites: set batch.Status/Unhealthy, save it, log an incident, and
// publish a batch_failed event. Halting is not decided here:
// checkHaltThreshold runs periodically from the provisioning poll.
func (a *Monitor) markBatchDone(ctx context.Context, ws *WorkPoolWithState, batch *BatchAPIRequest, status BatchStatus, incidentType, reason string, now time.Time) error {
	batch.Status = status
	batch.Unhealthy = true
	a.recordIncident(ctx, ws.Pool.WorkpoolID, incidentType, reason)
	if err := a.saveTerminalBatch(ctx, batch); err != nil {
		return fmt.Errorf("save batch: %w", err)
	}
	if a.batchOutcomes != nil {
		if err := a.batchOutcomes.PublishBatchFailed(ctx, ws.Pool.WorkpoolID, reason); err != nil {
			a.errorLog.Add("markBatchDone: publish batch_failed for workpool %s: %v", ws.Pool.WorkpoolID, err)
		}
	}
	return nil
}

// checkHaltThreshold transitions the workpool to halted if the last N batch
// outcomes recorded in the Events log within defaultHaltCheckWindow all
// failed. Saves state if it transitions. The halt transition is reported
// only via workpool_state_change — it is not itself published as a
// workpool_incident (that event type is reserved for the anomalies that led
// up to the halt).
func (a *Monitor) checkHaltThreshold(ctx context.Context, pool *WorkPool, state *WorkPoolState) error {
	n := pool.MaxConsecutiveFailedBatches
	if n <= 0 {
		n = defaultMaxConsecutiveFailedBatches
	}

	since := a.clock.Now().Add(-defaultHaltCheckWindow)
	if state.HaltResetAt.After(since) {
		// A manual reset (see ResetHaltedWorkPool) happened more recently than
		// the normal lookback window would reach; ignore anything before it so
		// stale pre-reset failures can't immediately re-trigger a halt.
		since = state.HaltResetAt
	}
	recent, err := a.events.ListRecentBatchOutcomes(ctx, pool.WorkpoolID, since)
	if err != nil {
		return fmt.Errorf("list recent batch outcomes for workpool %s: %w", pool.WorkpoolID, err)
	}

	// ListRecentBatchOutcomes returns most-recent-first; take only the most recent n.
	if len(recent) > n {
		recent = recent[:n]
	}

	if len(recent) == n {
		allFailed := true
		for _, o := range recent {
			if !o.Failed {
				allFailed = false
				break
			}
		}
		if allFailed {
			state.State = WorkPoolStatusHalted
			haltMessage := fmt.Sprintf(
				"Last %d batches all failed within the last hour — possible configuration problem", n)
			if err := a.saveState(ctx, state, haltMessage); err != nil {
				return fmt.Errorf("save workpool %s: %w", pool.WorkpoolID, err)
			}
		}
	}
	return nil
}

// ResetHaltedWorkPool clears a workpool out of WorkPoolStatusHalted so
// provisioning can resume, recording the reset time so that batch failures
// from before it are ignored by the next checkHaltThreshold decision (see
// WorkPoolState.HaltResetAt). No-op (returns the current state, unchanged)
// if the workpool isn't currently halted. Exported so callers without a live
// *Monitor (e.g. the dashboard-backend, which talks to Firestore directly)
// can trigger a reset.
func ResetHaltedWorkPool(ctx context.Context, pools WorkPoolStore, workpoolID string, now time.Time) (previous WorkPoolStatus, err error) {
	ws, err := pools.Get(ctx, workpoolID)
	if err != nil {
		return "", err
	}
	previous = ws.State.State
	if previous != WorkPoolStatusHalted {
		return previous, nil
	}
	ws.State.State = WorkPoolStatusOK
	ws.State.HaltResetAt = now
	if err := pools.SaveState(ctx, ws.State); err != nil {
		return previous, fmt.Errorf("save workpool %s: %w", workpoolID, err)
	}
	return previous, nil
}

// RunRequeueOrphanedTasks runs one task-recovery pass: any tasks owned by workers
// whose heartbeats have expired are reset to pending. Exported for functional tests.
func (a *Monitor) RunRequeueOrphanedTasks(ctx context.Context) error {
	return a.runRequeueOrphanedTasks(ctx)
}

// RunJobSummaryPoll runs one job-summary pass: recomputes status for every
// non-terminal job and publishes job_terminated events as needed.
// Exported for functional tests; requires SetJobSummaryStore to have been called.
func (a *Monitor) RunJobSummaryPoll(ctx context.Context) error {
	return a.runJobSummaryPoll(ctx)
}

// ReviveJobSummary recomputes the JobSummary for jobID if it is terminal, as
// done on a job_created event for a retried job. Exported for functional tests;
// requires SetJobSummaryStore to have been called.
func (a *Monitor) ReviveJobSummary(ctx context.Context, jobID string) error {
	return a.reviveJobSummary(ctx, jobID)
}

// param returns v if v != 0, otherwise def. Used to apply per-workpool defaults.
func param[T int | time.Duration](v, def T) T {
	if v != 0 {
		return v
	}
	return def
}
