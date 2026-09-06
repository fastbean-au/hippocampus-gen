package trace

import (
	"regexp"
	"testing"
)

// singleToken is what both search backends agree is one term: FTS5's unicode61 and OpenSearch's
// standard analyser each split on everything else, hyphens and underscores included.
var singleToken = regexp.MustCompile(`^[a-z0-9]+$`)

// TestVocabularyIsSingleTokens guards the constraint that cannot be seen by reading the list. A
// hyphen in one term does not fail anything loudly - it just splits into two common tokens, and the
// vocabulary quietly stops being as discriminating as Config.MemoriesPerTerm claims.
func TestVocabularyIsSingleTokens(t *testing.T) {
	for _, a := range workAreas {
		for _, v := range a.Terms {
			if !singleToken.MatchString(v) {
				t.Errorf("area %s: %q is not a single alphanumeric token", a.Name, v)
			}
		}
	}
}

// TestVocabularyIsGloballyUnique guards the same property from the other side: a term carried by
// two areas is carried by twice the memories, which halves its discriminating power without
// changing the vocabulary's apparent size.
func TestVocabularyIsGloballyUnique(t *testing.T) {
	seen := make(map[string]string, len(workAreas)*40)

	for _, a := range workAreas {
		for _, v := range a.Terms {
			if where, dup := seen[v]; dup {
				t.Errorf("duplicate term %q: in both %s and %s", v, where, a.Name)

				continue
			}

			seen[v] = a.Name
		}
	}
}

// TestVocabularyServesTheDefaultConfig checks the list is big enough for the vocabulary the agent
// defaults ask for - 20,000 memories at 4 terms each and 100 memories per term wants 800 distinct
// terms - so the generator never has to reuse one at the configuration people actually run.
func TestVocabularyServesTheDefaultConfig(t *testing.T) {
	const (
		memories        = 20000
		termsPerMemory  = 4
		memoriesPerTerm = 100
	)

	want := memories * termsPerMemory / memoriesPerTerm
	got := 0

	for _, a := range workAreas {
		got += len(a.Terms)
	}

	if got < want {
		t.Errorf("vocabulary serves %d terms, but the default config wants %d", got, want)
	}
}

// TestVocabularyAreasAreEvenlySized keeps the areas balanced. Terms are drawn from a memory's own
// area, so a thin area would make its memories' terms markedly less discriminating than the rest -
// a difference that would show up as unexplained variance in retrieval rather than as a failure.
func TestVocabularyAreasAreEvenlySized(t *testing.T) {
	const want = 40

	for _, a := range workAreas {
		if len(a.Terms) != want {
			t.Errorf("area %s carries %d terms, wanted %d", a.Name, len(a.Terms), want)
		}
	}
}

// TestVocabularyAreaNamesAreUsablePaths checks the area names can serve as directory segments in
// the paths generated memories name, which is the second job the area name does.
func TestVocabularyAreaNamesAreUsablePaths(t *testing.T) {
	seen := make(map[string]bool, len(workAreas))

	for _, a := range workAreas {
		if !singleToken.MatchString(a.Name) {
			t.Errorf("area name %q is not usable as a path segment", a.Name)
		}

		if seen[a.Name] {
			t.Errorf("duplicate area name %q", a.Name)
		}

		seen[a.Name] = true
	}
}
