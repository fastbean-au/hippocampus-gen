package trace

import (
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus-gen/internal/fit"
	"github.com/fastbean-au/hippocampus-gen/internal/params"
)

// loadParams reads the committed parameter file, so the tests exercise the generator against the
// real fitted corpus rather than against invented numbers.
func loadParams(t *testing.T) fit.Params {
	t.Helper()

	out, err := params.Default()
	if err != nil {
		t.Fatalf("loading params: %v", err)
	}

	return out
}

func testConfig(t *testing.T) Config {
	t.Helper()

	return Config{
		Params:               loadParams(t),
		Seed:                 1,
		Memories:             4000,
		Days:                 60,
		Agents:               3,
		SignificanceSignal:   SignalImportance,
		ImportanceShape:      ShapeMeasured,
		SignificanceScale:    ScaleLinear,
		MustKeepShare:        0.05,
		SignificanceNoise:    0.3,
		LinkScale:            0.15,
		RetrievalHorizonDays: 30,
		TermsPerMemory:       4,
		MemoriesPerTerm:      100,
		QueryTerms:           3,
		MinSignificance:      1000,
		MaxSignificance:      30000,
		BodyBytes:            256,
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	cfg := testConfig(t)

	a, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	b, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if len(a.Ops) != len(b.Ops) || len(a.Retrievals) != len(b.Retrievals) {
		t.Fatalf("same seed gave different sizes: %d/%d ops, %d/%d retrievals",
			len(a.Ops), len(b.Ops), len(a.Retrievals), len(b.Retrievals))
	}

	for i := range a.Ops {
		if a.Ops[i] != b.Ops[i] {
			t.Fatalf("op %d differs between runs of the same seed", i)
		}
	}

	// A published benchmark result has to be reproducible, and that means the seed - not the clock,
	// not map iteration order - decides everything.
	for i := range a.Memories {
		if a.Memories[i].Significance != b.Memories[i].Significance {
			t.Fatalf("memory %d significance differs between runs of the same seed", i)
		}
	}

	cfg.Seed = 2

	c, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if len(c.Ops) == len(a.Ops) && c.Ops[len(c.Ops)/2] == a.Ops[len(a.Ops)/2] {
		t.Error("a different seed produced an identical trace")
	}
}

func TestOpsAreOrderedAndStoresPrecedeTheirRecalls(t *testing.T) {
	tr, err := Generate(testConfig(t))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	stored := map[int]bool{}

	for i, v := range tr.Ops {
		if i > 0 && v.At.Before(tr.Ops[i-1].At) {
			t.Fatalf("op %d is out of time order", i)
		}

		if v.Kind == OpStore {
			if stored[v.Memory] {
				t.Fatalf("memory %d is stored twice", v.Memory)
			}

			stored[v.Memory] = true

			continue
		}

		// A recall of a memory that has not been written yet would be a NotFound at replay, and
		// worse, would silently drop the reinforcement the whole benchmark turns on.
		if !stored[v.Memory] {
			t.Fatalf("memory %d is recalled before it is stored", v.Memory)
		}
	}
}

// TestGeneratedReuseMatchesTheFittedDistribution is the test that matters most: it measures the
// generated trace and checks it reproduces the corpus the parameters came from.
//
// It deliberately checks the COMBINED gap distribution rather than the burst/tail split. The split
// is drawn correctly, but sessions in a generated trace are derived by chunking rather than laid out
// in time (see the package comment), so refitting one would recover the chunking's split and not the
// drawn one. What must survive generation is the mixture's shape - and above all the long tail,
// which is the entire argument for the benchmark existing.
func TestGeneratedReuseMatchesTheFittedDistribution(t *testing.T) {
	cfg := testConfig(t)

	tr, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	last := map[int]time.Time{}

	var gaps []float64

	for _, v := range tr.Ops {
		if prev, ok := last[v.Memory]; ok {
			gaps = append(gaps, v.At.Sub(prev).Hours())
		}

		last[v.Memory] = v.At
	}

	if len(gaps) < 1000 {
		t.Fatalf("only %d reuse gaps generated - too few to compare", len(gaps))
	}

	sort.Float64s(gaps)

	// The expected mixture, sampled straight from the parameters the generator was given.
	rng := rand.New(rand.NewSource(99))
	reuse := cfg.Params.Reuse

	var want []float64

	for i := 0; i < 200000; i++ {
		if rng.Float64() < reuse.BurstShare {
			want = append(want, reuse.Burst.Sample(rng.Float64()))

			continue
		}

		want = append(want, reuse.Tail.Sample(rng.Float64()))
	}

	sort.Float64s(want)

	// The window truncates the longest gaps - a reuse falling past the end becomes a retrieval
	// instead - so the upper percentiles are compared loosely and the median tightly.
	compareQuantile(t, "median", gaps, want, 0.5, 0.5)
	compareQuantile(t, "p75", gaps, want, 0.75, 0.5)
	compareQuantile(t, "p90", gaps, want, 0.9, 0.6)

	beyondDay := shareBeyond(gaps, 24)
	beyondWeek := shareBeyond(gaps, 24*7)

	// The headline figures from the corpus - about 11% of reuses beyond a day and 3% beyond a week.
	// If generation lost these, the trace would no longer pose the problem the benchmark exists to
	// measure, and every policy would look alike.
	if beyondDay < 0.05 || beyondDay > 0.25 {
		t.Errorf("share of reuses beyond a day is %.3f, want near the fitted %.3f",
			beyondDay, reuse.BeyondOneDay)
	}

	if beyondWeek < 0.01 {
		t.Errorf("share of reuses beyond a week is %.3f, want near the fitted %.3f",
			beyondWeek, reuse.BeyondOneWeek)
	}
}

// compareQuantile checks two sorted samples agree at a quantile within a relative tolerance.
func compareQuantile(t *testing.T, name string, got []float64, want []float64, q float64, tol float64) {
	t.Helper()

	g := got[int(q*float64(len(got)-1))]
	w := want[int(q*float64(len(want)-1))]

	if w == 0 {

		return
	}

	if ratio := math.Abs(g-w) / w; ratio > tol {
		t.Errorf("%s: generated %.4fh against fitted %.4fh (%.0f%% off, tolerance %.0f%%)",
			name, g, w, 100*ratio, 100*tol)
	}
}

func shareBeyond(sorted []float64, hours float64) float64 {
	n := 0

	for _, v := range sorted {
		if v > hours {
			n++
		}
	}

	return float64(n) / float64(len(sorted))
}

// TestGeneratedPopularityMatchesTheFittedLadder checks the other property reproduced by
// construction: how references concentrate onto a few entities, and how many are touched once and
// never again. A third of the store being write-once is the case FOR forgetting, so a generator that
// lost it would be flattering the result.
func TestGeneratedPopularityMatchesTheFittedLadder(t *testing.T) {
	cfg := testConfig(t)

	tr, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	once := 0

	for _, v := range tr.Memories {
		if v.Recalls == 0 {
			once++
		}
	}

	share := float64(once) / float64(len(tr.Memories))

	// The generated share sits above the fitted one because a reuse drawn past the window's end is
	// truncated into the retrieval set, which the corpus measurement had no equivalent of. It must
	// stay in the same territory rather than drifting to nearly all or nearly none.
	if share < 0.25 || share > 0.6 {
		t.Errorf("never-recalled share is %.2f, want roughly the fitted %.2f",
			share, cfg.Params.Popularity.OnceOnlyFraction)
	}
}

// TestRetrievalSetIsHeldOut is the anti-circularity check. If any part of the question set reached
// the store, the benchmark would be measuring its own answer key.
func TestRetrievalSetIsHeldOut(t *testing.T) {
	cfg := testConfig(t)

	tr, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if len(tr.Retrievals) == 0 {
		t.Fatal("no retrievals generated")
	}

	horizon := tr.End.Add(time.Duration(cfg.RetrievalHorizonDays * float64(24*time.Hour)))

	for _, v := range tr.Retrievals {
		if !v.DueAt.After(tr.End) {
			t.Fatalf("retrieval for memory %d is due at %s, inside the replay window", v.Needle, v.DueAt)
		}

		if v.DueAt.After(horizon) {
			t.Fatalf("retrieval for memory %d is due at %s, past the horizon", v.Needle, v.DueAt)
		}

		if v.Query == "" {
			t.Fatalf("retrieval for memory %d has an empty query", v.Needle)
		}

		if !tr.Memories[v.Needle].Wanted {
			t.Fatalf("memory %d is a needle but is not flagged wanted", v.Needle)
		}
	}

	// No operation may fall at or after the window's end, which is what makes the held-out set
	// unobservable rather than merely unused.
	for _, v := range tr.Ops {
		if v.At.After(tr.End) {
			t.Fatalf("op on memory %d lands at %s, past the window end %s", v.Memory, v.At, tr.End)
		}
	}
}

// TestSignificanceTracksRecallsUnderLowNoiseAndNotUnderHigh pins the sweep axis. The benchmark's
// most interesting question is how much write-time signal is needed, so the knob controlling it has
// to actually control it.
func TestSignificanceTracksRecallsUnderLowNoiseAndNotUnderHigh(t *testing.T) {
	cfg := testConfig(t)
	cfg.SignificanceSignal = SignalFrequency
	cfg.SignificanceNoise = 0

	clean, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg.SignificanceNoise = 1

	noisy, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cleanR := correlation(clean)
	noisyR := correlation(noisy)

	if cleanR < 0.8 {
		t.Errorf("with no noise, significance should track recalls closely, got r=%.2f", cleanR)
	}

	if math.Abs(noisyR) > 0.2 {
		t.Errorf("with full noise, significance should be uncorrelated with recalls, got r=%.2f", noisyR)
	}
}

// correlation is Pearson's r between log1p(recalls) and significance across a trace's memories.
func correlation(tr *Trace) float64 {
	var xs, ys []float64

	for _, v := range tr.Memories {
		xs = append(xs, math.Log1p(float64(v.Recalls)))
		ys = append(ys, float64(v.Significance))
	}

	mx, my := meanOf(xs), meanOf(ys)
	num, dx, dy := 0.0, 0.0, 0.0

	for i := range xs {
		num += (xs[i] - mx) * (ys[i] - my)
		dx += (xs[i] - mx) * (xs[i] - mx)
		dy += (ys[i] - my) * (ys[i] - my)
	}

	if dx == 0 || dy == 0 {

		return 0
	}

	return num / math.Sqrt(dx*dy)
}

func meanOf(xs []float64) float64 {
	total := 0.0

	for _, v := range xs {
		total += v
	}

	return total / float64(len(xs))
}

// TestLinksPointBackwardsAndStayInBounds guards the two ways a link can fail a replay outright: a
// target the store does not hold yet, and more links than the contract accepts.
func TestLinksPointBackwardsAndStayInBounds(t *testing.T) {
	tr, err := Generate(testConfig(t))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	linked := 0

	for i, v := range tr.Memories {
		if len(v.Links) > maxLinksPerMemory {
			t.Fatalf("memory %d declares %d links, over the contract's %d", i, len(v.Links), maxLinksPerMemory)
		}

		if len(v.Links) > 0 {
			linked++
		}

		seen := map[int]struct{}{}

		for _, target := range v.Links {
			if target == i {
				t.Fatalf("memory %d links to itself", i)
			}

			if _, ok := seen[target]; ok {
				t.Fatalf("memory %d links to %d twice", i, target)
			}

			seen[target] = struct{}{}

			if !tr.Memories[target].At.Before(v.At) {
				t.Fatalf("memory %d links forward to %d, which the store will not hold yet", i, target)
			}
		}
	}

	if linked == 0 {
		t.Error("no links were generated at all")
	}
}

func TestGenerateRejectsBadConfigurations(t *testing.T) {
	base := testConfig(t)

	cases := map[string]func(c *Config){
		"no memories":       func(c *Config) { c.Memories = 0 },
		"no days":           func(c *Config) { c.Days = 0 },
		"no agents":         func(c *Config) { c.Agents = 0 },
		"noise above one":   func(c *Config) { c.SignificanceNoise = 1.5 },
		"inverted range":    func(c *Config) { c.MinSignificance, c.MaxSignificance = 30000, 1000 },
		"no terms":          func(c *Config) { c.TermsPerMemory = 0 },
		"query too wide":    func(c *Config) { c.QueryTerms = 99 },
		"no popularity fit": func(c *Config) { c.Params.Popularity.CountQuantiles = nil },
		"unknown signal":    func(c *Config) { c.SignificanceSignal = "vibes" },
		"unknown shape":     func(c *Config) { c.ImportanceShape = "wobbly" },
		"must-keep over 1":  func(c *Config) { c.MustKeepShare = 1.5 },
		"unknown scale":     func(c *Config) { c.SignificanceScale = "logarithmicish" },
		"log from zero":     func(c *Config) { c.SignificanceScale = ScaleLog; c.MinSignificance = 0 },
	}

	for name, mutate := range cases {
		cfg := base
		mutate(&cfg)

		if _, err := Generate(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestBodiesCarryTheirTermsAndAUniqueToken(t *testing.T) {
	tr, err := Generate(testConfig(t))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	tokens := map[string]int{}

	for i, v := range tr.Memories {
		if prev, ok := tokens[v.Token]; ok {
			t.Fatalf("memories %d and %d share the token %q", prev, i, v.Token)
		}

		tokens[v.Token] = i

		if len(v.Body) < tr.Config.BodyBytes {
			t.Fatalf("memory %d body is %d bytes, under the %d requested", i, len(v.Body), tr.Config.BodyBytes)
		}
	}
}

// TestTheQuestionSetIsNotTriviallySolvedByRecency guards the benchmark's fairness from both sides.
//
// Needles are entities whose next reference fell just past the window, so how recently they were
// last touched decides how much a recency baseline can win by knowing nothing else. Two ways this
// could go wrong and neither is detectable from the result alone: if every needle had been touched
// moments before the cutoff, "keep the newest N" would score near-perfectly and the benchmark would
// prove nothing; if none had, recency would score near-zero and the comparison would be a straw man.
//
// What the fitted distribution actually produces is neither, by way of the inspection paradox - a
// gap long enough to straddle the window's end is unlikely to be a short one. Needles are meaningfully
// more recent than the store at large, and still spread over weeks.
func TestTheQuestionSetIsNotTriviallySolvedByRecency(t *testing.T) {
	tr, err := Generate(testConfig(t))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	last := map[int]time.Time{}

	for _, v := range tr.Ops {
		last[v.Memory] = v.At
	}

	var needles, all []float64

	for _, v := range tr.Retrievals {
		needles = append(needles, tr.End.Sub(last[v.Needle]).Hours())
	}

	for i := range tr.Memories {
		all = append(all, tr.End.Sub(last[i]).Hours())
	}

	sort.Float64s(needles)
	sort.Float64s(all)

	median := needles[len(needles)/2]
	p90 := needles[9*len(needles)/10]

	// Recency must carry signal: a needle should be more recently touched than a typical memory.
	if median >= all[len(all)/2] {
		t.Errorf("needles (median %.1fh since last touch) are no more recent than the store at large (%.1fh) - recency would be worthless",
			median, all[len(all)/2])
	}

	// But not so much signal that recency alone answers the question. If almost every needle sat
	// within a day of the cutoff, a recency window would be unbeatable and nothing else would matter.
	if within(needles, 24) > 0.6 {
		t.Errorf("%.0f%% of needles were touched within a day of the cutoff - the question set is trivially recency-solvable",
			100*within(needles, 24))
	}

	// And the hard tail has to exist, since it is where a bounded store's policy is actually tested.
	if p90 < 24 {
		t.Errorf("needle p90 age is only %.1fh - the question set has no long-gap tail to discriminate on", p90)
	}
}

// within is the share of a sorted sample at or below a bound.
func within(sorted []float64, hours float64) float64 {
	n := 0

	for _, v := range sorted {
		if v <= hours {
			n++
		}
	}

	return float64(n) / float64(len(sorted))
}
