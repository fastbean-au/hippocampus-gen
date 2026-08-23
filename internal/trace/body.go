package trace

import (
	"strconv"
	"strings"
)

// syllables build the synthetic vocabulary. They are consonant-vowel pairs so the generated words
// are pronounceable and a person reading a memory in the console sees something word-shaped rather
// than a hash. The set is deliberately plain ASCII: every word must survive both search backends'
// tokenisers unchanged, and SQLite's FTS5 unicode61 and OpenSearch's standard analyser agree only
// on alphanumerics.
var syllables = []string{
	"ka", "re", "to", "mi", "lan", "sor", "vel", "dur", "nim", "tas",
	"pel", "gor", "ith", "mor", "sel", "bar", "cun", "dal", "fen", "hes",
	"jor", "kel", "lom", "nar", "oth", "pri", "quon", "ral", "sim", "tor",
	"ulm", "ven", "wyr", "xan", "yel", "zor", "ard", "bek", "cil", "dov",
}

// fillers pad a body out to its target size. They are ordinary English function words so a body
// reads like prose and, more usefully, so they are the same in every memory - a term that appears
// everywhere carries no discriminating power, which is precisely what padding should do.
var fillers = []string{
	"the", "and", "for", "with", "from", "that", "this", "when", "into", "over",
	"after", "before", "while", "under", "against", "between", "during", "without",
}

// word maps an index onto a unique pronounceable word. The mapping is a base-len(syllables)
// expansion, so distinct indices always give distinct words however large the vocabulary grows.
func word(i int) string {
	if i < 0 {
		i = -i
	}

	var b strings.Builder

	for {
		b.WriteString(syllables[i%len(syllables)])

		i /= len(syllables)

		if i == 0 {

			break
		}
	}

	return b.String()
}

// token is a memory's own unique term. It carries the entity index, so it is unique by construction
// and a memory can always be found exactly - which is what the replay's own sanity check uses,
// though the benchmark's queries deliberately do not, since an exact-match query would make
// retrieval a restatement of retention rather than a measurement of ranking.
func token(entity int) string {
	return word(entity%len(syllables)*7+3) + strconv.Itoa(entity)
}

// body renders a memory's text: its topic terms, its unique token, and enough filler to reach the
// target size. Terms come first so a truncated body still carries what a query matches on.
func body(topics []string, own string, size int) string {
	var b strings.Builder

	b.WriteString("note ")
	b.WriteString(own)

	for _, v := range topics {
		b.WriteString(" ")
		b.WriteString(v)
	}

	i := 0

	for b.Len() < size {
		b.WriteString(" ")
		b.WriteString(fillers[i%len(fillers)])

		i++
	}

	return b.String()
}
