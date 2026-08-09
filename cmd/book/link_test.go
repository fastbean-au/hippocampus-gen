package main

import (
	"testing"

	"github.com/fastbean-au/hippocampus-gen/internal/link"
)

// TestCharacterLinksCollapsesSharedHead covers the case the novel produces constantly: two characters
// who appear in the same paragraph, so both of their threads have that paragraph as their head, and
// the next paragraph naming them both would otherwise declare two links to it. The service rejects a
// write naming the same target twice, so the paragraph must declare one link, at the stronger of the
// two weights.
func TestCharacterLinksCollapsesSharedHead(t *testing.T) {
	threads := newCharacterThreads()

	cast := mentioned("Joe and Biddy were at the forge, and Biddy looked at Joe.")

	if len(cast) != 2 {
		t.Fatalf("expected Joe and Biddy to be the cast, got %d", len(cast))
	}

	advanceCharacters(threads, cast, "m-1", 1)

	links := characterLinks(threads, cast, 2)

	if len(links) != 1 {
		t.Fatalf("expected the two threads on m-1 to collapse to one link, got %d", len(links))
	}

	if links[0].GetId() != "m-1" {
		t.Fatalf("expected the link to point at m-1, got %q", links[0].GetId())
	}

	// Joe outranks Biddy in the cast table, so his weight is the one the surviving link carries.
	if links[0].GetSignificance() != 20000 {
		t.Fatalf("expected the stronger significance to survive, got %d", links[0].GetSignificance())
	}
}

// TestCharacterLinksKeepsDistinctHeads guards the collapse against over-reaching: threads whose heads
// are different paragraphs are separate associations and must stay separate links.
func TestCharacterLinksKeepsDistinctHeads(t *testing.T) {
	threads := newCharacterThreads()

	advanceCharacters(threads, mentioned("Joe at the forge."), "m-1", 1)
	advanceCharacters(threads, mentioned("Biddy at the school."), "m-2", 2)

	links := characterLinks(threads, mentioned("Joe and Biddy talked."), 3)

	if len(links) != 2 {
		t.Fatalf("expected one link per distinct head, got %d", len(links))
	}

	seen := map[string]bool{}

	for _, l := range links {
		seen[l.GetId()] = true
	}

	for _, id := range []string{"m-1", "m-2"} {
		if !seen[id] {
			t.Fatalf("expected a link to %q", id)
		}
	}
}

// TestCharacterLinksFirstMentionHasNothingToPointAt is the base case: a cast met for the first time
// leaves the paragraph unlinked rather than linking it to an empty id.
func TestCharacterLinksFirstMentionHasNothingToPointAt(t *testing.T) {
	threads := link.NewThreads(0, maxCharacterThreads)

	if links := characterLinks(threads, mentioned("Estella crossed the yard."), 1); len(links) != 0 {
		t.Fatalf("expected no links for a first mention, got %d", len(links))
	}
}
