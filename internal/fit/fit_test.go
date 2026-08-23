package fit

import (
	"math"
	"testing"
	"time"
)

// base is the fixture corpus's first timestamp, so a test can express an expectation as an offset
// from it rather than as a literal.
var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func approx(t *testing.T, name string, got float64, want float64, tol float64) {
	t.Helper()

	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %g, want %g (+/- %g)", name, got, want, tol)
	}
}

func TestScanReadsReferencesAndSkipsUnparseableLines(t *testing.T) {
	obs, err := Scan("testdata/transcripts", "/proj")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Four in-prefix references: a, b, a in session-a and a in session-b. The /other/c.go call is
	// outside the prefix, the text block names nothing, and the trailing garbage line is skipped.
	if len(obs.References) != 4 {
		t.Fatalf("expected 4 references, got %d: %+v", len(obs.References), obs.References)
	}

	if !obs.References[0].At.Equal(base) {
		t.Errorf("references are not in time order, first is %s", obs.References[0].At)
	}

	// Items count every tool call plus plain user turns, including the one whose path was filtered
	// out - what a session produced is not the same question as which entities it touched.
	if obs.Items["sess-a"] != 4 {
		t.Errorf("sess-a items: got %d, want 4", obs.Items["sess-a"])
	}

	if obs.Items["sess-b"] != 2 {
		t.Errorf("sess-b items: got %d, want 2", obs.Items["sess-b"])
	}
}

func TestScanRejectsAnEmptyOrUnmatchedCorpus(t *testing.T) {
	if _, err := Scan("testdata", ""); err == nil {
		t.Error("expected an error for a directory holding no transcripts")
	}

	if _, err := Scan("testdata/transcripts", "/nothing-matches-this"); err == nil {
		t.Error("expected an error when the prefix excludes every reference")
	}
}

func TestFitPopularity(t *testing.T) {
	obs, err := Scan("testdata/transcripts", "/proj")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	got := obs.Fit("fixture", base).Popularity

	// a.go is referenced three times and b.go once, so one of the two entities is a singleton.
	approx(t, "once-only fraction", got.OnceOnlyFraction, 0.5, 1e-9)

	// Both entities are needed to reach 80% of four references, the head being three.
	approx(t, "head fraction", got.HeadFraction, 1.0, 1e-9)

	// The slope of log frequency against log rank through (0, log3) and (log2, log1).
	approx(t, "zipf alpha", got.ZipfAlpha, 1.585, 0.01)
}

func TestFitSplitsReuseOnTheSessionNotAThreshold(t *testing.T) {
	obs, err := Scan("testdata/transcripts", "/proj")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	got := obs.Fit("fixture", base).Reuse

	// Two reuses of a.go: one 20 seconds later in the same session, one two days later in another.
	// The split is on the session, so the burst share is exactly one half.
	approx(t, "burst share", got.BurstShare, 0.5, 1e-9)

	if got.Burst.Samples != 1 || got.Tail.Samples != 1 {
		t.Fatalf("expected one sample in each mode, got burst=%d tail=%d", got.Burst.Samples, got.Tail.Samples)
	}

	approx(t, "burst median hours", got.Burst.MedianHours, 20.0/3600, 1e-9)
	approx(t, "tail median hours", got.Tail.MedianHours, 48-20.0/3600, 1e-6)

	// The figures the benchmark exists to justify: half the reuse here is beyond a day, none beyond
	// a week.
	approx(t, "beyond one day", got.BeyondOneDay, 0.5, 1e-9)
	approx(t, "beyond one week", got.BeyondOneWeek, 0, 1e-9)
}

func TestFitSessionsAndLinks(t *testing.T) {
	obs, err := Scan("testdata/transcripts", "/proj")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	params := obs.Fit("fixture", base)

	if params.Corpus.Sessions != 2 || params.Corpus.DistinctEntities != 2 {
		t.Errorf("corpus: got %d sessions / %d entities, want 2/2",
			params.Corpus.Sessions, params.Corpus.DistinctEntities)
	}

	approx(t, "span days", params.Corpus.SpanDays, 2, 1e-6)

	// One inter-arrival gap, the two days between the sessions' first references.
	if params.Sessions.InterArrivalHours.Samples != 1 {
		t.Fatalf("expected one inter-arrival sample, got %d", params.Sessions.InterArrivalHours.Samples)
	}

	approx(t, "inter-arrival median", params.Sessions.InterArrivalHours.MedianHours, 48, 1e-6)

	if params.Sessions.ItemsPerSession.Min != 2 || params.Sessions.ItemsPerSession.Max != 4 {
		t.Errorf("items per session: got min %d max %d, want 2/4",
			params.Sessions.ItemsPerSession.Min, params.Sessions.ItemsPerSession.Max)
	}

	// sess-a touched two entities (one pair), sess-b touched one (no pair).
	approx(t, "pairs per session", params.Links.CooccurrencePairsPerSession, 0.5, 1e-9)
	approx(t, "links per reference", params.Links.LinksPerReference, 0.25, 1e-9)
}

func TestFitRecordsProvenance(t *testing.T) {
	obs, err := Scan("testdata/transcripts", "/proj")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	params := obs.Fit("a description", base)

	if params.FittedFrom != "a description" {
		t.Errorf("fitted_from: got %q", params.FittedFrom)
	}

	// A committed parameter file has to say when it was fitted, since the corpus does not ship
	// beside it.
	if params.FittedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("fitted_at: got %q", params.FittedAt)
	}
}

func TestDistributionSampleInterpolatesAndClamps(t *testing.T) {
	d := Distribution{Quantiles: []float64{0, 10, 20}}

	cases := []struct {
		u    float64
		want float64
	}{
		{-1, 0},
		{0, 0},
		{0.25, 5},
		{0.5, 10},
		{0.75, 15},
		{1, 20},
		{2, 20},
	}

	for _, v := range cases {
		approx(t, "sample", d.Sample(v.u), v.want, 1e-9)
	}
}

func TestDistributionSampleFallsBackWithoutALadder(t *testing.T) {
	// Several modes are legitimately empty on a thin corpus, so this must not panic.
	approx(t, "empty", Distribution{}.Sample(0.5), 0, 1e-9)
	approx(t, "median only", Distribution{MedianHours: 7}.Sample(0.5), 7, 1e-9)
}

// TestSampledLadderReproducesTheSourceDistribution is the check that the generated trace will
// actually look like the corpus. It is the reason the ladder exists at all: the parametric
// log-normal fit does not survive this test on the real corpus's cross-session mode, whose fitted
// parameters imply a median several times its observed one.
func TestSampledLadderReproducesTheSourceDistribution(t *testing.T) {
	// A deliberately bimodal sample - a tight cluster of fast reuses and a scattering of slow ones -
	// which is the shape a single log-normal cannot hold.
	var samples []float64

	for i := 0; i < 800; i++ {
		samples = append(samples, 0.001+0.0001*float64(i%10))
	}

	for i := 0; i < 200; i++ {
		samples = append(samples, 10+float64(i))
	}

	d := fitDistribution(samples)

	// Draw uniformly across the ladder and compare the redrawn median and 90th percentile with the
	// source's.
	var drawn []float64

	const draws = 4000

	for i := 0; i < draws; i++ {
		drawn = append(drawn, d.Sample(float64(i)/float64(draws-1)))
	}

	sortFloats(drawn)

	approx(t, "resampled median", percentile(drawn, 0.5), d.MedianHours, math.Max(d.MedianHours*0.2, 0.001))
	approx(t, "resampled p90", percentile(drawn, 0.9), d.P90Hours, d.P90Hours*0.2)

	// And the bimodality survives: the ladder must not have smoothed the gap between the modes away.
	if percentile(drawn, 0.5) > 1 {
		t.Errorf("fast mode lost: resampled median %g should stay well under an hour", percentile(drawn, 0.5))
	}

	if percentile(drawn, 0.95) < 5 {
		t.Errorf("slow mode lost: resampled p95 %g should be in the slow cluster", percentile(drawn, 0.95))
	}
}

func sortFloats(xs []float64) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
