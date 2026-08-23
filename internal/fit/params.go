// Package fit measures the working-memory dynamics of a real agent from its session transcripts,
// and emits them as the parameter set the synthetic trace generator consumes.
//
// The point of the exercise is credibility rather than convenience. A retention benchmark whose
// author writes both the trace and the ground truth proves nothing, so the reuse distribution the
// trace reproduces is measured from a corpus rather than invented. The corpus itself is private
// working data and does not ship; the fitted Params do, which is what makes the benchmark auditable
// - a reader can see exactly which distribution the trace was built to reproduce, and what it was
// fitted from.
//
// The measurement that matters most is that reuse is BIMODAL. An agent re-reads something seconds
// later (the within-session burst) far more often than it returns to it days later, but the second
// mode is the one a bounded store is judged on: a recency window sized for the burst discards
// precisely what is wanted next week. Burst and tail are therefore fitted SEPARATELY, split by
// whether the re-reference fell in the same session, never smoothed into one distribution.
package fit

// Params is the fitted parameter set, written as JSON and read back by the trace generator. Every
// field is either something the generator samples from or something a test asserts the generated
// trace reproduces - there is nothing here for decoration.
type Params struct {
	// FittedFrom describes the corpus, and FittedAt when. Neither is consumed by the generator;
	// they exist so a committed params file says what it came from without the corpus being present.
	FittedFrom string `json:"fitted_from"`
	FittedAt   string `json:"fitted_at"`

	Corpus     Corpus     `json:"corpus"`
	Popularity Popularity `json:"popularity"`
	Importance Importance `json:"importance"`
	Reuse      Reuse      `json:"reuse"`
	Sessions   Sessions   `json:"sessions"`
	Links      Links      `json:"links"`
}

// Corpus records the shape of what was measured, so a fit from a thin corpus is visible as such
// rather than being mistaken for a confident one.
type Corpus struct {
	Sessions         int     `json:"sessions"`
	References       int     `json:"references"`
	DistinctEntities int     `json:"distinct_entities"`
	SpanDays         float64 `json:"span_days"`
	ReferencesPerDay float64 `json:"references_per_day"`
}

// Popularity is how references concentrate across entities. ZipfAlpha is the slope of a log-log
// regression of frequency against rank, which is what the generator draws entities from;
// OnceOnlyFraction and HeadFraction are validation targets rather than generator inputs - a
// generated trace that reproduces alpha but not the once-only share has not reproduced the corpus.
type Popularity struct {
	ZipfAlpha        float64 `json:"zipf_alpha"`
	OnceOnlyFraction float64 `json:"once_only_fraction"`

	// CountQuantiles is the inverse-CDF ladder of per-entity reference counts, and is what the
	// generator draws an entity's popularity from. It is here for the same reason Distribution
	// carries one: a pure power law fitted to this corpus overshoots the head badly (it implies a
	// most-referenced entity around 655 references against an observed 229) and puts half the
	// entities in the singleton bucket against an observed third. ZipfAlpha describes the shape for
	// a reader; the ladder reproduces it.
	CountQuantiles []float64 `json:"count_quantiles"`

	// HeadFraction is the share of distinct entities that together account for HeadShare of all
	// references - the usual "80% of the traffic comes from 30% of the files" statement.
	HeadFraction float64 `json:"head_fraction"`
	HeadShare    float64 `json:"head_share"`
}

// Importance is the signal that is NOT an access pattern, and it exists because the first version of
// this benchmark did not have one.
//
// That version made significance a noisy function of how often a memory would be recalled. Which
// meant significance carried no information the recall stream did not already carry: the store was
// handed a degraded copy of something it could already see, and asked to beat a policy using the
// clean version. It could not, by construction rather than by design, and the tell was that the
// significance baseline and the LFU baseline scored within a point of each other.
//
// What is needed is something that predicts future need WITHOUT being a function of past access. In
// a real store that is what significance is: a writer asserting that a thing matters for reasons the
// access log does not show - a credential, a decision record, a constraint that stays true whether
// or not anyone re-reads it.
//
// The corpus supplies one, and it is measured rather than declared: whether the agent MUTATED the
// entity or only read it. Editing a file is a different act from consulting it, and how often you
// consult something says little about whether you changed it. MutationShare is the per-entity
// fraction of references that were mutations, and PopularityCorrelation is how much that travels
// with raw reference count - so the generator reproduces the real overlap between the two signals
// instead of assuming they are independent.
type Importance struct {
	// Quantiles is the inverse-CDF ladder of per-entity mutation share, which the generator draws an
	// entity's latent importance from.
	Quantiles []float64 `json:"quantiles"`

	Mean float64 `json:"mean"`

	// NeverMutated is the share of entities that were only ever read. These are the ones a store
	// leaning on access patterns cannot distinguish at all.
	NeverMutated float64 `json:"never_mutated"`

	// PopularityCorrelation is Pearson's r between an entity's mutation share and the log of its
	// reference count. Near zero means importance is genuinely independent of how often a thing is
	// touched, which is the case that makes significance worth having.
	PopularityCorrelation float64 `json:"popularity_correlation"`

	Mutations  int `json:"mutations"`
	References int `json:"references"`
}

// Distribution is a fitted positive-valued sample - a re-reference gap, a session length - held two
// ways, because the two are for different readers.
//
// MeanLog and StdDevLog summarise it as a log-normal, which is the conventional description of a
// duration spanning orders of magnitude and is what a person reads. Quantiles is an empirical
// inverse-CDF ladder, and is what the generator actually SAMPLES from.
//
// Carrying both is deliberate rather than redundant. Fitting the corpus showed the cross-session
// mode is not log-normal at all: its fitted parameters imply a 5.7h median against an observed 17h,
// because that mode is itself a mixture - overlapping sessions contributing near-zero gaps, and
// genuine multi-day returns. Sampling the parametric fit would have generated a trace that did not
// reproduce the corpus it was fitted from, so the ladder is authoritative and the log-normal is
// commentary. Comparing MedianHours against exp(MeanLog) is how badly-fitting modes announce
// themselves.
type Distribution struct {
	Samples     int     `json:"samples"`
	MeanLog     float64 `json:"mean_log"`
	StdDevLog   float64 `json:"stddev_log"`
	MedianHours float64 `json:"median_hours"`
	P90Hours    float64 `json:"p90_hours"`
	P99Hours    float64 `json:"p99_hours"`

	// Quantiles holds QuantileSteps+1 evenly spaced points of the empirical inverse CDF, from the
	// minimum at index 0 to the maximum at the last. Sample interpolates between them.
	Quantiles []float64 `json:"quantiles"`
}

// QuantileSteps is the number of intervals in a Distribution's inverse-CDF ladder, so it carries
// QuantileSteps+1 points. Twenty is enough to hold a bimodal shape faithfully while leaving the
// parameter file readable.
const QuantileSteps = 20

// Sample draws from the empirical distribution by linear interpolation into the quantile ladder,
// for u uniform on [0,1]. It falls back to the log-normal fit when no ladder is present, and to
// zero when neither is - a distribution fitted from an empty sample yields zero rather than a
// panic, since several are legitimately empty on a thin corpus.
func (d Distribution) Sample(u float64) float64 {
	if len(d.Quantiles) < 2 {

		return d.MedianHours
	}

	if u <= 0 {

		return d.Quantiles[0]
	}

	if u >= 1 {

		return d.Quantiles[len(d.Quantiles)-1]
	}

	pos := u * float64(len(d.Quantiles)-1)
	i := int(pos)
	frac := pos - float64(i)

	return d.Quantiles[i] + frac*(d.Quantiles[i+1]-d.Quantiles[i])
}

// Reuse is the bimodal re-reference model - the centre of the whole exercise.
//
// BurstShare splits the two modes: the fraction of re-references that landed in the SAME session as
// the previous reference to that entity. That split is observed rather than chosen, which is why it
// is preferred to thresholding the gap at some arbitrary duration.
//
// BeyondOneDay and BeyondOneWeek are the numbers that justify the benchmark existing: they are the
// share of reuses a recency window of that size would have missed.
type Reuse struct {
	BurstShare float64      `json:"burst_share"`
	Burst      Distribution `json:"burst"`
	Tail       Distribution `json:"tail"`

	BeyondOneDay  float64 `json:"beyond_one_day"`
	BeyondOneWeek float64 `json:"beyond_one_week"`
}

// Sessions is the arrival process: how often an agent session starts, how long it runs, and how
// much it produces. The generator lays simulated time out from these.
type Sessions struct {
	InterArrivalHours Distribution `json:"inter_arrival_hours"`
	DurationHours     Distribution `json:"duration_hours"`

	// ItemsPerSession counts everything a session produced, entity-naming or not, and is
	// descriptive. ReferencesPerSession counts only what named an entity, and is the arrival rate
	// the generator actually lays memories out at.
	ItemsPerSession            Discrete `json:"items_per_session"`
	ReferencesPerSession       Discrete `json:"references_per_session"`
	DistinctEntitiesPerSession Discrete `json:"distinct_entities_per_session"`
}

// Discrete summarises a count-valued sample. It carries both the mean/standard deviation the
// generator samples from and the observed extremes, so a generated session count wildly outside the
// corpus range is visible.
type Discrete struct {
	Samples int     `json:"samples"`
	Mean    float64 `json:"mean"`
	StdDev  float64 `json:"stddev"`
	Min     int     `json:"min"`
	Median  int     `json:"median"`
	Max     int     `json:"max"`
}

// Links is the associative structure, taken from entities referenced within the same session. Two
// entities an agent touched in one sitting are related in the sense the store means by a link, and
// this is the only place the trace's link graph comes from.
type Links struct {
	// CooccurrencePairsPerSession is the mean number of distinct entity pairs co-occurring in one
	// session, and LinksPerReference the same quantity normalised by reference count - the form the
	// generator uses, since it decides links per memory written rather than per session.
	CooccurrencePairsPerSession float64 `json:"cooccurrence_pairs_per_session"`
	LinksPerReference           float64 `json:"links_per_reference"`
}
