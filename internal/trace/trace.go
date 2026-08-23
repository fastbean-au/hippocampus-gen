// Package trace generates the synthetic agent workload the retention benchmark replays, from the
// parameters internal/fit measured off a real agent's transcripts.
//
// # What is being modelled
//
// An agent works on entities - files, facts, decisions. The FIRST time it touches one it stores a
// memory; every later touch is a RECALL of that memory. That mapping is the whole reason the corpus
// is usable: a re-reference gap measured in the transcripts is exactly a recall gap in the store, so
// the reinforcement the store performs is being driven by a distribution somebody actually
// exhibited rather than one chosen here.
//
// # What is exact and what is approximate
//
// Two properties are reproduced BY CONSTRUCTION, because they are what the benchmark's conclusion
// rests on: the per-entity popularity distribution (drawn from the fitted count ladder) and the
// bimodal re-reference gap distribution (drawn from the fitted burst/tail ladders). A test asserts
// both by running the fitter back over a generated trace.
//
// Sessions are approximate, and deliberately so. They are derived by chunking each agent's
// references into runs of the fitted per-session size rather than by laying sessions out in time and
// snapping references into them - snapping would perturb the gap distribution, which is the one
// thing that must stay exact. A session is a bucketing that becomes an event and bounds the link
// graph; the benchmark measures retention, not session realism, and paying for the latter with the
// former would be the wrong trade.
//
// # The ground truth
//
// The retrieval set is drawn from the same renewal process as the recalls, but consists only of
// references landing AFTER the replay window closes. The store therefore never observes any part of
// it - it sees significance at write time and recalls as they land, never the future - which is what
// keeps the benchmark from being circular. An entity whose next reference falls beyond the horizon
// simply is not in the retrieval set, so what the agent "needed later" is decided by the fitted
// distribution and not by this package.
package trace

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/fastbean-au/hippocampus-gen/internal/fit"
)

// maxLinksPerMemory mirrors the contract's own per-item link cap (types.MaxLinks). Declared here
// rather than imported so the generator can clamp before it builds anything the service would
// reject outright.
const maxLinksPerMemory = 128

// LinkSignificance is the weight every generated link carries. A single value keeps the link graph's
// contribution to a memory's value proportional to its DEGREE, which is the property the benchmark
// is interested in - a memory the agent kept returning to alongside others is better connected -
// rather than to a per-edge weight this package would have had to invent.
const LinkSignificance = 1000

// OpKind distinguishes the two things a replay does to the store.
type OpKind int

const (
	// OpStore writes a memory for an entity's first reference.
	OpStore OpKind = iota

	// OpRecall reinforces the memory an entity already has.
	OpRecall
)

// SignificanceSignal names what write-time significance is taken to be a proxy for.
type SignificanceSignal string

const (
	// SignalImportance makes significance track the memory's latent importance, which is drawn
	// independently of its access pattern. This is the only one of the three that gives significance
	// anything to say that the recall stream does not already say, and it is the default for that
	// reason.
	SignalImportance SignificanceSignal = "importance"

	// SignalFrequency makes significance track total recall count.
	SignalFrequency SignificanceSignal = "frequency"

	// SignalLongevity makes significance track how many distinct sessions return to the entity.
	SignalLongevity SignificanceSignal = "longevity"
)

// ImportanceShape selects the distribution latent importance is drawn from.
type ImportanceShape string

const (
	// ShapeMeasured draws from the corpus's fitted mutation-share ladder. Faithful, but coarse: in
	// the fitted corpus half the entities are mutated on every reference and so tie at the top,
	// leaving importance able to separate only the rest.
	ShapeMeasured ImportanceShape = "measured"

	// ShapeGraded draws uniformly instead, preserving the measured INDEPENDENCE from access while
	// restoring spread. Reported alongside the measured shape rather than instead of it.
	ShapeGraded ImportanceShape = "graded"
)

// SignificanceScale is how a latent signal on [0,1] becomes a stored significance.
type SignificanceScale string

const (
	// ScaleLinear spreads significance evenly across the configured range.
	ScaleLinear SignificanceScale = "linear"

	// ScaleLog spreads it geometrically, so the logarithm is even.
	ScaleLog SignificanceScale = "log"
)

// RetrievalKind distinguishes the two questions a bounded store is asked, which are not the same
// question and must not be averaged into one number.
type RetrievalKind string

const (
	// KindNextTouch is "what will the agent look up next" - drawn from the reuse process, so it is
	// predicted by recency almost by definition. A cache is built to answer this.
	KindNextTouch RetrievalKind = "next-touch"

	// KindMustKeep is "what matters, whether or not it has been touched" - drawn by importance,
	// independently of the access pattern. This is the question a memory store exists for and the
	// one the first version of this benchmark never asked.
	KindMustKeep RetrievalKind = "must-keep"
)

// Config are the generation knobs. Params carries everything measured; the rest is what a run
// chooses.
type Config struct {
	Params fit.Params
	Seed   int64

	// Memories is the number of entities, and so the number of memories the trace stores. Days is
	// the simulated span they arrive over, and Agents how many independent workers share the store
	// (each becomes a group label).
	Memories int
	Days     float64
	Agents   int

	// SignificanceSignal is what write-time significance is a proxy for. See significance.
	SignificanceSignal SignificanceSignal

	// ImportanceShape is the distribution latent importance is drawn from.
	ImportanceShape ImportanceShape

	// MustKeepShare is how many "what matters" questions to ask, as a fraction of the store. These
	// are drawn by importance rather than by access, and are disjoint in cause from the next-touch
	// questions though an entity may be picked by both.
	MustKeepShare float64

	// SignificanceNoise blends the write-time significance between a perfect signal and none at all:
	// 0 makes significance an oracle for future recall, 1 makes it uniform noise. It is the axis the
	// benchmark sweeps, because it asks the question that decides adoption - how much can be told
	// about a memory when it is written, and how much does the answer matter.
	SignificanceNoise float64

	// LinkScale scales the fitted co-occurrence density down to a plausible per-memory link count.
	// The fitted figure counts every pair in a sitting and so is an upper bound, not a target.
	LinkScale float64

	// RetrievalHorizonDays is how far past the replay window a later reference may land and still
	// count as something the agent needed.
	RetrievalHorizonDays float64

	// TermsPerMemory and MemoriesPerTerm shape the topic vocabulary, and through it how hard
	// retrieval is. A query is built from a memory's topic terms rather than from its unique token,
	// so several memories compete for every question and RANKING decides which surface - an
	// exact-match query would have made retrieval a restatement of retention rather than a
	// measurement. MemoriesPerTerm is the average number of memories carrying any one term, which
	// sets the vocabulary size for a given store: the smaller it is, the more discriminating a term.
	TermsPerMemory  int
	MemoriesPerTerm int

	// QueryTerms is how many of a memory's terms a retrieval question asks with.
	QueryTerms int

	// SignificanceScale is how the [0,1] blended signal is mapped onto the significance range.
	//
	// It exists to test a specific claim about the store. Ordering by the decay value S/A^a is
	// ordering by log(S) - a*log(A), so significance enters the comparison through its LOGARITHM. A
	// linearly-spread significance is therefore compressed at the top: the gap between 20,000 and
	// 30,000 is smaller in log terms than the gap between 1,000 and 2,000, which means the most
	// important memories are the least distinguishable from each other - backwards, for a policy
	// deciding what to keep. ScaleLog spreads significance geometrically so that log(S) is uniform,
	// which is the shape the decay model implicitly expects.
	SignificanceScale SignificanceScale

	// IDPrefix is prepended to every generated memory and session id. A single run leaves it empty;
	// a long-running writer that generates trace after trace must set it per generation, or the
	// second trace's ids collide with the first's and every write becomes an update.
	IDPrefix string

	MinSignificance int32
	MaxSignificance int32
	BodyBytes       int
}

// Memory is one generated memory: what will be stored, and - known only because the whole trace is
// generated before any of it is replayed - how often it will actually be wanted.
type Memory struct {
	ID     string
	Entity int
	Agent  int
	Group  string

	At           time.Time
	Significance int32
	Token        string
	Terms        []string
	Body         string

	// Session indexes the session that created it, which becomes its event.
	Session int

	// Links are indices of other memories this one links to.
	Links []int

	// Importance is the memory's latent worth on [0,1], drawn independently of how often it is
	// accessed. It is never sent to the store; significance is the store's noisy view of it.
	Importance float64

	// Sessions is how many distinct sessions touch this memory's entity - its longevity, as
	// distinct from how often it is touched. Recalls is how many times the replay will recall it,
	// and Wanted whether it appears in the held-out retrieval set. Neither is ever sent to the store; both exist for scoring and for the
	// significance signal.
	Sessions int
	Recalls  int
	Wanted   bool
}

// Op is one replayed operation, in simulated time. Session is the session it happened during,
// carried on every op rather than only on the stores so the generated trace can be measured by the
// same fitter that produced its parameters.
type Op struct {
	At      time.Time
	Kind    OpKind
	Memory  int
	Session int
}

// Session is a contiguous run of one agent's work, stored as an event.
type Session struct {
	ID    string
	Agent int
	Group string
	Start time.Time
	End   time.Time
}

// Retrieval is one held-out question: what the agent asked for after the window closed, and which
// memory answers it.
type Retrieval struct {
	Query  string
	Needle int
	DueAt  time.Time
	Kind   RetrievalKind
}

// Trace is a complete generated workload.
type Trace struct {
	Config Config

	Start time.Time
	End   time.Time

	Memories   []Memory
	Sessions   []Session
	Ops        []Op
	Retrievals []Retrieval
}

// reference is one touch of one entity at one moment, before sessions or memories exist.
type reference struct {
	at      time.Time
	entity  int
	first   bool
	session int
}

// agentFor maps an entity onto the agent that works with it. A pure function of the index, so
// sessions can be grouped by agent before memories exist.
func (t *Trace) agentFor(entity int) int {
	return entity % t.Config.Agents
}

// Generate builds a deterministic trace. The same Config and Seed always produce the same trace,
// which is what makes a published benchmark result reproducible.
func Generate(cfg Config) (*Trace, error) {
	if err := cfg.validate(); err != nil {

		return nil, err
	}

	rng := rand.New(rand.NewSource(cfg.Seed))

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Duration(cfg.Days * float64(24*time.Hour)))
	horizon := end.Add(time.Duration(cfg.RetrievalHorizonDays * float64(24*time.Hour)))

	out := &Trace{
		Config:   cfg,
		Start:    start,
		End:      end,
		Memories: make([]Memory, cfg.Memories),
	}

	refs, wanted := out.placeReferences(rng, horizon)

	sort.Slice(refs, func(i int, j int) bool {

		return refs[i].at.Before(refs[j].at)
	})

	// Sessions before memories: significance may depend on how many distinct sessions touch an
	// entity, which is not knowable until the references have been grouped.
	out.buildSessions(refs)
	out.buildMemories(rng, refs)
	out.buildOps(refs)
	out.buildLinks(rng)
	out.buildRetrievals(rng, wanted)

	return out, nil
}

// validate rejects the configurations that would otherwise produce a trace quietly missing the
// thing it is meant to demonstrate.
func (c Config) validate() error {
	switch {

	case c.Memories <= 0:

		return fmt.Errorf("memories must be positive, got %d", c.Memories)

	case c.Days <= 0:

		return fmt.Errorf("days must be positive, got %g", c.Days)

	case c.Agents <= 0:

		return fmt.Errorf("agents must be positive, got %d", c.Agents)

	case c.SignificanceSignal != SignalFrequency && c.SignificanceSignal != SignalLongevity &&
		c.SignificanceSignal != SignalImportance:

		return fmt.Errorf("significance signal must be %q, %q or %q, got %q",
			SignalImportance, SignalFrequency, SignalLongevity, c.SignificanceSignal)

	case c.ImportanceShape != ShapeMeasured && c.ImportanceShape != ShapeGraded:

		return fmt.Errorf("importance shape must be %q or %q, got %q", ShapeMeasured, ShapeGraded, c.ImportanceShape)

	case c.SignificanceScale != ScaleLinear && c.SignificanceScale != ScaleLog:

		return fmt.Errorf("significance scale must be %q or %q, got %q", ScaleLinear, ScaleLog, c.SignificanceScale)

	case c.SignificanceScale == ScaleLog && c.MinSignificance <= 0:

		return fmt.Errorf("a log significance scale needs a positive minimum, got %d", c.MinSignificance)

	case c.MustKeepShare < 0 || c.MustKeepShare > 1:

		return fmt.Errorf("must-keep share must be within [0,1], got %g", c.MustKeepShare)

	case c.SignificanceNoise < 0 || c.SignificanceNoise > 1:

		return fmt.Errorf("significance noise must be within [0,1], got %g", c.SignificanceNoise)

	case c.TermsPerMemory <= 0 || c.MemoriesPerTerm <= 0 || c.QueryTerms <= 0:

		return fmt.Errorf("terms per memory, memories per term and query terms must all be positive")

	case c.QueryTerms > c.TermsPerMemory:

		return fmt.Errorf("query terms (%d) cannot exceed terms per memory (%d)", c.QueryTerms, c.TermsPerMemory)

	case c.MaxSignificance <= c.MinSignificance:

		return fmt.Errorf("max significance (%d) must exceed min (%d)", c.MaxSignificance, c.MinSignificance)

	case len(c.Params.Popularity.CountQuantiles) < 2:

		return fmt.Errorf("params carry no popularity ladder - refit with a current agentfit")

	case len(c.Params.Reuse.Burst.Quantiles) < 2 && len(c.Params.Reuse.Tail.Quantiles) < 2:

		return fmt.Errorf("params carry no reuse ladders - refit with a current agentfit")
	}

	return nil
}

// placeReferences runs the renewal process per entity: a first reference placed uniformly in the
// window, then successive references at gaps drawn from the fitted bimodal mixture. The first
// reference to land after the window closes but within the horizon is the entity's retrieval-set
// entry, and the walk stops there - the store is never told about it, and nothing beyond it matters.
func (t *Trace) placeReferences(rng *rand.Rand, horizon time.Time) ([]reference, map[int]time.Time) {
	pop := t.Config.Params.Popularity
	reuse := t.Config.Params.Reuse

	span := t.End.Sub(t.Start)
	refs := make([]reference, 0, t.Config.Memories*2)
	wanted := map[int]time.Time{}

	imp := t.Config.Params.Importance

	for i := 0; i < t.Config.Memories; i++ {
		// One uniform decides popularity, a second decides importance, and the fitted correlation
		// between them is applied as a small shift of the second by the first. Drawing them
		// independently would assert something the corpus can measure - and measured, they are very
		// nearly independent (r = -0.15), which is the whole reason importance is worth having.
		uPop := rng.Float64()
		uImp := clamp01(rng.Float64() + imp.PopularityCorrelation*(uPop-0.5))

		t.Memories[i].Importance = t.importanceFrom(uImp)

		count := int(math.Round(sampleLadder(pop.CountQuantiles, uPop)))
		if count < 1 {
			count = 1
		}

		at := t.Start.Add(time.Duration(rng.Float64() * float64(span)))
		refs = append(refs, reference{at: at, entity: i, first: true})

		for j := 1; j < count; j++ {
			gap := reuse.Tail.Sample(rng.Float64())

			if rng.Float64() < reuse.BurstShare {
				gap = reuse.Burst.Sample(rng.Float64())
			}

			at = at.Add(time.Duration(gap * float64(time.Hour)))

			if at.After(t.End) {
				// The first reference past the window is what the agent wanted later - but only if
				// it lands close enough to be a fair question.
				if at.Before(horizon) {
					wanted[i] = at
				}

				break
			}

			refs = append(refs, reference{at: at, entity: i})
		}
	}

	return refs, wanted
}

// importanceFrom maps a uniform draw onto a latent importance, by the configured shape.
func (t *Trace) importanceFrom(u float64) float64 {
	if t.Config.ImportanceShape == ShapeGraded {

		return u
	}

	return clamp01(sampleLadder(t.Config.Params.Importance.Quantiles, u))
}

func clamp01(v float64) float64 {
	switch {

	case v < 0:

		return 0

	case v > 1:

		return 1
	}

	return v
}

// buildMemories fills in each entity's memory. Significance is set here because it may depend on the
// recall count, which is only knowable once every reference has been placed - the generator may see
// the future, which is exactly what the store may not. Importance is already set, having been drawn
// alongside popularity.
func (t *Trace) buildMemories(rng *rand.Rand, refs []reference) {
	spread := make([]map[int]struct{}, len(t.Memories))

	for _, v := range refs {
		if spread[v.entity] == nil {
			spread[v.entity] = map[int]struct{}{}
		}

		spread[v.entity][v.session] = struct{}{}

		if v.first {
			t.Memories[v.entity].At = v.at
			t.Memories[v.entity].Session = v.session

			continue
		}

		t.Memories[v.entity].Recalls++
	}

	most, widest := 0, 0

	for i := range t.Memories {
		t.Memories[i].Sessions = len(spread[i])

		if n := t.Memories[i].Recalls; n > most {
			most = n
		}

		if n := t.Memories[i].Sessions; n > widest {
			widest = n
		}
	}

	for i := range t.Memories {
		m := &t.Memories[i]

		m.Entity = i
		m.Agent = t.agentFor(i)
		m.Group = fmt.Sprintf("agent-%02d", m.Agent)
		m.ID = fmt.Sprintf("%smem-%08d", t.Config.IDPrefix, i)
		m.Token = token(i)
		m.Terms = t.terms(rng, i)
		m.Body = body(m.Terms, m.Token, t.Config.BodyBytes)
		m.Significance = t.significance(rng, *m, most, widest)
	}
}

// vocabulary is the number of distinct topic terms, chosen so each is carried by about
// MemoriesPerTerm memories however large the store is. A fixed vocabulary would make every term
// useless at scale and every term unique at small scale.
func (t *Trace) vocabulary() int {
	n := t.Config.Memories * t.Config.TermsPerMemory / t.Config.MemoriesPerTerm

	if n < t.Config.TermsPerMemory {
		n = t.Config.TermsPerMemory
	}

	return n
}

// terms draws a memory's topic terms. Terms are drawn without replacement so a memory never repeats
// one, which would otherwise let a term's frequency inside a single body distort bm25.
func (t *Trace) terms(rng *rand.Rand, entity int) []string {
	size := t.vocabulary()
	seen := map[int]struct{}{}
	out := make([]string, 0, t.Config.TermsPerMemory)

	for len(out) < t.Config.TermsPerMemory {
		i := rng.Intn(size)

		if _, ok := seen[i]; ok {
			continue
		}

		seen[i] = struct{}{}

		out = append(out, word(i*3+11))
	}

	return out
}

// significance blends a true signal about the memory with noise, per Config.SignificanceNoise.
//
// WHICH signal is Config.SignificanceSignal, and the choice matters more than anything else in this
// package, because it decides what "significance" is taken to mean:
//
//   - SignalFrequency ties it to how many times the memory will be recalled in total. This makes
//     significance a frequency oracle, and a store leaning on it behaves like LFU - which is exactly
//     what the first runs showed, the two scoring within a point of each other.
//   - SignalLongevity ties it to how many distinct SESSIONS come back to the entity. This is the
//     better model of what a person means by significant, and it is a genuinely different signal
//     from both recency and raw frequency: a file hammered fifty times in one sitting and then
//     abandoned is not important, and frequency cannot tell the two apart.
//
// Neither is the "right" one and the benchmark reports both, because what significance predicts is a
// property of the deployment writing it, not of the store. Publishing one number would have been
// choosing the flattering half of a sensitivity analysis.
//
// The signal is damped by log1p before it is normalised because both counts are heavily skewed -
// left linear, a single popular entity would flatten every other memory's signal to nothing.
func (t *Trace) significance(rng *rand.Rand, m Memory, most int, widest int) int32 {
	signal, top := float64(m.Recalls), float64(most)

	switch t.Config.SignificanceSignal {

	case SignalImportance:
		// Already on [0,1] and not a count, so it is neither damped nor normalised.
		signal, top = m.Importance, 0

	case SignalLongevity:
		signal, top = float64(m.Sessions), float64(widest)
	}

	if top > 0 {
		signal = math.Log1p(signal) / math.Log1p(top)
	}

	blended := (1-t.Config.SignificanceNoise)*signal + t.Config.SignificanceNoise*rng.Float64()

	low, high := float64(t.Config.MinSignificance), float64(t.Config.MaxSignificance)

	if t.Config.SignificanceScale == ScaleLog {

		return int32(math.Round(low * math.Pow(high/low, blended)))
	}

	return t.Config.MinSignificance + int32(math.Round(blended*(high-low)))
}

// buildSessions chunks each agent's references into runs of the fitted per-session size. See the
// package comment for why sessions are derived by count rather than laid out in time.
func (t *Trace) buildSessions(refs []reference) {
	perAgent := map[int][]int{}

	for i, v := range refs {
		perAgent[t.agentFor(v.entity)] = append(perAgent[t.agentFor(v.entity)], i)
	}

	size := int(math.Round(t.Config.Params.Sessions.ReferencesPerSession.Mean))
	if size < 1 {
		size = 1
	}

	agents := make([]int, 0, len(perAgent))

	for k := range perAgent {
		agents = append(agents, k)
	}

	sort.Ints(agents)

	// Sessions are numbered in agent order so a seed always yields the same ids.
	for _, agent := range agents {
		indices := perAgent[agent]

		for i := 0; i < len(indices); i += size {
			last := i + size

			if last > len(indices) {
				last = len(indices)
			}

			id := len(t.Sessions)

			t.Sessions = append(t.Sessions, Session{
				ID:    fmt.Sprintf("%ssession-%06d", t.Config.IDPrefix, id),
				Agent: agent,
				Group: fmt.Sprintf("agent-%02d", agent),
				Start: refs[indices[i]].at,
				End:   refs[indices[last-1]].at,
			})

			for _, v := range indices[i:last] {
				refs[v].session = id
			}
		}
	}
}

// buildOps turns the placed references into the time-ordered operation list a replay walks.
func (t *Trace) buildOps(refs []reference) {
	t.Ops = make([]Op, 0, len(refs))

	for _, v := range refs {
		kind := OpRecall

		if v.first {
			kind = OpStore
		}

		t.Ops = append(t.Ops, Op{At: v.at, Kind: kind, Memory: v.entity, Session: v.session})
	}
}

// buildLinks relates memories created in the same session, at the fitted co-occurrence density
// scaled by LinkScale. Links are attached to the LATER memory of each pair, so a link's target
// always already exists when the replay writes it - the store rejects a link to an id it does not
// hold, and an out-of-order pair would fail the write rather than merely losing an edge.
func (t *Trace) buildLinks(rng *rand.Rand) {
	density := t.Config.Params.Links.LinksPerReference * t.Config.LinkScale
	if density <= 0 {

		return
	}

	bySession := map[int][]int{}

	for i := range t.Memories {
		bySession[t.Memories[i].Session] = append(bySession[t.Memories[i].Session], i)
	}

	sessions := make([]int, 0, len(bySession))

	for k := range bySession {
		sessions = append(sessions, k)
	}

	sort.Ints(sessions)

	for _, s := range sessions {
		members := bySession[s]

		sort.Slice(members, func(i int, j int) bool {

			return t.Memories[members[i]].At.Before(t.Memories[members[j]].At)
		})

		for i, v := range members {
			if i == 0 {
				continue
			}

			n := int(math.Round(density))

			if extra := density - math.Floor(density); rng.Float64() < extra {
				n = int(math.Floor(density)) + 1
			}

			if n > i {
				n = i
			}

			if n > maxLinksPerMemory {
				n = maxLinksPerMemory
			}

			for j := 0; j < n; j++ {
				target := members[rng.Intn(i)]

				if !contains(t.Memories[v].Links, target) {
					t.Memories[v].Links = append(t.Memories[v].Links, target)
				}
			}
		}
	}
}

// buildRetrievals assembles the held-out question set from its two halves.
//
// The next-touch half comes from the reuse process: entities whose next reference fell just past the
// window. Recency predicts these almost by definition, and a cache is built to answer them.
//
// The must-keep half is drawn by IMPORTANCE, independently of the access pattern, and asked at a
// time unrelated to when the entity was last touched. This is the question the first version of this
// benchmark never asked, and the reason it concluded a recency window was the best available policy:
// with no question that access patterns cannot predict, nothing that knows more than access patterns
// can win.
//
// The two are kept apart in the results rather than averaged, because they are different questions
// and a store may reasonably be good at one and not the other. An entity picked by both is asked
// about twice, once under each kind; excluding overlaps would bias the must-keep set toward rarely
// touched entities and overstate the effect.
func (t *Trace) buildRetrievals(rng *rand.Rand, wanted map[int]time.Time) {
	t.buildNextTouch(wanted)
	t.buildMustKeep(rng)
}

// buildMustKeep draws questions by importance, at times unrelated to the access pattern.
func (t *Trace) buildMustKeep(rng *rand.Rand) {
	target := int(t.Config.MustKeepShare * float64(len(t.Memories)))
	if target <= 0 {

		return
	}

	horizon := time.Duration(t.Config.RetrievalHorizonDays * float64(24*time.Hour))

	// Rejection sampling on importance, so an entity's chance of being asked about is proportional
	// to how much it matters and to nothing else.
	for picked, attempts := 0, 0; picked < target && attempts < 100*target; attempts++ {
		i := rng.Intn(len(t.Memories))

		if rng.Float64() > t.Memories[i].Importance {
			continue
		}

		picked++

		t.Memories[i].Wanted = true

		t.Retrievals = append(t.Retrievals, Retrieval{
			Query:  t.queryFor(i),
			Needle: i,
			DueAt:  t.End.Add(time.Duration(rng.Float64() * float64(horizon))),
			Kind:   KindMustKeep,
		})
	}
}

// queryFor is the question asked about a memory: a few of its topic terms, never its unique token.
func (t *Trace) queryFor(i int) string {
	terms := t.Memories[i].Terms

	if len(terms) > t.Config.QueryTerms {
		terms = terms[:t.Config.QueryTerms]
	}

	return strings.Join(terms, " ")
}

// buildNextTouch turns the out-of-window references into the reuse-driven half.
func (t *Trace) buildNextTouch(wanted map[int]time.Time) {
	entities := make([]int, 0, len(wanted))

	for k := range wanted {
		entities = append(entities, k)
	}

	sort.Ints(entities)

	for _, v := range entities {
		t.Memories[v].Wanted = true

		t.Retrievals = append(t.Retrievals, Retrieval{
			Query:  t.queryFor(v),
			Needle: v,
			DueAt:  wanted[v],
			Kind:   KindNextTouch,
		})
	}
}

// sampleLadder interpolates into an inverse-CDF ladder, as fit.Distribution.Sample does for
// durations. Popularity carries a bare slice rather than a Distribution, hence the duplicate.
func sampleLadder(ladder []float64, u float64) float64 {
	if len(ladder) < 2 {

		return 0
	}

	if u <= 0 {

		return ladder[0]
	}

	if u >= 1 {

		return ladder[len(ladder)-1]
	}

	pos := u * float64(len(ladder)-1)
	i := int(pos)

	return ladder[i] + (pos-float64(i))*(ladder[i+1]-ladder[i])
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {

			return true
		}
	}

	return false
}
