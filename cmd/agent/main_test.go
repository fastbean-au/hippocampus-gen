package main

import (
	"strings"
	"testing"
	"time"
)

// TestARestartedWriterDoesNotReuseItsIds: the live writer's pass counter starts at 0 every time it
// starts, and ids built from the pass alone collided with memories the store still held from the
// previous run. On the demo, ~half the passes failed with AlreadyExists for a day after every
// restart, until the counter passed the old run's high mark. A pass's ids must be unique to the run.
func TestARestartedWriterDoesNotReuseItsIds(t *testing.T) {
	started := time.Date(2026, 10, 8, 3, 47, 8, 0, time.UTC)
	restarted := started.Add(5 * time.Second)

	first := passPrefix(runID(started), 0)
	second := passPrefix(runID(restarted), 0)

	if first == second {
		t.Errorf("pass 0 of two runs shares the prefix %q, so the second run re-uses the first's ids", first)
	}

	if passPrefix(runID(started), 0) == passPrefix(runID(started), 1) {
		t.Error("two passes of one run share a prefix")
	}

	// Lexical order follows start order, so a store's ids still read oldest-run first.
	if strings.Compare(first, second) >= 0 {
		t.Errorf("a later run's prefix %q does not sort after the earlier %q", second, first)
	}
}
