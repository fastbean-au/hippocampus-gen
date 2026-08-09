package main

import (
	"testing"
)

// errorLevel is the ERROR entry from the level table, which is what makes a line declare both a trace
// link and an incident link.
func errorLevel(t *testing.T) level {
	t.Helper()

	for _, l := range levels {
		if l.rank != errorRank {
			continue
		}

		return l
	}

	t.Fatal("expected an ERROR level in the table")

	return level{}
}

// TestMemoryLinksCollapsesSharedHead covers an error whose request trace and whose service incident
// chain both point at the same line - the previous line on this token was this service's previous
// error. The service rejects a write naming the same target twice, so the two threads have to
// collapse into one link, at the heavier of their weights.
func TestMemoryLinksCollapsesSharedHead(t *testing.T) {
	threads := newThreads()

	lvl := errorLevel(t)

	first := line{service: "api", level: lvl, token: "tok-1", seq: 1}

	threads.advanceMemory(first, "m-1")

	links := threads.memoryLinks(line{service: "api", level: lvl, token: "tok-1", seq: 2})

	if len(links) != 1 {
		t.Fatalf("expected the trace and incident threads on m-1 to collapse to one link, got %d", len(links))
	}

	if links[0].GetId() != "m-1" {
		t.Fatalf("expected the link to point at m-1, got %q", links[0].GetId())
	}

	if links[0].GetSignificance() != incidentLinkSignificance {
		t.Fatalf("expected the heavier incident weight to survive, got %d", links[0].GetSignificance())
	}
}

// TestMemoryLinksKeepsDistinctHeads guards the collapse: a trace head and an incident head that are
// different lines are two separate associations and stay two links.
func TestMemoryLinksKeepsDistinctHeads(t *testing.T) {
	threads := newThreads()

	lvl := errorLevel(t)

	// The service's previous error came in on one request; the line about to be written follows a
	// different request whose head is a later line.
	threads.advanceMemory(line{service: "api", level: lvl, token: "tok-1", seq: 1}, "m-1")
	threads.advanceMemory(line{service: "api", level: levels[1], token: "tok-2", seq: 2}, "m-2")

	links := threads.memoryLinks(line{service: "api", level: lvl, token: "tok-2", seq: 3})

	if len(links) != 2 {
		t.Fatalf("expected a trace link and an incident link, got %d", len(links))
	}
}
