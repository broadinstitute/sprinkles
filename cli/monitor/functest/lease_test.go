package functest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	monitor "github.com/broadinstitute/sprinkles/monitor"
)

// TestFirestoreLeaseStore exercises the lease transactions against the real
// emulator: mutual exclusion, expiry takeover, renewal, loss detection and
// release.
func TestFirestoreLeaseStore(t *testing.T) {
	startFirestoreEmulator(t)
	ctx := context.Background()
	fs := newFirestoreClient(t, ctx)
	store := monitor.NewFirestoreLeaseStore(fs)

	const ttl = time.Minute
	now := time.Now()
	a := monitor.LeaseClaim{HolderID: "a-" + randomID(), Description: "host-a pid 1"}
	b := monitor.LeaseClaim{HolderID: "b-" + randomID(), Description: "host-b pid 2"}

	// Start from a clean slate: the document is shared by every test run.
	_ = store.Release(ctx, a.HolderID)

	if err := store.TryAcquire(ctx, a, now, ttl); err != nil {
		t.Fatalf("a acquires a free lease: %v", err)
	}

	// b is locked out while a's lease is live, and told who holds it.
	var held *monitor.LeaseHeldError
	if err := store.TryAcquire(ctx, b, now.Add(10*time.Second), ttl); !errors.As(err, &held) {
		t.Fatalf("b should be refused with LeaseHeldError, got %v", err)
	} else if held.HolderID != a.HolderID {
		t.Errorf("held by %q, want %q", held.HolderID, a.HolderID)
	}

	// a can re-acquire its own lease (e.g. after a restart with the same id).
	if err := store.TryAcquire(ctx, a, now.Add(10*time.Second), ttl); err != nil {
		t.Errorf("a re-acquires its own lease: %v", err)
	}

	// Renewing pushes the expiry out, so b is still locked out after the
	// original expiry time.
	if err := store.Renew(ctx, a.HolderID, now.Add(50*time.Second), ttl); err != nil {
		t.Fatalf("a renews: %v", err)
	}
	if err := store.TryAcquire(ctx, b, now.Add(90*time.Second), ttl); !errors.As(err, &held) {
		t.Errorf("b should still be refused after a renewed, got %v", err)
	}

	// Once the lease expires b can take over, and a learns it has lost it.
	if err := store.TryAcquire(ctx, b, now.Add(3*time.Minute), ttl); err != nil {
		t.Fatalf("b takes over an expired lease: %v", err)
	}
	if err := store.Renew(ctx, a.HolderID, now.Add(3*time.Minute), ttl); !errors.Is(err, monitor.ErrLeaseLost) {
		t.Errorf("a's renewal should report ErrLeaseLost, got %v", err)
	}

	// A holder that isn't current can't release someone else's lease...
	if err := store.Release(ctx, a.HolderID); err != nil {
		t.Fatalf("release by non-holder: %v", err)
	}
	if err := store.TryAcquire(ctx, a, now.Add(3*time.Minute+time.Second), ttl); !errors.As(err, &held) {
		t.Errorf("a's release must not have freed b's lease, got %v", err)
	}
	// ...but the holder can, and then anyone may acquire immediately.
	if err := store.Release(ctx, b.HolderID); err != nil {
		t.Fatalf("release by holder: %v", err)
	}
	if err := store.TryAcquire(ctx, a, now.Add(3*time.Minute+2*time.Second), ttl); err != nil {
		t.Errorf("a acquires right after b released: %v", err)
	}

	// Renewing when the document is gone reports loss too.
	if err := store.Release(ctx, a.HolderID); err != nil {
		t.Fatal(err)
	}
	if err := store.Renew(ctx, a.HolderID, now, ttl); !errors.Is(err, monitor.ErrLeaseLost) {
		t.Errorf("renew with no lease document should report ErrLeaseLost, got %v", err)
	}
}
