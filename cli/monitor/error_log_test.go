package monitor

import (
	"fmt"
	"testing"
)

func TestErrorLogCapsAtCapacity(t *testing.T) {
	l := NewErrorLog(5)
	for i := range 8 {
		l.Add("error %d", i)
	}

	entries := l.Entries()
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(entries))
	}
	// Oldest entries (0, 1, 2) should have been evicted; the most recent 5
	// (3..7) should remain, oldest first.
	for i, e := range entries {
		want := fmt.Sprintf("error %d", i+3)
		if e.Message != want {
			t.Errorf("entries[%d] = %q, want %q", i, e.Message, want)
		}
		if e.Timestamp.IsZero() {
			t.Errorf("entries[%d] has zero Timestamp", i)
		}
	}
}

func TestErrorLogEntriesIsACopy(t *testing.T) {
	l := NewErrorLog(10)
	l.Add("first")

	entries := l.Entries()
	entries[0].Message = "mutated"

	if got := l.Entries()[0].Message; got != "first" {
		t.Errorf("Entries() returned a live reference; got %q after mutating the copy", got)
	}
}

func TestErrorLogLatestTimestamp(t *testing.T) {
	l := NewErrorLog(5)
	if _, ok := l.LatestTimestamp(); ok {
		t.Fatal("expected no latest timestamp on an empty log")
	}
	l.Add("first")
	l.Add("second")
	got, ok := l.LatestTimestamp()
	if !ok {
		t.Fatal("expected a latest timestamp")
	}
	if want := l.Entries()[1].Timestamp; !got.Equal(want) {
		t.Errorf("LatestTimestamp() = %v, want %v", got, want)
	}
}
