// Package score turns a replayed trace into the benchmark's result: at one store size, how much of
// what the agent later needed does each retention policy still hold, and still find?
//
// # The baselines
//
// A forgetting store is a cache replacement policy, so the comparison is the one cache replacement
// has always used: the same trace, the same budget, several policies. The baselines here are the
// standard ones - FIFO, LRU, LFU, a static priority, and random as the floor - rather than a single
// invented "recency window", because a straw man would make the result worthless. LRU in particular
// is a strong baseline and is what most agent memory layers actually implement, knowingly or not.
//
// Every policy is given EXACTLY the budget the measured store ended up holding, so the comparison is
// at equal size. That budget is an outcome of the run rather than a number chosen here.
//
// # What is measured directly and what is simulated
//
// The Hippocampus arm is measured: its survivors are the ids a real instance still held, and its
// rankings come from searching that instance. The baselines cannot be measured, because those stores
// do not exist - so they are simulated over the same trace, and their rankings are taken by
// filtering the CONTROL instance's ranking (a second instance that kept everything) down to the ids
// each policy retained.
//
// That asymmetry is deliberate and it runs AGAINST the product: a baseline is scored with the full
// store's ranking quality, including the term statistics and the ranking blend of a store that
// forgot nothing. Any margin the measured arm shows over a baseline is therefore a lower bound on
// the real one.
package score

import (
	"math/rand"
	"sort"
	"time"

	"github.com/fastbean-au/hippocampus-gen/internal/trace"
)

// Policy computes which memories a retention rule would still hold, given a budget. Every baseline
// is one of these; the measured arm is not, since its survivors come from a service.
type Policy struct {
	Name string
	Why  string

	// Retain returns the retained memory indices. It is given the trace and the budget, and nothing
	// about the future - the same restriction the store operates under.
	Retain func(tr *trace.Trace, budget int) map[int]bool
}

// Inputs are what the baselines need beyond the trace itself.
type Inputs struct {
	Seed int64

	// Touched overrides the times the recency-based policies order on. Set it to the wall-clock
	// moments the replay actually issued each operation - see replay.Touched for why that, and not
	// the trace's simulated schedule, is the fair comparison. Nil falls back to the trace, which is
	// right for a hand-built trace in a test and wrong for a real run.
	Touched []time.Time
}

// lastTouched is when each memory was last stored or recalled, which LRU orders on.
func lastTouched(tr *trace.Trace, in Inputs) []time.Time {
	if len(in.Touched) == len(tr.Memories) {

		return in.Touched
	}

	out := make([]time.Time, len(tr.Memories))

	for _, v := range tr.Ops {
		if v.At.After(out[v.Memory]) {
			out[v.Memory] = v.At
		}
	}

	return out
}

// Baselines are the policies every run is compared against, in the order they are reported.
func Baselines(in Inputs) []Policy {
	return []Policy{
		{
			Name: "keep-everything",
			Why:  "the ceiling: an unbounded store, which is the thing a finite one is trying to approximate",
			Retain: func(tr *trace.Trace, _ int) map[int]bool {
				out := make(map[int]bool, len(tr.Memories))

				for i := range tr.Memories {
					out[i] = true
				}

				return out
			},
		},
		{
			Name: "lru",
			Why:  "keep the most recently touched - the strongest baseline, and what most agent memory layers implement",
			Retain: func(tr *trace.Trace, budget int) map[int]bool {

				return topBy(tr, budget, in, func(i int, last []time.Time) float64 {

					return float64(last[i].UnixNano())
				})
			},
		},
		{
			Name: "lfu",
			Why:  "keep the most often recalled, ignoring when",
			Retain: func(tr *trace.Trace, budget int) map[int]bool {

				return topBy(tr, budget, in, func(i int, _ []time.Time) float64 {

					return float64(tr.Memories[i].Recalls)
				})
			},
		},
		{
			Name: "fifo",
			Why:  "keep the most recently created, ignoring use - the naive recency window",
			Retain: func(tr *trace.Trace, budget int) map[int]bool {

				return topBy(tr, budget, in, func(i int, _ []time.Time) float64 {

					return float64(tr.Memories[i].At.UnixNano())
				})
			},
		},
		{
			Name: "significance",
			Why:  "keep the highest write-time significance - a static priority, never revised",
			Retain: func(tr *trace.Trace, budget int) map[int]bool {

				return topBy(tr, budget, in, func(i int, _ []time.Time) float64 {

					return float64(tr.Memories[i].Significance)
				})
			},
		},
		{
			Name: "blend-25",
			Why:  "a trivial control: rank-normalised recency and significance, mixed 25/75",
			Retain: func(tr *trace.Trace, budget int) map[int]bool {

				return blended(tr, budget, 0.25, in)
			},
		},
		{
			Name: "blend-50",
			Why:  "the same control, mixed evenly - if this matches the decay model, the maths is not earning its place",
			Retain: func(tr *trace.Trace, budget int) map[int]bool {

				return blended(tr, budget, 0.50, in)
			},
		},
		{
			Name: "blend-75",
			Why:  "the same control, weighted 75/25 towards recency",
			Retain: func(tr *trace.Trace, budget int) map[int]bool {

				return blended(tr, budget, 0.75, in)
			},
		},
		{
			Name: "random",
			Why:  "the floor: retention should equal the share kept, and anything that cannot beat this is worthless",
			Retain: func(tr *trace.Trace, budget int) map[int]bool {
				rng := rand.New(rand.NewSource(in.Seed))

				order := rng.Perm(len(tr.Memories))
				out := make(map[int]bool, budget)

				for i := 0; i < budget && i < len(order); i++ {
					out[order[i]] = true
				}

				return out
			},
		},
	}
}

// topBy keeps the budget highest-scoring memories. Ties break on the memory index so a policy is
// deterministic - two memories with identical significance must not be separated by map order.
func topBy(tr *trace.Trace, budget int, in Inputs, score func(i int, last []time.Time) float64) map[int]bool {
	last := lastTouched(tr, in)

	order := make([]int, len(tr.Memories))

	for i := range order {
		order[i] = i
	}

	sort.SliceStable(order, func(a int, b int) bool {
		x, y := score(order[a], last), score(order[b], last)

		if x != y {

			return x > y
		}

		return order[a] < order[b]
	})

	if budget > len(order) {
		budget = len(order)
	}

	out := make(map[int]bool, budget)

	for _, v := range order[:budget] {
		out[v] = true
	}

	return out
}

// Arm is one thing being scored: a name, the memories it retained, and - when it was measured
// rather than simulated - its own ranked search results.
type Arm struct {
	Name string
	Why  string

	Retained map[int]bool

	// Ranked is this arm's own ranking per retrieval, best first, as memory indices. Nil means the
	// arm is simulated and the oracle's ranking is filtered to Retained instead.
	Ranked [][]int
}

// Lag buckets split the questions by how long before the window closed the needle was last touched.
//
// This decomposition is the point of the whole exercise rather than a detail of presentation. The
// corpus that the trace is fitted to shows re-reference is BIMODAL - most reuse is seconds later,
// but a long tail lands days to weeks out - and an aggregate score hides which half a policy is
// winning. A recency policy is expected to be near-unbeatable on the short buckets, because "what
// was touched most recently" IS the question there. The long buckets are where a bounded store's
// policy has to earn its keep, and where a recency window sized for the burst throws away exactly
// what is wanted.
var lagBuckets = []struct {
	Name  string
	Upper time.Duration
}{
	{"under 1h", time.Hour},
	{"1h - 24h", 24 * time.Hour},
	{"1d - 7d", 7 * 24 * time.Hour},
	{"over 7d", 1<<62 - 1},
}

// Bucket is one lag band's score for one arm.
type Bucket struct {
	Name      string  `json:"name"`
	Needles   int     `json:"needles"`
	Retained  int     `json:"retained"`
	Retention float64 `json:"retention"`
}

// Result is one arm's score.
type Result struct {
	Name string `json:"name"`
	Why  string `json:"why"`

	// Kept is how many memories the arm held, and KeptShare that as a fraction of everything stored.
	Kept      int     `json:"kept"`
	KeptShare float64 `json:"kept_share"`

	// Retention is the share of the held-out questions whose answer the arm still holds at all.
	Retention float64 `json:"retention"`

	// Retrieval is the share whose answer the arm both holds AND returns within the top k. It can
	// only be lower than Retention, and the gap between them is the cost of ranking rather than of
	// forgetting.
	Retrieval float64 `json:"retrieval"`

	Needles   int `json:"needles"`
	Retained  int `json:"retained"`
	Retrieved int `json:"retrieved"`

	// ByLag decomposes Retention by how stale the needle was when the window closed.
	ByLag []Bucket `json:"by_lag"`

	// ByKind decomposes Retention by which question was being asked. This is the split that matters
	// most: "what will be looked up next" is a cache question that recency answers almost by
	// definition, while "what matters regardless of access" is the one a memory store exists for.
	// Averaging the two into a single number hides whichever the policy is bad at.
	ByKind []KindResult `json:"by_kind"`
}

// KindResult is one question kind's score for one arm.
type KindResult struct {
	Kind      string  `json:"kind"`
	Needles   int     `json:"needles"`
	Retained  int     `json:"retained"`
	Retrieved int     `json:"retrieved"`
	Retention float64 `json:"retention"`
	Retrieval float64 `json:"retrieval"`
}

// Score evaluates every arm against the trace's held-out retrieval set. oracle is the control
// instance's ranking per retrieval, used for any arm carrying none of its own.
func Score(tr *trace.Trace, arms []Arm, oracle [][]int, k int) []Result {
	out := make([]Result, 0, len(arms))

	lags := lagOf(tr)
	kinds := kindsOf(tr)

	for _, arm := range arms {
		result := Result{
			Name:    arm.Name,
			Why:     arm.Why,
			Kept:    len(arm.Retained),
			Needles: len(tr.Retrievals),
			ByLag:   make([]Bucket, len(lagBuckets)),
		}

		for i, v := range lagBuckets {
			result.ByLag[i].Name = v.Name
		}

		result.ByKind = make([]KindResult, len(kinds))

		for i, v := range kinds {
			result.ByKind[i].Kind = v
		}

		if len(tr.Memories) > 0 {
			result.KeptShare = float64(result.Kept) / float64(len(tr.Memories))
		}

		for i, v := range tr.Retrievals {
			bucket := bucketFor(lags[v.Needle])
			result.ByLag[bucket].Needles++

			kind := indexOfKind(kinds, string(v.Kind))
			if kind >= 0 {
				result.ByKind[kind].Needles++
			}

			if !arm.Retained[v.Needle] {
				continue
			}

			result.Retained++
			result.ByLag[bucket].Retained++

			if kind >= 0 {
				result.ByKind[kind].Retained++
			}

			if inTopK(arm.rankingFor(i, oracle), arm.Retained, v.Needle, k) {
				result.Retrieved++

				if kind >= 0 {
					result.ByKind[kind].Retrieved++
				}
			}
		}

		if result.Needles > 0 {
			result.Retention = float64(result.Retained) / float64(result.Needles)
			result.Retrieval = float64(result.Retrieved) / float64(result.Needles)
		}

		for i := range result.ByLag {
			if result.ByLag[i].Needles > 0 {
				result.ByLag[i].Retention = float64(result.ByLag[i].Retained) / float64(result.ByLag[i].Needles)
			}
		}

		for i := range result.ByKind {
			if n := result.ByKind[i].Needles; n > 0 {
				result.ByKind[i].Retention = float64(result.ByKind[i].Retained) / float64(n)
				result.ByKind[i].Retrieval = float64(result.ByKind[i].Retrieved) / float64(n)
			}
		}

		out = append(out, result)
	}

	return out
}

// kindsOf is the question kinds present in a trace, in a stable order: the declared kinds first so a
// report's columns never move, then anything else found.
//
// The catch-all matters. A retrieval carrying no kind at all is legitimate - a hand-built trace in a
// test, or one generated before kinds existed - and it must land in a real column rather than
// nowhere, because indexOfKind's answer is used to index the result.
func kindsOf(tr *trace.Trace) []string {
	seen := map[string]bool{}

	var out []string

	add := func(kind string) {
		if seen[kind] {

			return
		}

		seen[kind] = true

		out = append(out, kind)
	}

	for _, v := range []trace.RetrievalKind{trace.KindNextTouch, trace.KindMustKeep} {
		for _, r := range tr.Retrievals {
			if r.Kind == v {
				add(string(v))
			}
		}
	}

	for _, r := range tr.Retrievals {
		add(string(r.Kind))
	}

	return out
}

// indexOfKind locates a kind's column. It reports -1 rather than 0 for a kind that is not there, so
// a caller cannot index an empty result by accident - which is exactly what a zero would have done.
func indexOfKind(kinds []string, kind string) int {
	for i, v := range kinds {
		if v == kind {

			return i
		}
	}

	return -1
}

// lagOf is how long before the window closed each memory was last touched, in SIMULATED time.
//
// Deliberately the trace's schedule rather than the replay's wall clock, unlike the policies:
// this describes the QUESTION being asked - how stale was the answer - which is a property of the
// workload and not of how fast it was replayed.
func lagOf(tr *trace.Trace) []time.Duration {
	last := make([]time.Time, len(tr.Memories))

	for _, v := range tr.Ops {
		if v.At.After(last[v.Memory]) {
			last[v.Memory] = v.At
		}
	}

	out := make([]time.Duration, len(tr.Memories))

	for i := range out {
		out[i] = tr.End.Sub(last[i])
	}

	return out
}

func bucketFor(lag time.Duration) int {
	for i, v := range lagBuckets {
		if lag < v.Upper {

			return i
		}
	}

	return len(lagBuckets) - 1
}

// rankingFor is the arm's own ranking when it has one, and the oracle's otherwise.
func (a Arm) rankingFor(i int, oracle [][]int) []int {
	if a.Ranked != nil {
		if i < len(a.Ranked) {

			return a.Ranked[i]
		}

		return nil
	}

	if i < len(oracle) {

		return oracle[i]
	}

	return nil
}

// inTopK walks a ranking, skipping candidates the arm did not retain, and reports whether the needle
// appears within the first k that survive that filter.
//
// Skipping rather than truncating is the point: a policy that forgot the three memories ranked above
// the needle does not thereby lose it - in its own store those rows are simply absent, and the
// needle ranks three places higher.
func inTopK(ranking []int, retained map[int]bool, needle int, k int) bool {
	seen := 0

	for _, v := range ranking {
		if !retained[v] {
			continue
		}

		if v == needle {

			return true
		}

		seen++

		if seen >= k {

			return false
		}
	}

	return false
}

// Curve scores every baseline across a ladder of store sizes, which is the standard way a cache
// replacement policy is reported and the reason it belongs here.
//
// It is nearly free: a baseline is computed from the trace, so it can be scored at any budget
// without replaying anything. Only the measured arm needs a real run per size, which is why the
// measured arm is a point on this chart rather than a line.
//
// What the curve exposes is that a recency policy is a hard CUTOFF rather than a ranking. Its budget
// buys a window of a certain width - at a fifth of this store, about eight days - and it retains
// everything inside that window perfectly and almost nothing outside it. So its score is not a
// measure of how well it chooses; it is a measure of whether the questions happen to fall inside the
// window its budget bought. Halve the budget and the window halves with it.
func Curve(tr *trace.Trace, oracle [][]int, budgets []int, opts CurveOptions) map[string][]Result {
	out := map[string][]Result{}

	for _, p := range Baselines(opts.Inputs) {
		if p.Name == "keep-everything" {
			continue
		}

		for _, budget := range budgets {
			arm := Arm{Name: p.Name, Why: p.Why, Retained: p.Retain(tr, budget)}
			out[p.Name] = append(out[p.Name], Score(tr, []Arm{arm}, oracle, opts.K)[0])
		}
	}

	return out
}

// CurveOptions carries Curve's scalars, keeping its signature to four parameters.
type CurveOptions struct {
	Inputs

	// K is the k in retrieval@k.
	K int
}

// LongTailBucket is the index of the bucket a bounded store is actually judged on - the questions
// whose answer was last touched over a week before the window closed.
const LongTailBucket = 3

// blended is the control the whole decay model has to justify itself against: rank-normalise
// recency and rank-normalise significance, mix them at a fixed weight, keep the top N.
//
// It exists because "a store that blends recency with a write-time priority" describes both
// Hippocampus and about ten lines of code, and the difference between them is the entire argument
// for the decay curves. If a fixed linear mix matches the tuned model on both question kinds, the
// model is decoration; if the model traces a better frontier, that is a result worth publishing. It
// is scored at every budget the real arms are, so the comparison is never at a size that flatters
// either.
//
// Ranks rather than raw values, because significance and a timestamp share no scale and any
// weighting of the raw numbers would really be a weighting of their units.
func blended(tr *trace.Trace, budget int, recencyWeight float64, in Inputs) map[int]bool {
	recency := rankOf(tr, in, func(i int, last []time.Time) float64 {

		return float64(last[i].UnixNano())
	})

	significance := rankOf(tr, in, func(i int, _ []time.Time) float64 {

		return float64(tr.Memories[i].Significance)
	})

	return topBy(tr, budget, in, func(i int, _ []time.Time) float64 {

		return recencyWeight*recency[i] + (1-recencyWeight)*significance[i]
	})
}

// rankOf maps each memory onto its percentile rank under a scoring function, on [0,1].
func rankOf(tr *trace.Trace, in Inputs, score func(i int, last []time.Time) float64) []float64 {
	last := lastTouched(tr, in)

	order := make([]int, len(tr.Memories))

	for i := range order {
		order[i] = i
	}

	sort.SliceStable(order, func(a int, b int) bool {

		return score(order[a], last) < score(order[b], last)
	})

	out := make([]float64, len(order))

	if len(order) < 2 {

		return out
	}

	for rank, v := range order {
		out[v] = float64(rank) / float64(len(order)-1)
	}

	return out
}
