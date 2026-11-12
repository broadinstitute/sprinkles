package sprinkles

import (
	"strings"
	"testing"
)

func TestOOMMessage(t *testing.T) {
	if oomMessage(nil) != "" || oomMessage(&ResourceUsage{}) != "" {
		t.Fatal("expected no message when not OOM-killed")
	}
	zero := int64(0)
	if oomMessage(&ResourceUsage{ContainerMemoryOOMKillCount: &zero}) != "" {
		t.Fatal("expected no message for zero oom_kill count")
	}
	limit, peak := int64(2<<30), int64(2<<30)
	m := oomMessage(&ResourceUsage{OOMKilled: true, ContainerMemoryLimitBytes: &limit, ContainerMemoryPeakBytes: &peak})
	if !strings.Contains(m, "main process") || !strings.Contains(m, "limit 2.00 GiB") {
		t.Fatalf("unexpected message: %q", m)
	}
	one := int64(1)
	m = oomMessage(&ResourceUsage{ContainerMemoryOOMKillCount: &one})
	if !strings.Contains(m, "1 process(es)") {
		t.Fatalf("unexpected message: %q", m)
	}
}
