package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	hippo "github.com/fastbean-au/hippocampus/contract"
)

// playPortion is one curated span of the W.S. Gilbert stage adaptation (gtexp.txt) together with the
// inclusive range of novel chapters it condenses. The adaptation retells the same story as the
// novel across a Prologue and three acts, so a handful of its scenes serve as ready-made summaries
// for contiguous runs of chapters. startAnchor/endAnchor are verbatim, unique substrings bounding
// the span in the raw play text; the text between them (endAnchor excluded) becomes the summary
// body.
type playPortion struct {
	firstChapter int
	lastChapter  int
	startAnchor  string
	endAnchor    string
}

// playPortions map the novel's 59 chapters onto eight scenes of the adaptation. The ranges are
// contiguous and cover every chapter, so any summarization candidate resolves to exactly one
// portion. Anchors are lines lifted verbatim from gtexp.txt and were each confirmed to appear
// exactly once.
var playPortions = []playPortion{
	{1, 3, "SCENE. Exterior of Village Churchyard.", "Magwitch is seen in the Churchyard"},
	{4, 7, "Magwitch is seen in the Churchyard", "END OF PROLOGUE."},
	{8, 15, "Ten years have elapsed since the Prologue", "Enter Mr. Jaggers"},
	{16, 19, "Enter Mr. Jaggers", "ACT DROP"},
	{20, 33, "Herbert Pocket discovered", "Magwitch appears at the door"},
	{34, 39, "Magwitch appears at the door", "END OF ACT 2nd."},
	{40, 52, "Enter Pip and Clerk", "SCENE 2nd."},
	{53, 59, "INTERIOR OF SLUICE HOUSE.", "Notes on the typescript:"},
}

// pageNumberRegex matches a line that is nothing but digits — a page number or a song-verse number
// in the raw typescript — so those can be dropped when a portion is cleaned into a summary.
var pageNumberRegex = regexp.MustCompile(`^\d+$`)

// romanValues gives each Roman numeral letter its value, for turning a "Chapter XIV." heading back
// into an ordinal so the right play portion can be selected.
var romanValues = map[byte]int{'I': 1, 'V': 5, 'X': 10, 'L': 50, 'C': 100, 'D': 500, 'M': 1000}

// summarize condenses ripe events using the stage-play adaptation. It nudges the service to run a
// consolidation cycle now, asks which events it considers ready to summarise, and for each replaces
// all of that event's memories with a single summary memory drawn from the matching play portion.
func summarize(client hippo.HippocampusClient) {
	ctx := context.Background()

	// Trigger a consolidation cycle now so the candidate list reflects the book we just loaded
	// rather than a stale snapshot from an earlier cycle.
	//
	// A failure here is a warning, not the end of the pass. The cycle is an optimisation - it makes
	// the candidate list current - and the list is still worth asking for without it. Sleep is also
	// the one call here a group-scoped token cannot make (it acts on the whole store, so the service
	// refuses it whatever the tier), and aborting would have made the whole summarisation pass
	// unavailable to a scoped token over a call it does not actually depend on. If the service is
	// genuinely unreachable, the next call says so.
	if _, err := client.Sleep(ctx, &hippo.EmptyRequest{}); err != nil {
		fmt.Printf("WARNING could not trigger a consolidation cycle, continuing with the candidate list as it stands: %s\n", err.Error())
	}

	resp, err := client.GetSummarisationCandidates(ctx, &hippo.EmptyRequest{})
	if err != nil {
		fmt.Printf("ERROR getting summarisation candidates: %s\n", err.Error())

		return
	}

	candidates := resp.GetCandidates()
	if len(candidates) == 0 {
		fmt.Println("No summarization candidates returned; check consolidation.summarizationMinMemories on the server.")

		return
	}

	play := string(playData)

	for _, candidate := range candidates {
		chapter := chapterNumber(candidate.GetEventName())

		if chapter == 0 {
			fmt.Printf("SKIP %q: cannot determine chapter from name %q\n", candidate.GetEventId(), candidate.GetEventName())

			continue
		}

		body := summaryForChapter(play, chapter)

		if body == "" {
			fmt.Printf("SKIP %q: no play portion for chapter %d\n", candidate.GetEventId(), chapter)

			continue
		}

		req := &hippo.ReplaceMemoriesWithSummaryRequest{
			EventId: candidate.GetEventId(),
			Summary: &hippo.Memory{
				EventId:      candidate.GetEventId(),
				Significance: randomSignificance(),
				Body:         body,
			},
		}

		r, err := client.ReplaceMemoriesWithSummary(ctx, req)

		if err != nil {
			fmt.Printf("ERROR summarizing event %q: %s\n", candidate.GetEventId(), err.Error())

			continue
		}

		fmt.Printf("Summarized %s (%s): replaced %d memories\n", candidate.GetEventName(), candidate.GetEventId(), r.GetMemoriesReplaced())
	}
}

// summaryForChapter returns the cleaned play text of the portion covering the given chapter, or an
// empty string when no portion does.
func summaryForChapter(play string, chapter int) string {
	body := ""

	for _, portion := range playPortions {
		if chapter < portion.firstChapter || chapter > portion.lastChapter {
			continue
		}

		body = portionText(play, portion)

		break
	}

	return body
}

// portionText extracts the raw play text between a portion's anchors and cleans it into a single
// summary paragraph. If the end anchor is missing the text runs to the end of the play; if the
// start anchor is missing the portion yields an empty string.
func portionText(play string, portion playPortion) string {
	start := strings.Index(play, portion.startAnchor)

	if start < 0 {
		return ""
	}

	rest := play[start:]

	end := strings.Index(rest, portion.endAnchor)

	if end >= 0 {
		rest = rest[:end]
	}

	return cleanPlayText(rest)
}

// cleanPlayText turns a span of the raw OCR typescript into a single readable line: trailing and
// leading whitespace is stripped from every line, blank lines and bare page/verse numbers are
// dropped, and what remains is joined with single spaces.
func cleanPlayText(in string) string {
	lines := strings.Split(in, "\n")
	out := make([]string, 0, len(lines))

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			continue
		}

		if pageNumberRegex.MatchString(trimmed) {
			continue
		}

		out = append(out, trimmed)
	}

	return strings.Join(out, " ")
}

// chapterNumber turns a "Chapter XIV." event name into its ordinal (14), or 0 when the name is not
// a recognised chapter heading.
func chapterNumber(name string) int {
	s := ChapterRegex.FindStringSubmatch(name)

	if s == nil {
		return 0
	}

	return fromRoman(s[1])
}

// fromRoman converts an uppercase Roman numeral to its integer value.
func fromRoman(in string) int {
	total := 0
	prev := 0

	for i := len(in) - 1; i >= 0; i-- {
		v := romanValues[in[i]]

		if v < prev {
			total -= v

			continue
		}

		total += v
		prev = v
	}

	return total
}
