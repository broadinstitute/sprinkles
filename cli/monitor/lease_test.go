package monitor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/broadinstitute/sprinkles/scheduler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLeaseStore implements LeaseStore's documented semantics in memory.
type fakeLeaseStore struct {
	mu        sync.Mutex
	exists    bool
	holder    LeaseClaim
	expiresAt time.Time
	renewErr  error         // returned by Renew while set (not ErrLeaseLost)
	renewed   chan struct{} // receives one value per Renew call
}

func newFakeLeaseStore() *fakeLeaseStore {
	return &fakeLeaseStore{renewed: make(chan struct{}, 100)}
}

// give sets the lease to another holder directly.
func (f *fakeLeaseStore) give(id string, expiresAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exists, f.holder, f.expiresAt = true, LeaseClaim{HolderID: id, Description: "other host"}, expiresAt
}

func (f *fakeLeaseStore) TryAcquire(_ context.Context, c LeaseClaim, now time.Time, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists && f.holder.HolderID != c.HolderID && f.expiresAt.After(now) {
		return &LeaseHeldError{HolderID: f.holder.HolderID, Description: f.holder.Description, ExpiresAt: f.expiresAt}
	}
	f.exists, f.holder, f.expiresAt = true, c, now.Add(ttl)
	return nil
}

func (f *fakeLeaseStore) Renew(_ context.Context, id string, now time.Time, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer func() { f.renewed <- struct{}{} }()
	if f.renewErr != nil {
		return f.renewErr
	}
	if !f.exists || f.holder.HolderID != id {
		return fmt.Errorf("%w: taken", ErrLeaseLost)
	}
	f.expiresAt = now.Add(ttl)
	return nil
}

func (f *fakeLeaseStore) Release(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists && f.holder.HolderID == id {
		f.exists = false
	}
	return nil
}

func (f *fakeLeaseStore) holderID() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.holder.HolderID, f.exists
}

// signalClock tells the test each time something starts waiting on it, so a
// test can advance the clock only once the code under test is parked there.
type signalClock struct {
	*scheduler.FakeClock
	waiting chan struct{}
}

func (c *signalClock) After(d time.Duration) <-chan time.Time {
	ch := c.FakeClock.After(d)
	select {
	case c.waiting <- struct{}{}:
	default:
	}
	return ch
}

func leaseWorld(store LeaseStore) (*World, *signalClock) {
	w := newWorld()
	sc := &signalClock{FakeClock: w.Clock, waiting: make(chan struct{}, 100)}
	w.A.clock = sc
	w.A.SetLease(store, 60*time.Second)
	return w, sc
}

func TestAcquireLease_FreeLease(t *testing.T) {
	store := newFakeLeaseStore()
	w, _ := leaseWorld(store)
	require.NoError(t, w.A.AcquireLease(context.Background()))
	assert.Empty(t, w.A.ErrorLog().Entries(), "nothing to report when the lease is simply free")
	id, held := store.holderID()
	assert.True(t, held)
	assert.Equal(t, w.A.lease.claim.HolderID, id)
}

func TestAcquireLease_NoLeaseConfiguredIsNoOp(t *testing.T) {
	w := newWorld()
	require.NoError(t, w.A.AcquireLease(context.Background()))
	w.A.ReleaseLease(context.Background())
}

// advanceWhileWaiting runs fn and keeps advancing the clock each time it parks
// on a wait, until fn returns.
func advanceWhileWaiting(t *testing.T, sc *signalClock, step time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	for {
		select {
		case <-done:
			return
		case <-sc.waiting:
			sc.Advance(step)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out")
		}
	}
}

// A crashed holder's lease expires, so its replacement waits and then takes it.
func TestAcquireLease_WaitsOutAnExpiredHolder(t *testing.T) {
	store := newFakeLeaseStore()
	w, sc := leaseWorld(store)
	store.give("crashed", w.Clock.Now().Add(30*time.Second))

	var err error
	advanceWhileWaiting(t, sc, w.A.lease.retryInterval, func() { err = w.A.AcquireLease(context.Background()) })

	require.NoError(t, err)
	id, _ := store.holderID()
	assert.Equal(t, w.A.lease.claim.HolderID, id)

	// The wait was reported where the dashboard shows it, and so was the
	// recovery.
	entries := w.A.ErrorLog().Entries()
	require.Len(t, entries, 2)
	assert.Contains(t, entries[0].Message, "monitor is not running")
	assert.Contains(t, entries[0].Message, "other host")
	assert.Contains(t, entries[1].Message, "now running")
}

// A live holder keeps renewing, so we give up rather than run a second monitor.
func TestAcquireLease_RefusesWhileAnotherHolderIsAlive(t *testing.T) {
	store := newFakeLeaseStore()
	w, sc := leaseWorld(store)
	store.give("alive", w.Clock.Now().Add(24*time.Hour))

	var err error
	advanceWhileWaiting(t, sc, w.A.lease.retryInterval, func() { err = w.A.AcquireLease(context.Background()) })

	var held *LeaseHeldError
	require.ErrorAs(t, err, &held)
	assert.Equal(t, "alive", held.HolderID)
	assert.Contains(t, err.Error(), "second monitor")

	// Reported once, up front (not on every retry), naming the holder.
	entries := w.A.ErrorLog().Entries()
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Message, "monitor is not running")
	assert.Contains(t, entries[0].Message, "other host")
	assert.Contains(t, entries[0].Message, "alive")
}

func TestReleaseLease_FreesItForAReplacement(t *testing.T) {
	store := newFakeLeaseStore()
	w, _ := leaseWorld(store)
	require.NoError(t, w.A.AcquireLease(context.Background()))

	w.A.ReleaseLease(context.Background())

	_, held := store.holderID()
	assert.False(t, held)
}

// startRenewal runs the renewal loop and returns a channel carrying its fatal error.
func startRenewal(w *World, sc *signalClock, ctx context.Context) <-chan error {
	fatal := make(chan error, 1)
	go w.A.runLeaseRenewal(ctx, func(err error) { fatal <- err })
	<-sc.waiting // parked waiting for the first renewal
	return fatal
}

// step advances one renewal interval and waits for the Renew call it triggers.
func step(t *testing.T, store *fakeLeaseStore, sc *signalClock, d time.Duration) {
	t.Helper()
	sc.Advance(d)
	select {
	case <-store.renewed:
	case <-time.After(5 * time.Second):
		t.Fatal("renewal did not run")
	}
}

func TestLeaseRenewal_ExtendsTheLease(t *testing.T) {
	store := newFakeLeaseStore()
	w, sc := leaseWorld(store)
	require.NoError(t, w.A.AcquireLease(context.Background()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fatal := startRenewal(w, sc, ctx)

	step(t, store, sc, 20*time.Second)
	<-sc.waiting
	step(t, store, sc, 20*time.Second)

	store.mu.Lock()
	expires := store.expiresAt
	store.mu.Unlock()
	assert.Equal(t, w.Clock.Now().Add(60*time.Second), expires)
	select {
	case err := <-fatal:
		t.Fatalf("unexpected fatal error: %v", err)
	default:
	}
}

func TestLeaseRenewal_StopsWhenTheLeaseIsTakenOver(t *testing.T) {
	store := newFakeLeaseStore()
	w, sc := leaseWorld(store)
	require.NoError(t, w.A.AcquireLease(context.Background()))
	fatal := startRenewal(w, sc, context.Background())

	store.give("usurper", w.Clock.Now().Add(time.Hour))
	step(t, store, sc, 20*time.Second)

	select {
	case err := <-fatal:
		assert.ErrorIs(t, err, ErrLeaseLost)
	case <-time.After(5 * time.Second):
		t.Fatal("expected the monitor to stop")
	}
	assert.NotEmpty(t, w.A.ErrorLog().Entries(), "the loss should be visible in the error log")
}

// If renewals keep failing we can't prove we still hold the lease, so we stop
// before it could be taken over (ttl 60s, interval 20s: stop at 40s).
func TestLeaseRenewal_StopsWhenRenewalsKeepFailing(t *testing.T) {
	store := newFakeLeaseStore()
	w, sc := leaseWorld(store)
	require.NoError(t, w.A.AcquireLease(context.Background()))
	store.renewErr = errors.New("firestore unavailable")
	fatal := startRenewal(w, sc, context.Background())

	step(t, store, sc, 20*time.Second) // 20s since last success: keep trying
	select {
	case err := <-fatal:
		t.Fatalf("stopped too early: %v", err)
	default:
	}
	<-sc.waiting
	step(t, store, sc, 20*time.Second) // 40s: out of margin

	select {
	case err := <-fatal:
		assert.ErrorContains(t, err, "could not renew")
	case <-time.After(5 * time.Second):
		t.Fatal("expected the monitor to stop")
	}
}

func TestLeaseRenewal_RecoversFromATransientError(t *testing.T) {
	store := newFakeLeaseStore()
	w, sc := leaseWorld(store)
	require.NoError(t, w.A.AcquireLease(context.Background()))
	store.renewErr = errors.New("blip")
	fatal := startRenewal(w, sc, context.Background())

	step(t, store, sc, 20*time.Second)
	store.mu.Lock()
	store.renewErr = nil
	store.mu.Unlock()
	<-sc.waiting
	step(t, store, sc, 20*time.Second) // succeeds, resetting the failure clock
	<-sc.waiting
	store.mu.Lock()
	store.renewErr = errors.New("blip")
	store.mu.Unlock()
	step(t, store, sc, 20*time.Second) // only 20s since last success again

	select {
	case err := <-fatal:
		t.Fatalf("unexpected fatal error: %v", err)
	default:
	}
}

// emptyJobSummaries is a JobSummaryStore with no jobs, enough for the monitor
// loop's job summary poll to run.
type emptyJobSummaries struct{}

func (emptyJobSummaries) Create(context.Context, *JobSummary) error              { return nil }
func (emptyJobSummaries) Get(context.Context, string) (*JobSummary, error)       { return nil, nil }
func (emptyJobSummaries) ListNonTerminal(context.Context) ([]*JobSummary, error) { return nil, nil }
func (emptyJobSummaries) Save(context.Context, *JobSummary) error                { return nil }
func (emptyJobSummaries) SaveHistory(context.Context, *JobSummaryHistory) error  { return nil }

// End to end: losing the lease stops the monitor loop.
func TestRunMonitorLoop_ExitsWhenLeaseIsLost(t *testing.T) {
	store := newFakeLeaseStore()
	w, sc := leaseWorld(store)
	require.NoError(t, w.A.AcquireLease(context.Background()))
	w.A.SetJobSummaryStore(emptyJobSummaries{})

	done := make(chan struct{})
	go func() { defer close(done); w.A.RunMonitorLoop(context.Background()) }()

	// Wait for the loop to settle and the renewal goroutine to park on the clock.
	time.Sleep(200 * time.Millisecond)
	store.give("usurper", w.Clock.Now().Add(time.Hour))
	sc.Advance(20 * time.Second)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor loop kept running after losing the lease")
	}
}

func TestSetLease_DefaultsAreFifteenMinutesWithFiveMinuteRenewal(t *testing.T) {
	w := newWorld()
	w.A.SetLease(newFakeLeaseStore(), 0)
	assert.Equal(t, 15*time.Minute, w.A.lease.ttl)
	assert.Equal(t, 5*time.Minute, w.A.lease.ttl/3, "renewal interval")
}

// With the default 15 minute lease the normal renewal interval is long, so a
// failed renewal is retried every 30s, and we only give up once the lease is
// 10 minutes (ttl*2/3) past its last successful renewal.
func TestLeaseRenewal_DefaultTTL_RetriesFailuresSoonerAndStopsAtTenMinutes(t *testing.T) {
	store := newFakeLeaseStore()
	w, sc := leaseWorld(store)
	w.A.SetLease(store, 0)
	require.NoError(t, w.A.AcquireLease(context.Background()))
	start := w.Clock.Now()
	store.renewErr = errors.New("firestore unavailable")
	fatal := startRenewal(w, sc, context.Background())

	step(t, store, sc, 5*time.Minute) // first renewal is due after the normal interval
	var stoppedAt time.Duration
loop:
	for {
		select {
		case <-fatal:
			stoppedAt = w.Clock.Now().Sub(start)
			break loop
		case <-sc.waiting:
		case <-time.After(5 * time.Second):
			t.Fatal("renewal loop stopped waiting without a fatal error")
		}
		step(t, store, sc, 30*time.Second) // later attempts come every 30s
		if w.Clock.Now().Sub(start) > 11*time.Minute {
			t.Fatal("kept retrying past the 10 minute margin")
		}
	}
	assert.Equal(t, 10*time.Minute, stoppedAt)
	assert.Greater(t, len(w.A.ErrorLog().Entries()), 3, "each failed attempt is logged")
}
