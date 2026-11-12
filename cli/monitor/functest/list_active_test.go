package functest_test

import (
	"context"
	"sort"
	"testing"

	monitor "github.com/broadinstitute/sprinkles/monitor"
)

// TestFirestoreWorkPoolStoreListActive checks, against the real emulator,
// that ListActive returns only pools whose recorded state isn't idle, and
// copes with a pool that has no summary yet and a summary whose pool is gone.
func TestFirestoreWorkPoolStoreListActive(t *testing.T) {
	startFirestoreEmulator(t)
	ctx := context.Background()
	fs := newFirestoreClient(t, ctx)
	pools := monitor.NewFirestoreWorkPoolStore(fs)

	prefix := "la-" + randomID() + "-"
	states := map[string]monitor.WorkPoolStatus{
		prefix + "ok":        monitor.WorkPoolStatusOK,
		prefix + "unhealthy": monitor.WorkPoolStatusUnhealthy,
		prefix + "halted":    monitor.WorkPoolStatusHalted,
		prefix + "idle":      monitor.WorkPoolStatusIdle,
	}
	for id, st := range states {
		writeWorkPool(t, ctx, fs, &fsWorkPoolDoc{WorkpoolID: id, Region: "us-central1"})
		if err := pools.SaveState(ctx, &monitor.WorkPoolState{WorkpoolID: id, State: st}); err != nil {
			t.Fatal(err)
		}
	}
	// A pool that has never been summarized has no state yet.
	writeWorkPool(t, ctx, fs, &fsWorkPoolDoc{WorkpoolID: prefix + "never-summarized"})
	// A summary whose WorkPool document no longer exists.
	if err := pools.SaveState(ctx, &monitor.WorkPoolState{WorkpoolID: prefix + "orphan", State: monitor.WorkPoolStatusOK}); err != nil {
		t.Fatal(err)
	}

	active, err := pools.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	got := map[string]monitor.WorkPoolStatus{}
	for _, ws := range active {
		if len(ws.Pool.WorkpoolID) >= len(prefix) && ws.Pool.WorkpoolID[:len(prefix)] == prefix {
			got[ws.Pool.WorkpoolID] = ws.State.State
		}
		if ws.Pool.Region == "" && ws.Pool.WorkpoolID != prefix+"orphan" {
			t.Errorf("pool %s came back without its WorkPool document fields", ws.Pool.WorkpoolID)
		}
	}
	want := map[string]monitor.WorkPoolStatus{
		prefix + "ok":        monitor.WorkPoolStatusOK,
		prefix + "unhealthy": monitor.WorkPoolStatusUnhealthy,
		prefix + "halted":    monitor.WorkPoolStatusHalted,
	}
	if len(got) != len(want) {
		keys := func(m map[string]monitor.WorkPoolStatus) []string {
			var k []string
			for id := range m {
				k = append(k, id)
			}
			sort.Strings(k)
			return k
		}
		t.Fatalf("ListActive returned %v, want %v", keys(got), keys(want))
	}
	for id, st := range want {
		if got[id] != st {
			t.Errorf("%s: state %q, want %q", id, got[id], st)
		}
	}
}
