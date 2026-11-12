package monitor

import (
	"sync"
	"time"

	"github.com/broadinstitute/sprinkles/scheduler"
)

// Names of the monitor's background pollers, as shown in the dashboard and
// used as the prefix of their ErrorLog messages.
const (
	PollerClusterReconciler = "cluster reconciler"
	PollerProvisioning      = "provisioning poll"
	PollerTaskRecovery      = "task recovery"
	PollerBatchStartup      = "batch startup monitor"
	PollerExpiryCleaner     = "expiry cleaner"
	PollerJobSummary        = "job summary poll"
	PollerWorkPoolSummary   = "workpool summary poll"
)

// How a poller run was started: a Pub/Sub or job event woke it early
// ("notification"), or its fallback timer came due ("timer").
const (
	TriggerNotification = "notification"
	TriggerTimer        = "timer"
)

// PollerStats is an in-memory record of what each background poller has been
// doing since the monitor process started, so the dashboard can show whether
// the monitor is alive and what it's been up to. Like ErrorLog, it is
// per-process and resets on restart. Safe for concurrent use; all methods are
// no-ops on a nil receiver.
type PollerStats struct {
	clock     scheduler.Clock
	mu        sync.Mutex
	startedAt time.Time
	order     []string
	pollers   map[string]*pollerState
	// firestore counts the monitor's Firestore calls; nil if not enabled.
	firestore *FirestoreCallStats
}

type pollerState struct {
	maxInterval         time.Duration
	runs                int64
	notificationRuns    int64
	timerRuns           int64
	failures            int64
	consecutiveFailures int64
	lastStarted         time.Time
	lastFinished        time.Time
	lastSuccess         time.Time
	lastDuration        time.Duration
	lastError           string
	lastErrorAt         time.Time
	counters            map[string]int64
}

// PollerSnapshot is a point-in-time copy of one poller's stats.
type PollerSnapshot struct {
	Name                string
	MaxInterval         time.Duration
	Runs                int64
	NotificationRuns    int64
	TimerRuns           int64
	Failures            int64
	ConsecutiveFailures int64
	LastStarted         time.Time
	LastFinished        time.Time
	LastSuccess         time.Time
	LastDuration        time.Duration
	LastError           string
	LastErrorAt         time.Time
	Counters            map[string]int64
}

// FirestoreCalls returns the monitor's Firestore call counters, or nil if
// they aren't enabled.
func (s *PollerStats) FirestoreCalls() *FirestoreCallStats {
	if s == nil {
		return nil
	}
	return s.firestore
}

// PollerStatsSnapshot is a point-in-time copy of all pollers' stats, in
// registration order.
type PollerStatsSnapshot struct {
	StartedAt time.Time
	Pollers   []PollerSnapshot
}

func NewPollerStats(clock scheduler.Clock) *PollerStats {
	return &PollerStats{
		clock:     clock,
		startedAt: clock.Now(),
		pollers:   map[string]*pollerState{},
	}
}

// state returns the named poller's state, creating it on first use. Must be
// called with s.mu held.
func (s *PollerStats) state(name string) *pollerState {
	p, ok := s.pollers[name]
	if !ok {
		p = &pollerState{counters: map[string]int64{}}
		s.pollers[name] = p
		s.order = append(s.order, name)
	}
	return p
}

// Register declares a poller and the longest it should go between runs
// (used by the UI to flag a poller that has gone quiet), so it appears in
// snapshots before its first run.
func (s *PollerStats) Register(name string, maxInterval time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state(name).maxInterval = maxInterval
}

// Run executes fn, recording when it ran, how long it took, how it was
// triggered, and whether it failed. It returns fn's error.
func (s *PollerStats) Run(name, trigger string, fn func() error) error {
	if s == nil {
		return fn()
	}
	start := s.clock.Now()
	s.mu.Lock()
	s.state(name).lastStarted = start
	s.mu.Unlock()

	err := fn()

	end := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.state(name)
	p.runs++
	if trigger == TriggerNotification {
		p.notificationRuns++
	} else {
		p.timerRuns++
	}
	p.lastFinished = end
	p.lastDuration = end.Sub(start)
	if err != nil {
		p.failures++
		p.consecutiveFailures++
		p.lastError = err.Error()
		p.lastErrorAt = end
	} else {
		p.consecutiveFailures = 0
		p.lastSuccess = end
	}
	return err
}

// Add increments one of the named poller's cumulative work counters (e.g.
// "batches_submitted"); counters count since the monitor started.
func (s *PollerStats) Add(name, counter string, n int64) {
	if s == nil || n == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state(name).counters[counter] += n
}

// Snapshot returns a copy of the current stats.
func (s *PollerStats) Snapshot() PollerStatsSnapshot {
	if s == nil {
		return PollerStatsSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := PollerStatsSnapshot{StartedAt: s.startedAt}
	for _, name := range s.order {
		p := s.pollers[name]
		counters := make(map[string]int64, len(p.counters))
		for k, v := range p.counters {
			counters[k] = v
		}
		out.Pollers = append(out.Pollers, PollerSnapshot{
			Name:                name,
			MaxInterval:         p.maxInterval,
			Runs:                p.runs,
			NotificationRuns:    p.notificationRuns,
			TimerRuns:           p.timerRuns,
			Failures:            p.failures,
			ConsecutiveFailures: p.consecutiveFailures,
			LastStarted:         p.lastStarted,
			LastFinished:        p.lastFinished,
			LastSuccess:         p.lastSuccess,
			LastDuration:        p.lastDuration,
			LastError:           p.lastError,
			LastErrorAt:         p.lastErrorAt,
			Counters:            counters,
		})
	}
	return out
}
