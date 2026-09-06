package trace

import (
	"fmt"
	"math/rand"
	"strings"
)

// The realistic renderer: the same generated trace written as an engineer's working notes instead
// of as synthetic syllables. vocabulary.go holds the terms; body.go holds the synthetic renderer
// this one sits beside.
//
// It exists for demonstrations, not for measurement. Retention is scored on whether the store still
// holds a needle, which never touches text, so it is unaffected by which renderer ran; retrieval@k
// is scored on the store's own ranking, which is BM25 over these bodies, so it IS affected. That is
// why the synthetic renderer stays the default everywhere except --live, which never scores.

// Vocabulary selects which renderer writes a memory's text.
type Vocabulary string

const (
	// VocabSynthetic is the invented, uniformly-distributed vocabulary in body.go. It is the
	// default - including when Vocabulary is left empty - because it keeps retrieval difficulty a
	// controlled parameter rather than an accident of English word frequency.
	VocabSynthetic Vocabulary = "synthetic"

	// VocabRealistic renders the same trace as engineering notes over a plausible repository.
	VocabRealistic Vocabulary = "realistic"
)

// termsPerArea is how many terms each working area carries, and is checked by the vocabulary tests.
// The realistic vocabulary's size is therefore fixed at len(workAreas) * termsPerArea, which is why
// Config.MemoriesPerTerm cannot be honoured in this mode - see effectiveMemoriesPerTerm.
const termsPerArea = 40

// dirs are the top-level directories generated paths are spread across.
var dirs = []string{"internal", "cmd", "pkg", "api"}

// stems are the file names within an area. Ordinary Go file names, so a path reads like one.
var stems = []string{
	"handler", "client", "store", "worker", "config", "router", "pool", "codec",
	"queue", "reaper", "sweeper", "cursor", "batch", "lease", "shard", "clock",
}

// verbs open a note: what the agent did to the file.
var verbs = []string{
	"traced", "reproduced", "patched", "reverted", "profiled", "instrumented",
	"reviewed", "benchmarked", "rewrote", "audited", "bisected", "annotated",
}

// symbols name a function inside the file. They are deliberately drawn from a generic pool rather
// than from the memory's own topic terms: repeating a topic term inside the body would raise its
// term frequency in exactly the document that already carries it, and quietly boost that document
// for its own query.
var symbols = []string{
	"reapExpired", "flushPending", "resolveTarget", "acquireSlot", "drainQueue",
	"mergeRanges", "sealSegment", "rotateKeys", "settleBatch", "reconcileState",
	"probeUpstream", "yieldSlot", "hydrateCache", "collapseRuns", "emitDelta",
	"claimLease",
}

// findings are the substance of a note, and take two of the memory's topic terms.
var findings = []string{
	"the %s counter never resets once %s starts",
	"%s and %s disagree about which one owns the lock",
	"under load the %s path degrades into %s",
	"%s is computed before %s is applied, so the window is off by one",
	"moving %s ahead of the %s check is what fixes it",
	"%s silently drops the %s it was handed",
	"a slow %s here stalls every %s behind it",
	"%s is retried without resetting %s, so the second attempt is wrong",
}

// asides carry a third term, so a memory's later terms are not left out of its prose entirely.
var asides = []string{
	"Worth checking whether %s has the same problem.",
	"The %s path is the one to watch next.",
	"Same root cause as the %s report.",
	"No sign of it affecting %s.",
}

// tails pad a body out to its target size. Like body.go's function-word filler they are shared
// across every memory ON PURPOSE - a sentence appearing everywhere carries almost no discriminating
// power, which is what padding should do. They are varied only so a person reading three memories
// in a row does not see the same sentence three times.
var tails = []string{
	"Confirmed against the staging replica before merging.",
	"Left a note on the issue; needs a follow-up once the migration lands.",
	"Covered by a regression test now.",
	"Reproduced twice, so it is not a flake.",
	"Same shape as the report from last week.",
	"Rolled out behind a flag while we watch it.",
	"The fix is small but the blast radius is not.",
	"Needs a second pair of eyes before it ships.",
	"Filed the follow-up rather than widening this change.",
	"The old behaviour is kept for one more release.",
	"Measured before and after; the difference holds.",
	"Nothing else in the package depends on this ordering.",
}

// mix is a deterministic integer hash (a splitmix64 finaliser). Entity indices are consecutive, and
// indexing the word lists by a radix expansion of them made consecutive memories near-identical -
// entities 0 to 3 all landed on the same area and file with only the directory changing. Hashing
// scatters them instead, and does so identically on every platform and every run.
func mix(v int) uint64 {
	x := uint64(v) + 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb

	return x ^ (x >> 31)
}

// pick indexes a list by a hashed entity and a salt, so the several choices a single memory makes
// are independent of one another.
func pick(entity int, salt int, n int) int {
	return int(mix(entity*31+salt) % uint64(n))
}

// areaFor maps an entity onto the working area it belongs to. Every choice a memory makes about
// itself - its path, its terms, its session's name - runs through this, which is what keeps a note
// about internal/auth talking about tokens rather than about compaction.
func areaFor(entity int) area {
	return workAreas[pick(entity, 1, len(workAreas))]
}

// pathFor renders the file a memory is about. The suffix only appears once an area and stem have
// been used before, so most paths read as plain file names.
func pathFor(entity int) string {
	a := areaFor(entity)
	dir := dirs[pick(entity, 2, len(dirs))]
	stem := stems[pick(entity, 3, len(stems))]
	n := pick(entity, 4, 8)

	if n == 0 {

		return fmt.Sprintf("%s/%s/%s.go", dir, a.Name, stem)
	}

	return fmt.Sprintf("%s/%s/%s%d.go", dir, a.Name, stem, n)
}

// realisticTerms draws a memory's topic terms from its OWN area, which is what makes the terms and
// the path agree. Drawn without replacement, for the same reason the synthetic renderer does: a
// repeated term inside one body distorts its term frequency.
func realisticTerms(rng *rand.Rand, entity int, n int) []string {
	pool := areaFor(entity).Terms

	if n > len(pool) {
		n = len(pool)
	}

	seen := make(map[int]struct{}, n)
	out := make([]string, 0, n)

	for len(out) < n {
		i := rng.Intn(len(pool))

		if _, ok := seen[i]; ok {
			continue
		}

		seen[i] = struct{}{}

		out = append(out, pool[i])
	}

	return out
}

// realisticBody renders the note. The finding comes first, on the same principle as the synthetic
// renderer: a truncated body still carries what a query matches on.
func realisticBody(rng *rand.Rand, entity int, topics []string, size int) string {
	if len(topics) == 0 {

		return ""
	}

	first, second := topics[0], topics[0]

	if len(topics) > 1 {
		second = topics[1]
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%s %s in %s(): %s.",
		verbs[rng.Intn(len(verbs))],
		pathFor(entity),
		symbols[pick(entity, 5, len(symbols))],
		fmt.Sprintf(findings[rng.Intn(len(findings))], first, second))

	if len(topics) > 2 {
		b.WriteString(" ")
		fmt.Fprintf(&b, asides[rng.Intn(len(asides))], topics[2])
	}

	// Padding starts at a per-memory offset so neighbouring memories do not share an opening tail.
	offset := pick(entity, 6, len(tails))

	for i := 0; b.Len() < size; i++ {
		b.WriteString(" ")
		b.WriteString(tails[(offset+i)%len(tails)])
	}

	return b.String()
}

// sessionName titles an event after the area its memories were mostly about, which is the closest
// thing the generated workload has to a subject. Ties break towards the lowest area index, so the
// name is a function of the session's contents alone.
func sessionName(group string, entities []int) string {
	if len(entities) == 0 {

		return fmt.Sprintf("%s working session", group)
	}

	counts := make(map[string]int, len(entities))
	best, top := "", 0

	for _, v := range entities {
		name := areaFor(v).Name
		counts[name]++
	}

	for _, a := range workAreas {
		if counts[a.Name] > top {
			best, top = a.Name, counts[a.Name]
		}
	}

	return fmt.Sprintf("%s: work on %s", group, best)
}

// effectiveMemoriesPerTerm reports what Config.MemoriesPerTerm actually comes out at under the
// realistic vocabulary, whose size is fixed by the list rather than by the config.
//
// Terms are drawn from a memory's own area, so with an even spread of memories across areas each
// term is carried by Memories * TermsPerMemory / (areas * termsPerArea) memories. At the agent
// defaults - 20,000 memories, 4 terms each, 20 areas of 40 - that is 100, which is exactly what
// --memories-per-term defaults to. The list was sized so the default configuration is honoured
// without the generator reusing a term; ask for a more discriminating vocabulary than 800 terms can
// serve and the terms simply spread thinner.
func effectiveMemoriesPerTerm(memories int, termsPerMemory int) int {
	size := len(workAreas) * termsPerArea

	if size == 0 {

		return 0
	}

	return memories * termsPerMemory / size
}
