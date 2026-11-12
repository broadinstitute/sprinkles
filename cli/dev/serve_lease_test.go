package dev

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/broadinstitute/sprinkles/monitor"
	"github.com/broadinstitute/sprinkles/scheduler"
)

// heldLeaseStore always reports the lease as held by another monitor.
type heldLeaseStore struct{}

func (heldLeaseStore) TryAcquire(_ context.Context, _ monitor.LeaseClaim, now time.Time, _ time.Duration) error {
	return &monitor.LeaseHeldError{HolderID: "other-id", Description: "otherhost pid 7", ExpiresAt: now.Add(10 * time.Minute)}
}
func (heldLeaseStore) Renew(context.Context, string, time.Time, time.Duration) error { return nil }
func (heldLeaseStore) Release(context.Context, string) error                         { return nil }

// With another monitor holding the lease, runMonitorWhenLeased must not run
// the monitor loop, must say why in the error log straight away, and must keep
// waiting (rather than returning, which would take `serve` down with it) until
// told to stop.
func TestRunMonitorWhenLeased_StaysInStandbyAndSaysWhy(t *testing.T) {
	m := monitor.New(scheduler.RealClock, nil, nil, nil, nil, nil, nil, "test-db")
	m.SetLease(heldLeaseStore{}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runMonitorWhenLeased(ctx, m) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(m.ErrorLog().Entries()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the monitor never reported that it isn't running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	msg := m.ErrorLog().Entries()[0].Message
	for _, want := range []string{"monitor is not running", "otherhost pid 7", "other-id"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error log message %q should mention %q", msg, want)
		}
	}

	select {
	case <-done:
		t.Fatal("runMonitorWhenLeased returned while the lease was still held")
	case <-time.After(200 * time.Millisecond):
	}
	if n := len(m.PollerStats().Snapshot().Pollers); n != 0 {
		t.Errorf("the monitor loop must not have started, but %d pollers are registered", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runMonitorWhenLeased didn't stop when its context was cancelled")
	}
}
