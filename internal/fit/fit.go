package fit

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// headShare is the reference share Popularity.HeadFraction is reported against - the "80%" in "80%
// of the references come from 30% of the entities".
const headShare = 0.8

// minGapHours floors a re-reference gap before its logarithm is taken. Two references can carry the
// same timestamp (one assistant turn issuing several tool calls), and log(0) would poison the fit;
// one second is below the resolution anything downstream cares about.
const minGapHours = 1.0 / 3600.0

// Reference is one observed reference to one entity at one moment, in one session. An entity is
// whatever the agent addressed - in a Claude Code transcript, the path a tool call named.
type Reference struct {
	At      time.Time
	Session string
	Entity  string

	// Mutation records that this reference CHANGED the entity rather than merely reading it. It is
	// the corpus's one signal about importance that is not a restatement of access frequency; see
	// Importance.
	Mutation bool
}

// Observations is the raw measurement a corpus scan yields, before any distribution is fitted to
// it. Scan produces it and Fit consumes it; keeping them separate is what lets the fitting be
// tested against a hand-built observation set with no transcript files involved.
type Observations struct {
	References []Reference

	// Items counts everything a session produced that an agent might plausibly have stored - tool
	// calls and user turns alike - which is a broader set than References, since not every item
	// names an entity.
	Items map[string]int
}

// record is the subset of a transcript line this package reads. Transcript files carry many line
// types (mode, permission-mode, file-history-snapshot, ...); every field here is absent from most of
// them, which is why they are all optional and a line contributing nothing is simply skipped.
type record struct {
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"sessionId"`
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message"`
}

// message is the assistant/user turn a record carries. Content is either a plain string (a simple
// user turn) or an array of blocks, so it stays raw until its shape is known.
type message struct {
	Content json.RawMessage `json:"content"`
}

// block is one content block. Only tool_use blocks name an entity; the input keys differ per tool,
// hence both spellings.
type block struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Input struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	} `json:"input"`
}

// mutatingTools are the tool names that CHANGE the entity they name. Everything else that names a
// path is treated as a read. The set is small and explicit rather than pattern-matched, because a
// tool wrongly counted as a mutation would quietly inflate the one signal in this package that is
// not derived from access frequency.
var mutatingTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

// Scan reads every .jsonl transcript under dir and returns the references and item counts it holds.
// A prefix restricts entities to paths carrying it (empty keeps them all), which is how a fit is
// confined to one project's own files rather than counting every path the agent happened to touch.
//
// Malformed lines are skipped rather than failing the scan: these files are appended to live, so a
// truncated final line is normal and is not a reason to discard a month of history.
func Scan(dir string, prefix string) (*Observations, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {

		return nil, fmt.Errorf("globbing transcripts in %s: %w", dir, err)
	}

	if len(names) == 0 {

		return nil, fmt.Errorf("no .jsonl transcripts found in %s", dir)
	}

	out := &Observations{Items: map[string]int{}}

	for _, name := range names {
		if err := scanFile(name, prefix, out); err != nil {

			return nil, err
		}
	}

	if len(out.References) == 0 {

		return nil, fmt.Errorf("no entity references found in %s (prefix %q may be too narrow)", dir, prefix)
	}

	sort.Slice(out.References, func(i int, j int) bool {

		return out.References[i].At.Before(out.References[j].At)
	})

	return out, nil
}

// scanFile accumulates one transcript file into out. The session id is taken from the record when
// present and from the filename otherwise, since the earliest line types carry it but some do not.
func scanFile(name string, prefix string, out *Observations) error {
	data, err := os.ReadFile(name)
	if err != nil {

		return fmt.Errorf("reading %s: %w", name, err)
	}

	fallback := strings.TrimSuffix(filepath.Base(name), ".jsonl")

	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var rec record

		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}

		if rec.Timestamp == "" || len(rec.Message) == 0 {
			continue
		}

		at, err := time.Parse(time.RFC3339, rec.Timestamp)
		if err != nil {
			continue
		}

		session := rec.SessionID
		if session == "" {
			session = fallback
		}

		addTurn(rec, session, at, prefix, out)
	}

	return nil
}

// addTurn folds one message-bearing record into out: it counts as an item, and each of its tool_use
// blocks naming an in-prefix path counts as a reference.
func addTurn(rec record, session string, at time.Time, prefix string, out *Observations) {
	var msg message

	if err := json.Unmarshal(rec.Message, &msg); err != nil {

		return
	}

	var blocks []block

	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		// A string content is a plain user turn: an item that names no entity.
		if rec.Type == "user" {
			out.Items[session]++
		}

		return
	}

	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}

		out.Items[session]++

		entity := b.Input.FilePath
		if entity == "" {
			entity = b.Input.Path
		}

		if entity == "" || !strings.Contains(entity, prefix) {
			continue
		}

		out.References = append(out.References, Reference{
			At:       at,
			Session:  session,
			Entity:   entity,
			Mutation: mutatingTools[b.Name],
		})
	}
}

// Fit reduces observations to the parameter set the trace generator samples from. from describes
// the corpus for the record; at stamps the fit. References must be in time order, which Scan
// guarantees.
func (o *Observations) Fit(from string, at time.Time) Params {
	out := Params{
		FittedFrom: from,
		FittedAt:   at.UTC().Format(time.RFC3339),
	}

	out.Popularity = o.popularity()
	out.Importance = o.importance()
	out.Reuse = o.reuse()
	out.Sessions, out.Links = o.sessions()
	out.Corpus = o.corpus()

	return out
}

// corpus records the raw shape of the measurement.
func (o *Observations) corpus() Corpus {
	entities := map[string]struct{}{}
	sessions := map[string]struct{}{}

	for _, v := range o.References {
		entities[v.Entity] = struct{}{}
		sessions[v.Session] = struct{}{}
	}

	span := o.References[len(o.References)-1].At.Sub(o.References[0].At).Hours() / 24

	out := Corpus{
		Sessions:         len(sessions),
		References:       len(o.References),
		DistinctEntities: len(entities),
		SpanDays:         span,
	}

	if span > 0 {
		out.ReferencesPerDay = float64(len(o.References)) / span
	}

	return out
}

// popularity fits the entity frequency distribution. The Zipf exponent is the negated slope of a
// least-squares line through log(rank) against log(frequency), which is the standard estimator and,
// more to the point, the one a reader can reproduce from the same counts.
func (o *Observations) popularity() Popularity {
	counts := map[string]int{}

	for _, v := range o.References {
		counts[v.Entity]++
	}

	freqs := make([]int, 0, len(counts))
	once := 0

	for _, v := range counts {
		freqs = append(freqs, v)

		if v == 1 {
			once++
		}
	}

	sort.Sort(sort.Reverse(sort.IntSlice(freqs)))

	ascending := make([]float64, len(freqs))

	for i, v := range freqs {
		ascending[len(freqs)-1-i] = float64(v)
	}

	xs := make([]float64, len(freqs))
	ys := make([]float64, len(freqs))

	for i, v := range freqs {
		xs[i] = math.Log(float64(i + 1))
		ys[i] = math.Log(float64(v))
	}

	out := Popularity{
		ZipfAlpha:        -slope(xs, ys),
		OnceOnlyFraction: float64(once) / float64(len(counts)),
		HeadShare:        headShare,
		CountQuantiles:   make([]float64, QuantileSteps+1),
	}

	for i := range out.CountQuantiles {
		out.CountQuantiles[i] = percentile(ascending, float64(i)/float64(QuantileSteps))
	}

	// The smallest prefix of the ranked entities accounting for headShare of all references.
	target := float64(len(o.References)) * headShare
	running := 0.0

	for i, v := range freqs {
		running += float64(v)

		if running >= target {
			out.HeadFraction = float64(i+1) / float64(len(freqs))

			break
		}
	}

	return out
}

// importance fits the mutation signal: per entity, what share of its references changed it, and how
// far that travels with how often it is touched.
func (o *Observations) importance() Importance {
	counts := map[string]int{}
	mutations := map[string]int{}
	total := 0

	for _, v := range o.References {
		counts[v.Entity]++

		if v.Mutation {
			mutations[v.Entity]++
			total++
		}
	}

	shares := make([]float64, 0, len(counts))

	var xs, ys []float64

	never := 0

	for k, v := range counts {
		share := float64(mutations[k]) / float64(v)
		shares = append(shares, share)

		if mutations[k] == 0 {
			never++
		}

		xs = append(xs, share)
		ys = append(ys, math.Log(float64(v)))
	}

	sort.Float64s(shares)

	out := Importance{
		Mean:         mean(shares),
		NeverMutated: float64(never) / float64(len(counts)),
		Mutations:    total,
		References:   len(o.References),
		Quantiles:    make([]float64, QuantileSteps+1),
	}

	for i := range out.Quantiles {
		out.Quantiles[i] = percentile(shares, float64(i)/float64(QuantileSteps))
	}

	out.PopularityCorrelation = correlation(xs, ys)

	return out
}

// correlation is Pearson's r, zero when either sample has no spread.
func correlation(xs []float64, ys []float64) float64 {
	if len(xs) < 2 {

		return 0
	}

	mx, my := mean(xs), mean(ys)
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

// reuse fits the bimodal re-reference model. Every reference after the first to a given entity is a
// reuse; it is a burst reuse when the previous reference to that entity fell in the same session and
// a tail reuse otherwise. Splitting on the session rather than on a chosen gap threshold is what
// keeps the two modes an observation instead of a decision.
func (o *Observations) reuse() Reuse {
	type previous struct {
		at      time.Time
		session string
	}

	last := map[string]previous{}

	var bursts, tails, alls []float64

	for _, v := range o.References {
		prev, seen := last[v.Entity]
		last[v.Entity] = previous{at: v.At, session: v.Session}

		if !seen {
			continue
		}

		gap := math.Max(v.At.Sub(prev.at).Hours(), minGapHours)
		alls = append(alls, gap)

		if v.Session == prev.session {
			bursts = append(bursts, gap)

			continue
		}

		tails = append(tails, gap)
	}

	out := Reuse{
		Burst: fitDistribution(bursts),
		Tail:  fitDistribution(tails),
	}

	if n := len(bursts) + len(tails); n > 0 {
		out.BurstShare = float64(len(bursts)) / float64(n)
	}

	out.BeyondOneDay = shareBeyond(alls, 24)
	out.BeyondOneWeek = shareBeyond(alls, 24*7)

	return out
}

// sessions fits the arrival process and the co-occurrence link density in one pass, since both are
// per-session aggregates over the same grouping.
func (o *Observations) sessions() (Sessions, Links) {
	starts := map[string]time.Time{}
	ends := map[string]time.Time{}
	entities := map[string]map[string]struct{}{}

	for _, v := range o.References {
		if at, ok := starts[v.Session]; !ok || v.At.Before(at) {
			starts[v.Session] = v.At
		}

		if at, ok := ends[v.Session]; !ok || v.At.After(at) {
			ends[v.Session] = v.At
		}

		if entities[v.Session] == nil {
			entities[v.Session] = map[string]struct{}{}
		}

		entities[v.Session][v.Entity] = struct{}{}
	}

	refs := map[string]int{}

	for _, v := range o.References {
		refs[v.Session]++
	}

	var durations []float64
	var distincts, items, references []int
	var pairs float64

	for k, v := range entities {
		durations = append(durations, math.Max(ends[k].Sub(starts[k]).Hours(), minGapHours))
		distincts = append(distincts, len(v))
		items = append(items, o.Items[k])
		references = append(references, refs[k])

		n := float64(len(v))
		pairs += n * (n - 1) / 2
	}

	ordered := make([]time.Time, 0, len(starts))

	for _, v := range starts {
		ordered = append(ordered, v)
	}

	sort.Slice(ordered, func(i int, j int) bool {

		return ordered[i].Before(ordered[j])
	})

	var gaps []float64

	for i := 1; i < len(ordered); i++ {
		gaps = append(gaps, math.Max(ordered[i].Sub(ordered[i-1]).Hours(), minGapHours))
	}

	out := Sessions{
		InterArrivalHours:          fitDistribution(gaps),
		DurationHours:              fitDistribution(durations),
		ItemsPerSession:            fitDiscrete(items),
		ReferencesPerSession:       fitDiscrete(references),
		DistinctEntitiesPerSession: fitDiscrete(distincts),
	}

	links := Links{}

	if len(entities) > 0 {
		links.CooccurrencePairsPerSession = pairs / float64(len(entities))
	}

	// Normalised per reference because the generator decides links per memory written, not per
	// session. It is an upper bound on a plausible link count - every pair in a sitting is counted -
	// so the generator scales it down and clamps to the contract's per-item limit rather than taking
	// it literally.
	if len(o.References) > 0 {
		links.LinksPerReference = pairs / float64(len(o.References))
	}

	return out, links
}

// fitDistribution takes the mean and standard deviation of the logs, the empirical percentiles
// alongside them so a poor fit is visible by comparison rather than having to be trusted, and the
// inverse-CDF ladder the generator samples from.
func fitDistribution(samples []float64) Distribution {
	out := Distribution{Samples: len(samples)}

	if len(samples) == 0 {

		return out
	}

	logs := make([]float64, len(samples))

	for i, v := range samples {
		logs[i] = math.Log(v)
	}

	out.MeanLog = mean(logs)
	out.StdDevLog = stdDev(logs, out.MeanLog)

	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)

	out.MedianHours = percentile(sorted, 0.5)
	out.P90Hours = percentile(sorted, 0.9)
	out.P99Hours = percentile(sorted, 0.99)

	out.Quantiles = make([]float64, QuantileSteps+1)

	for i := range out.Quantiles {
		out.Quantiles[i] = percentile(sorted, float64(i)/float64(QuantileSteps))
	}

	return out
}

// fitDiscrete summarises a count-valued sample.
func fitDiscrete(samples []int) Discrete {
	out := Discrete{Samples: len(samples)}

	if len(samples) == 0 {

		return out
	}

	sorted := append([]int(nil), samples...)
	sort.Ints(sorted)

	floats := make([]float64, len(sorted))

	for i, v := range sorted {
		floats[i] = float64(v)
	}

	out.Mean = mean(floats)
	out.StdDev = stdDev(floats, out.Mean)
	out.Min = sorted[0]
	out.Max = sorted[len(sorted)-1]
	out.Median = sorted[(len(sorted)-1)/2]

	return out
}

// shareBeyond is the fraction of samples exceeding hours - the share of reuses a recency window of
// that width would have missed.
func shareBeyond(samples []float64, hours float64) float64 {
	if len(samples) == 0 {

		return 0
	}

	n := 0

	for _, v := range samples {
		if v > hours {
			n++
		}
	}

	return float64(n) / float64(len(samples))
}

// slope is the least-squares gradient of ys against xs, zero when it is not determined.
func slope(xs []float64, ys []float64) float64 {
	if len(xs) < 2 {

		return 0
	}

	mx, my := mean(xs), mean(ys)
	num, den := 0.0, 0.0

	for i, x := range xs {
		num += (x - mx) * (ys[i] - my)
		den += (x - mx) * (x - mx)
	}

	if den == 0 {

		return 0
	}

	return num / den
}

func mean(samples []float64) float64 {
	if len(samples) == 0 {

		return 0
	}

	total := 0.0

	for _, v := range samples {
		total += v
	}

	return total / float64(len(samples))
}

func stdDev(samples []float64, m float64) float64 {
	if len(samples) < 2 {

		return 0
	}

	total := 0.0

	for _, v := range samples {
		total += (v - m) * (v - m)
	}

	return math.Sqrt(total / float64(len(samples)-1))
}

// percentile indexes into an already-sorted sample.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {

		return 0
	}

	i := int(p * float64(len(sorted)-1))

	return sorted[i]
}
