package trace

import (
	"strings"
	"testing"

	"github.com/fastbean-au/hippocampus-gen/internal/params"
)

// sample is the configuration these tests generate from. Small, so the tests stay quick.
func sample(t *testing.T, vocabulary Vocabulary) *Trace {
	t.Helper()

	p, err := params.Default()
	if err != nil {
		t.Fatalf("loading the committed parameters: %s", err.Error())
	}

	tr, err := Generate(Config{
		Params: p, Seed: 1, Memories: 200, Days: 30, Agents: 4,
		SignificanceSignal: SignalImportance, ImportanceShape: ShapeMeasured,
		SignificanceScale: ScaleLog, MustKeepShare: 0.05, SignificanceNoise: 0.3,
		LinkScale: 0.15, RetrievalHorizonDays: 30, TermsPerMemory: 4,
		MemoriesPerTerm: 100, QueryTerms: 3, MinSignificance: 1000,
		MaxSignificance: 30000, BodyBytes: 256, Vocabulary: vocabulary,
	})
	if err != nil {
		t.Fatalf("generating the trace: %s", err.Error())
	}

	return tr
}

// TestSyntheticVocabularyIsUnchanged pins the synthetic renderer's output.
//
// This is the test that matters most in this file. The published benchmark numbers were produced by
// the synthetic renderer, and a run has to stay reproducible from its seed for them to mean
// anything - so adding a second renderer must not perturb the first by so much as one draw from the
// random source. These values were taken from the generator before the realistic renderer existed.
func TestSyntheticVocabularyIsUnchanged(t *testing.T) {
	tr := sample(t, VocabSynthetic)
	m := tr.Memories[0]

	if got, want := strings.Join(m.Terms, " "), "quon tor sel dal"; got != want {
		t.Errorf("first memory's terms are %q, wanted %q - the synthetic trace has moved", got, want)
	}

	if got, want := m.Significance, int32(22646); got != want {
		t.Errorf("first memory's significance is %d, wanted %d - the random source has been perturbed", got, want)
	}

	if want := "note mi0 quon tor sel dal the and for with"; !strings.HasPrefix(m.Body, want) {
		t.Errorf("first memory's body is %q, wanted the prefix %q", m.Body, want)
	}
}

// TestEmptyVocabularyIsSynthetic checks the zero value keeps the old behaviour, since every caller
// that predates the field leaves it unset.
func TestEmptyVocabularyIsSynthetic(t *testing.T) {
	if got, want := sample(t, "").Memories[0].Body, sample(t, VocabSynthetic).Memories[0].Body; got != want {
		t.Errorf("an empty vocabulary rendered %q, wanted the synthetic %q", got, want)
	}
}

// TestRealisticTermsMatchTheirArea is the coherence property the whole design turns on: a note about
// a file in one area must not be discussing another area's vocabulary.
func TestRealisticTermsMatchTheirArea(t *testing.T) {
	tr := sample(t, VocabRealistic)

	for i, v := range tr.Memories {
		a := areaFor(v.Entity)
		in := make(map[string]bool, len(a.Terms))

		for _, term := range a.Terms {
			in[term] = true
		}

		for _, term := range v.Terms {
			if !in[term] {
				t.Fatalf("memory %d is filed under %s but carries the term %q, which belongs to another area",
					i, a.Name, term)
			}
		}

		if !strings.Contains(v.Body, "/"+a.Name+"/") {
			t.Fatalf("memory %d carries %s terms but names the path in %q", i, a.Name, v.Body)
		}
	}
}

// TestRealisticBodiesLeadWithTheirTerms checks a truncated body still carries what a query matches
// on - the same property the synthetic renderer has, and the reason the finding comes first.
func TestRealisticBodiesLeadWithTheirTerms(t *testing.T) {
	tr := sample(t, VocabRealistic)

	for i, v := range tr.Memories {
		head := v.Body

		if len(head) > 160 {
			head = head[:160]
		}

		if !strings.Contains(head, v.Terms[0]) {
			t.Errorf("memory %d does not carry its first term %q in the opening of %q", i, v.Terms[0], head)
		}
	}
}

// TestRealisticBodiesReachTheTargetSize checks the padding still does its job.
func TestRealisticBodiesReachTheTargetSize(t *testing.T) {
	tr := sample(t, VocabRealistic)

	for i, v := range tr.Memories {
		if len(v.Body) < tr.Config.BodyBytes {
			t.Errorf("memory %d has a %d byte body, short of the %d byte target",
				i, len(v.Body), tr.Config.BodyBytes)
		}
	}
}

// TestRealisticNeighboursDiffer guards the flaw hashing was introduced to fix: indexing the word
// lists by consecutive entity indices made neighbouring memories near-identical.
func TestRealisticNeighboursDiffer(t *testing.T) {
	tr := sample(t, VocabRealistic)
	paths := 0

	for i := 1; i < len(tr.Memories); i++ {
		if pathFor(tr.Memories[i].Entity) == pathFor(tr.Memories[i-1].Entity) {
			paths++
		}
	}

	if paths > len(tr.Memories)/20 {
		t.Errorf("%d of %d consecutive memories name the same path - the scatter has regressed",
			paths, len(tr.Memories))
	}
}

// TestRealisticIsDeterministic is the property every generated trace must have: the same seed and
// config produce the same trace, text included.
func TestRealisticIsDeterministic(t *testing.T) {
	first, second := sample(t, VocabRealistic), sample(t, VocabRealistic)

	for i := range first.Memories {
		if first.Memories[i].Body != second.Memories[i].Body {
			t.Fatalf("memory %d differs between two generations of the same seed:\n  %q\n  %q",
				i, first.Memories[i].Body, second.Memories[i].Body)
		}
	}

	for i := range first.Sessions {
		if first.Sessions[i].Name != second.Sessions[i].Name {
			t.Fatalf("session %d's name differs between two generations of the same seed", i)
		}
	}
}

// TestSessionNaming checks events are titled under the realistic vocabulary and left to the replay's
// fallback under the synthetic one.
func TestSessionNaming(t *testing.T) {
	for _, v := range sample(t, VocabRealistic).Sessions {
		if v.Name == "" {
			t.Fatalf("session %s has no name under the realistic vocabulary", v.ID)
		}

		if !strings.HasPrefix(v.Name, v.Group) {
			t.Errorf("session %s is named %q, which does not name its group", v.ID, v.Name)
		}
	}

	for _, v := range sample(t, VocabSynthetic).Sessions {
		if v.Name != "" {
			t.Fatalf("session %s is named %q under the synthetic vocabulary, where the replay expects to fall back",
				v.ID, v.Name)
		}
	}
}

// TestQueriesAreReadable checks the held-out questions come out as real words, which is the whole
// point of the exercise.
func TestQueriesAreReadable(t *testing.T) {
	tr := sample(t, VocabRealistic)

	if len(tr.Retrievals) == 0 {
		t.Fatal("the trace asks no questions")
	}

	for _, v := range tr.Retrievals {
		for _, term := range strings.Fields(v.Query) {
			if !singleToken.MatchString(term) {
				t.Errorf("query %q contains %q, which is not a single searchable token", v.Query, term)
			}
		}
	}
}

// TestEffectiveMemoriesPerTerm records the arithmetic the vocabulary was sized by: at the agent
// defaults the realistic list lands exactly on --memories-per-term's own default.
func TestEffectiveMemoriesPerTerm(t *testing.T) {
	if got, want := effectiveMemoriesPerTerm(20000, 4), 100; got != want {
		t.Errorf("the realistic vocabulary yields %d memories per term at the defaults, wanted %d", got, want)
	}
}

// TestVocabularyValidation checks the two ways a caller can ask for something the renderer cannot
// give are refused rather than silently ignored.
func TestVocabularyValidation(t *testing.T) {
	p, err := params.Default()
	if err != nil {
		t.Fatalf("loading the committed parameters: %s", err.Error())
	}

	base := Config{
		Params: p, Seed: 1, Memories: 200, Days: 30, Agents: 4,
		SignificanceSignal: SignalImportance, ImportanceShape: ShapeMeasured,
		SignificanceScale: ScaleLog, MustKeepShare: 0.05, SignificanceNoise: 0.3,
		LinkScale: 0.15, RetrievalHorizonDays: 30, TermsPerMemory: 4,
		MemoriesPerTerm: 100, QueryTerms: 3, MinSignificance: 1000,
		MaxSignificance: 30000, BodyBytes: 256,
	}

	unknown := base
	unknown.Vocabulary = "prose"

	if _, err := Generate(unknown); err == nil {
		t.Error("an unknown vocabulary was accepted")
	}

	wide := base
	wide.Vocabulary = VocabRealistic
	wide.TermsPerMemory = termsPerArea + 1
	wide.QueryTerms = 3

	if _, err := Generate(wide); err == nil {
		t.Errorf("terms per memory above the %d an area carries was accepted", termsPerArea)
	}
}
