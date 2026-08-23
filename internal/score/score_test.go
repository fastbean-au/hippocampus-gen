package score

import (
	"math"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus-gen/internal/trace"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// handTrace builds a small trace with every quantity chosen, so a policy's answer is knowable by
// inspection rather than by re-implementing the policy in the test.
//
// Six memories. Significance descends with the index; recall counts ascend; creation times ascend;
// last-touch is deliberately NOT the same order as creation, so LRU and FIFO must disagree.
func handTrace() *trace.Trace {
	tr := &trace.Trace{
		Start: epoch,
		End:   epoch.Add(24 * time.Hour),
	}

	for i := 0; i < 6; i++ {
		tr.Memories = append(tr.Memories, trace.Memory{
			ID:           string(rune('a' + i)),
			Significance: int32(600 - 100*i),
			Recalls:      i,
			At:           epoch.Add(time.Duration(i) * time.Hour),
		})

		tr.Ops = append(tr.Ops, trace.Op{At: tr.Memories[i].At, Kind: trace.OpStore, Memory: i})
	}

	// Memory 0 - the oldest, most significant, never recalled - is touched last of all, so LRU keeps
	// it and FIFO does not.
	tr.Ops = append(tr.Ops, trace.Op{At: epoch.Add(20 * time.Hour), Kind: trace.OpRecall, Memory: 0})

	return tr
}

func TestBaselinesRetainExactlyTheBudget(t *testing.T) {
	tr := handTrace()

	for _, p := range Baselines(Inputs{Seed: 1}) {
		got := p.Retain(tr, 3)

		if p.Name == "keep-everything" {
			if len(got) != len(tr.Memories) {
				t.Errorf("%s kept %d, want all %d", p.Name, len(got), len(tr.Memories))
			}

			continue
		}

		if len(got) != 3 {
			t.Errorf("%s kept %d, want the budget of 3", p.Name, len(got))
		}
	}
}

// TestEachBaselineOrdersOnItsOwnQuantity is what stops the suite silently collapsing into one
// policy under a different name, which would make every comparison meaningless.
func TestEachBaselineOrdersOnItsOwnQuantity(t *testing.T) {
	tr := handTrace()

	byName := map[string]Policy{}

	for _, v := range Baselines(Inputs{Seed: 1}) {
		byName[v.Name] = v
	}

	cases := []struct {
		policy string
		want   []int
	}{
		// Memory 0 was recalled at hour 20, later than anything else was created.
		{"lru", []int{0, 5, 4}},

		// Creation order alone, so the newest three.
		{"fifo", []int{5, 4, 3}},

		// Recall counts ascend with the index.
		{"lfu", []int{5, 4, 3}},

		// Significance descends with the index.
		{"significance", []int{0, 1, 2}},
	}

	for _, v := range cases {
		got := byName[v.policy].Retain(tr, 3)

		for _, want := range v.want {
			if !got[want] {
				t.Errorf("%s: expected to keep memory %d, kept %v", v.policy, want, keys(got))
			}
		}
	}
}

func keys(in map[int]bool) []int {
	out := []int{}

	for k := range in {
		out = append(out, k)
	}

	return out
}

func TestBaselinesAreDeterministic(t *testing.T) {
	tr := handTrace()

	for _, p := range Baselines(Inputs{Seed: 1}) {
		a, b := p.Retain(tr, 3), p.Retain(tr, 3)

		if len(a) != len(b) {
			t.Fatalf("%s: different sizes across calls", p.Name)
		}

		for k := range a {
			if !b[k] {
				t.Fatalf("%s: memory %d kept in one call and not the other", p.Name, k)
			}
		}
	}
}

// TestRandomScoresItsOwnKeptShare is the check that the harness is not rigged. Random retention has
// to come out at the fraction of the store it kept; if it does not, the scoring is wrong and every
// other number in the run is suspect.
func TestRandomScoresItsOwnKeptShare(t *testing.T) {
	tr := &trace.Trace{Start: epoch, End: epoch.Add(24 * time.Hour)}

	const memories = 4000

	for i := 0; i < memories; i++ {
		tr.Memories = append(tr.Memories, trace.Memory{At: epoch})
	}

	// Every fifth memory is asked about.
	for i := 0; i < memories; i += 5 {
		tr.Retrievals = append(tr.Retrievals, trace.Retrieval{Needle: i})
	}

	var random Policy

	for _, v := range Baselines(Inputs{Seed: 42}) {
		if v.Name == "random" {
			random = v
		}
	}

	budget := memories / 4
	retained := random.Retain(tr, budget)

	got := Score(tr, []Arm{{Name: "random", Retained: retained}}, nil, 10)[0]

	if math.Abs(got.Retention-0.25) > 0.03 {
		t.Errorf("random kept %.2f of the store but retained %.2f of the needles - scoring is wrong",
			got.KeptShare, got.Retention)
	}

	if math.Abs(got.KeptShare-0.25) > 0.001 {
		t.Errorf("kept share %.3f, want 0.25", got.KeptShare)
	}
}

func TestKeepEverythingIsTheCeiling(t *testing.T) {
	tr := handTrace()
	tr.Retrievals = []trace.Retrieval{{Needle: 0}, {Needle: 3}, {Needle: 5}}

	oracle := [][]int{{0, 1}, {3, 2}, {5, 4}}

	var everything Policy

	for _, v := range Baselines(Inputs{Seed: 1}) {
		if v.Name == "keep-everything" {
			everything = v
		}
	}

	got := Score(tr, []Arm{{Name: "all", Retained: everything.Retain(tr, 0)}}, oracle, 5)[0]

	if got.Retention != 1 {
		t.Errorf("retention %.2f, want 1", got.Retention)
	}

	if got.Retrieval != 1 {
		t.Errorf("retrieval %.2f, want 1 - every needle is ranked first for its own query", got.Retrieval)
	}
}

// TestForgettingHigherRankedCandidatesPromotesTheNeedle is the subtlety in inTopK. A policy that
// forgot the rows ranked above the needle does not thereby lose it: in that policy's own store those
// rows are simply absent, and the needle sits higher. Truncating the oracle's ranking instead of
// filtering it would have understated every bounded arm.
func TestForgettingHigherRankedCandidatesPromotesTheNeedle(t *testing.T) {
	tr := &trace.Trace{Start: epoch, End: epoch.Add(time.Hour)}

	for i := 0; i < 5; i++ {
		tr.Memories = append(tr.Memories, trace.Memory{At: epoch})
	}

	tr.Retrievals = []trace.Retrieval{{Needle: 4}}

	// The needle ranks last of five.
	oracle := [][]int{{0, 1, 2, 3, 4}}

	// At k=2 an arm holding everything cannot see it.
	full := Score(tr, []Arm{{Name: "all", Retained: map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true}}}, oracle, 2)[0]

	if full.Retrieval != 0 {
		t.Errorf("a store holding all five should not find a fifth-ranked needle at k=2, got %.2f", full.Retrieval)
	}

	// An arm that forgot the three above it finds it first.
	pruned := Score(tr, []Arm{{Name: "pruned", Retained: map[int]bool{0: true, 4: true}}}, oracle, 2)[0]

	if pruned.Retrieval != 1 {
		t.Errorf("forgetting the rows above the needle should surface it, got %.2f", pruned.Retrieval)
	}
}

// TestAMeasuredArmUsesItsOwnRanking pins the asymmetry the package comment describes: the product is
// scored on what its own instance actually returned, not on a filtered approximation.
func TestAMeasuredArmUsesItsOwnRanking(t *testing.T) {
	tr := &trace.Trace{Start: epoch, End: epoch.Add(time.Hour)}

	for i := 0; i < 3; i++ {
		tr.Memories = append(tr.Memories, trace.Memory{At: epoch})
	}

	tr.Retrievals = []trace.Retrieval{{Needle: 2}}

	retained := map[int]bool{0: true, 1: true, 2: true}

	// The oracle never surfaces the needle; the arm's own search did.
	oracle := [][]int{{0, 1}}
	own := [][]int{{2, 0, 1}}

	simulated := Score(tr, []Arm{{Name: "sim", Retained: retained}}, oracle, 1)[0]
	measured := Score(tr, []Arm{{Name: "measured", Retained: retained, Ranked: own}}, oracle, 1)[0]

	if simulated.Retrieval != 0 {
		t.Errorf("the simulated arm should follow the oracle, got %.2f", simulated.Retrieval)
	}

	if measured.Retrieval != 1 {
		t.Errorf("the measured arm should follow its own ranking, got %.2f", measured.Retrieval)
	}

	// Retention is unaffected by ranking - both hold the needle.
	if simulated.Retention != 1 || measured.Retention != 1 {
		t.Error("retention should not depend on ranking at all")
	}
}

func TestRetrievalNeverExceedsRetention(t *testing.T) {
	tr := handTrace()
	tr.Retrievals = []trace.Retrieval{{Needle: 0}, {Needle: 1}, {Needle: 2}, {Needle: 3}}

	oracle := [][]int{{0}, {1}, {2}, {3}}

	for _, p := range Baselines(Inputs{Seed: 3}) {
		arm := Arm{Name: p.Name, Retained: p.Retain(tr, 2)}

		got := Score(tr, []Arm{arm}, oracle, 3)[0]

		// Finding something requires holding it, so this ordering is a structural invariant rather
		// than an empirical one.
		if got.Retrieval > got.Retention {
			t.Errorf("%s: retrieval %.2f exceeds retention %.2f", p.Name, got.Retrieval, got.Retention)
		}
	}
}

// TestScoringSurvivesRetrievalsWithNoKind guards a crash rather than a number. A hand-built trace -
// or one generated before question kinds existed - carries retrievals with no kind, and an earlier
// version of the kind lookup answered 0 for "not found", which indexed an empty slice and panicked.
// Scoring must never be the thing that fails.
func TestScoringSurvivesRetrievalsWithNoKind(t *testing.T) {
	tr := handTrace()
	tr.Retrievals = []trace.Retrieval{{Needle: 0}, {Needle: 3}}

	got := Score(tr, []Arm{{Name: "all", Retained: map[int]bool{0: true, 3: true}}}, [][]int{{0}, {3}}, 5)

	if len(got) != 1 || got[0].Retention != 1 {
		t.Fatalf("expected a clean score, got %+v", got)
	}
}

// TestQuestionKindsAreScoredApart pins the split the benchmark turns on: a policy good at one kind
// and hopeless at the other must not be reported as merely average.
func TestQuestionKindsAreScoredApart(t *testing.T) {
	tr := handTrace()
	tr.Retrievals = []trace.Retrieval{
		{Needle: 0, Kind: trace.KindNextTouch},
		{Needle: 1, Kind: trace.KindNextTouch},
		{Needle: 4, Kind: trace.KindMustKeep},
		{Needle: 5, Kind: trace.KindMustKeep},
	}

	// An arm holding both next-touch answers and neither must-keep answer.
	got := Score(tr, []Arm{{Name: "recency", Retained: map[int]bool{0: true, 1: true}}}, nil, 5)[0]

	if len(got.ByKind) != 2 {
		t.Fatalf("expected two kinds, got %d", len(got.ByKind))
	}

	byKind := map[string]float64{}

	for _, v := range got.ByKind {
		byKind[v.Kind] = v.Retention
	}

	if byKind[string(trace.KindNextTouch)] != 1 {
		t.Errorf("next-touch retention %.2f, want 1", byKind[string(trace.KindNextTouch)])
	}

	if byKind[string(trace.KindMustKeep)] != 0 {
		t.Errorf("must-keep retention %.2f, want 0", byKind[string(trace.KindMustKeep)])
	}

	// And the aggregate is exactly the average that hides it.
	if got.Retention != 0.5 {
		t.Errorf("aggregate retention %.2f, want 0.5 - the number that conceals the split", got.Retention)
	}
}
