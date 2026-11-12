package functest_test

import (
	"context"
	"testing"

	monitor "github.com/broadinstitute/sprinkles/monitor"
	"google.golang.org/api/iterator"
)

// TestFirestoreCallStatsAgainstEmulator confirms the gRPC interceptors fire
// for a real Firestore client and classify each kind of call.
func TestFirestoreCallStatsAgainstEmulator(t *testing.T) {
	startFirestoreEmulator(t)
	stats := monitor.NewFirestoreCallStats()
	ctx := monitor.WithFirestoreLabel(context.Background(), "stats-test")
	fs, closeConn, err := stats.NewClient(ctx, testProject, testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()
	defer fs.Close()

	const coll = "StatsTest"
	for _, id := range []string{"a", "b", "c"} {
		if _, err := fs.Collection(coll).Doc(id).Set(ctx, map[string]any{"n": 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fs.Collection(coll).Doc("a").Get(ctx); err != nil {
		t.Fatal(err)
	}
	it := fs.Collection(coll).Where("n", "==", 1).Documents(ctx)
	for {
		if _, err := it.Next(); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fs.Collection(coll).NewAggregationQuery().WithCount("c").Get(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Collection(coll).Doc("c").Delete(ctx); err != nil {
		t.Fatal(err)
	}

	got := map[string]monitor.FirestoreCallCount{}
	counts, _ := stats.Snapshot()
	for _, c := range counts {
		if c.Poller == "stats-test" && c.Collection == coll {
			got[c.Op] = c
		}
	}
	want := map[string][2]int64{ // op -> {calls, docs}
		monitor.FirestoreOpWrite:     {3, 3},
		monitor.FirestoreOpGet:       {1, 1},
		monitor.FirestoreOpQuery:     {1, 3},
		monitor.FirestoreOpAggregate: {1, 1},
		monitor.FirestoreOpDelete:    {1, 1},
	}
	for op, w := range want {
		if g := got[op]; g.Calls != w[0] || g.Docs != w[1] {
			t.Errorf("%s: got calls=%d docs=%d, want calls=%d docs=%d", op, g.Calls, g.Docs, w[0], w[1])
		}
	}
}
