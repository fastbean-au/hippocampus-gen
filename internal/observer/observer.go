// Package observer holds the judgement an LLM-backed agent applies to what it reads: the prompt it
// is given, the reply it is expected to produce, and how the rating in that reply becomes a stored
// significance.
//
// It is separated from the command that runs the loop so all of it can be tested without a model or
// a service - the parsing especially, which is the part that meets untrusted output. A small model
// asked for a fixed shape will sometimes not produce it, and everything here is built on the
// assumption that the reply is a suggestion rather than a contract.
package observer

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Ratings are the importance bands the model is asked to choose between. Five is few enough that a
// small model picks between them consistently, and the labels are what the prompt shows it - a
// number alone invites the middle every time.
const (
	MinRating = 1
	MaxRating = 5
)

var ratingLabels = map[int]string{
	1: "trivia - true today, worthless next week",
	2: "routine - ordinary news, no lasting consequence",
	3: "notable - worth remembering for a while",
	4: "significant - changes how later news should be read",
	5: "landmark - would still matter in a year",
}

// Observation is what one cycle produced: a note and the model's own judgement of how much it
// matters. Sources are the ids of the memories that prompted it, carried into metadata so the note
// can be traced back to what it was formed from - they are in another store, so they cannot be links.
type Observation struct {
	Note    string
	Rating  int
	Sources []string
}

// BuildPrompt asks for one short observation over the supplied material, given what the agent
// already believes.
//
// Two things in it are load-bearing. The recalled notes are included so the agent does not restate
// what it already holds - the whole point of giving an agent a memory - and the rating scale is
// spelled out with consequences rather than as bare numbers, because a small model asked for "1 to
// 5" returns 3 almost every time.
func BuildPrompt(material []string, recalled []string) string {
	var b strings.Builder

	b.WriteString("You are keeping a running record of what matters in a news feed.\n\n")

	if len(recalled) > 0 {
		b.WriteString("You already noted:\n")

		for _, v := range recalled {
			b.WriteString("- ")
			b.WriteString(v)
			b.WriteString("\n")
		}

		b.WriteString("\nDo not repeat those. Add something they do not already say.\n\n")
	}

	b.WriteString("New headlines:\n")

	for _, v := range material {
		b.WriteString("- ")
		b.WriteString(v)
		b.WriteString("\n")
	}

	b.WriteString("\nWrite ONE sentence recording what is worth remembering, then rate how much it matters:\n")

	for i := MinRating; i <= MaxRating; i++ {
		fmt.Fprintf(&b, "  %d = %s\n", i, ratingLabels[i])
	}

	b.WriteString("\nReply in exactly this form and nothing else:\nIMPORTANCE: <number>\nNOTE: <one sentence>\n")

	return b.String()
}

// ParseResponse reads the model's reply.
//
// Deliberately forgiving about everything except the note itself. A small model prefixes its answer
// with chatter, wraps it in markdown, renames the fields, or omits the rating; none of that is worth
// discarding a usable observation over, so the rating falls back to the middle band and the note is
// taken from a NOTE: line if there is one and from the last non-empty line if there is not. An empty
// note is the one unrecoverable case, because there is nothing to store.
func ParseResponse(raw string) (Observation, error) {
	var out Observation

	out.Rating = 0

	var fallback string

	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "*#`-"))

		if trimmed == "" {
			continue
		}

		upper := strings.ToUpper(trimmed)

		switch {

		case strings.HasPrefix(upper, "IMPORTANCE"):
			out.Rating = firstNumber(trimmed)

		case strings.HasPrefix(upper, "NOTE"):
			// Trimmed again after the label: decoration wraps the field name as well as the line,
			// so "**NOTE:** text" leaves a "**" on the value that the line-level trim cannot reach.
			out.Note = strings.TrimSpace(strings.Trim(
				strings.TrimSpace(strings.TrimPrefix(trimmed[len("NOTE"):], ":")), "*#`\"'"))

		default:
			fallback = trimmed
		}
	}

	if out.Note == "" {
		out.Note = fallback
	}

	if out.Note == "" {

		return out, fmt.Errorf("no usable note in the model's reply")
	}

	out.Rating = clampRating(out.Rating)

	return out, nil
}

// firstNumber pulls the first run of digits out of a line, or zero when there is none.
func firstNumber(s string) int {
	start := -1

	for i, r := range s {
		if r >= '0' && r <= '9' {
			if start < 0 {
				start = i
			}

			continue
		}

		if start >= 0 {
			n, _ := strconv.Atoi(s[start:i])

			return n
		}
	}

	if start >= 0 {
		n, _ := strconv.Atoi(s[start:])

		return n
	}

	return 0
}

// clampRating folds anything outside the scale onto it, and an absent rating onto the middle band -
// which is the honest default when the model declined to judge.
func clampRating(rating int) int {
	switch {

	case rating < MinRating:

		return (MinRating + MaxRating) / 2

	case rating > MaxRating:

		return MaxRating
	}

	return rating
}

// SignificanceFor maps a rating onto a stored significance, spread GEOMETRICALLY across the range.
//
// Evenly spaced values would be wrong, and quietly so. Every decay method divides significance by a
// function of age, so significance is compared as a ratio: on an even scale the gap between the top
// two bands is far smaller than between the bottom two, and the memories the agent judged most
// important would be the ones the store could least tell apart. See the service's
// docs/consolidation.md, "Choosing significance values".
func SignificanceFor(rating int, low int32, high int32) int32 {
	rating = clampRating(rating)

	if low < 1 {
		low = 1
	}

	if high <= low {

		return low
	}

	// rating 1 lands on low, rating MaxRating on high, and the rest on a geometric progression
	// between them.
	step := float64(rating-MinRating) / float64(MaxRating-MinRating)

	return int32(math.Round(float64(low) * math.Pow(float64(high)/float64(low), step)))
}
