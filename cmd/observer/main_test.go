package main

import (
	"strings"
	"testing"
)

// TestBoundedSourcesFitsTheMetadataCap covers a failure seen in production: the observer generated
// an observation, paid for the model call, and then had the whole write refused because the joined
// source ids exceeded the contract's 512-byte metadata value cap. Losing a note to a provenance
// field is the wrong trade.
func TestBoundedSourcesFitsTheMetadataCap(t *testing.T) {
	long := make([]string, 40)

	for i := range long {
		long[i] = strings.Repeat("a", 40)
	}

	got := boundedSources(long)

	if len(got) > maxMetadataValueBytes {
		t.Errorf("joined sources are %d bytes, over the %d cap", len(got), maxMetadataValueBytes)
	}

	if got == "" {
		t.Error("expected at least some sources to fit")
	}

	// Whole ids only - a truncated id traces back to nothing.
	for _, id := range strings.Fields(got) {
		if len(id) != 40 {
			t.Errorf("id %q was truncated", id)
		}
	}
}

func TestBoundedSourcesHandlesEmptyAndSmallInputs(t *testing.T) {
	if got := boundedSources(nil); got != "" {
		t.Errorf("nil sources should join to empty, got %q", got)
	}

	if got := boundedSources([]string{"a", "b"}); got != "a b" {
		t.Errorf("short lists should pass through, got %q", got)
	}
}
