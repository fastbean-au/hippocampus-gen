package observer

import (
	"strings"
	"testing"
)

func TestBuildPromptCarriesMaterialAndWhatIsAlreadyKnown(t *testing.T) {
	got := BuildPrompt([]string{"a flood in Queensland"}, []string{"the river was already high"})

	for _, want := range []string{"a flood in Queensland", "the river was already high", "IMPORTANCE", "NOTE"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}

	// The recalled notes exist to stop the agent restating what it already holds, so the prompt has
	// to say that in words rather than merely include them.
	if !strings.Contains(got, "Do not repeat") {
		t.Error("the prompt includes what is already known without telling the model what to do with it")
	}

	// Every band is spelled out: a small model asked for a bare 1-to-5 returns the middle every time.
	for i := MinRating; i <= MaxRating; i++ {
		if !strings.Contains(got, ratingLabels[i]) {
			t.Errorf("rating band %d is not described", i)
		}
	}
}

func TestBuildPromptWithNothingRecalled(t *testing.T) {
	got := BuildPrompt([]string{"something happened"}, nil)

	if strings.Contains(got, "You already noted") {
		t.Error("an agent with no prior notes should not be shown an empty list of them")
	}
}

// TestParseResponseHandlesWhatASmallModelActuallyReturns is the point of this package. The reply is
// untrusted output from a weak model; every shape below was worth handling rather than discarding a
// usable observation over.
func TestParseResponseHandlesWhatASmallModelActuallyReturns(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantNote   string
		wantRating int
	}{
		{
			name:       "the requested form",
			raw:        "IMPORTANCE: 4\nNOTE: The dam released water ahead of the peak.",
			wantNote:   "The dam released water ahead of the peak.",
			wantRating: 4,
		},
		{
			name:       "reordered",
			raw:        "NOTE: Rates were held.\nIMPORTANCE: 2",
			wantNote:   "Rates were held.",
			wantRating: 2,
		},
		{
			name:       "markdown decoration",
			raw:        "**IMPORTANCE:** 5\n**NOTE:** A government fell.",
			wantNote:   "A government fell.",
			wantRating: 5,
		},
		{
			name:       "chatter before the answer",
			raw:        "Sure! Here is my response:\n\nIMPORTANCE: 3\nNOTE: Two outlets covered the same strike.",
			wantNote:   "Two outlets covered the same strike.",
			wantRating: 3,
		},
		{
			name:       "no rating at all falls to the middle band",
			raw:        "NOTE: Something happened.",
			wantNote:   "Something happened.",
			wantRating: 3,
		},
		{
			name:       "no labels at all - the last line is the note",
			raw:        "I think the important thing is that the border reopened.",
			wantNote:   "I think the important thing is that the border reopened.",
			wantRating: 3,
		},
		{
			name:       "a rating outside the scale is folded onto it",
			raw:        "IMPORTANCE: 97\nNOTE: Everything is important.",
			wantNote:   "Everything is important.",
			wantRating: 5,
		},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			got, err := ParseResponse(v.raw)
			if err != nil {
				t.Fatalf("ParseResponse: %v", err)
			}

			if got.Note != v.wantNote {
				t.Errorf("note: got %q, want %q", got.Note, v.wantNote)
			}

			if got.Rating != v.wantRating {
				t.Errorf("rating: got %d, want %d", got.Rating, v.wantRating)
			}
		})
	}
}

func TestParseResponseRejectsAnEmptyReply(t *testing.T) {
	// The one unrecoverable case: there is nothing to store.
	for _, raw := range []string{"", "   \n\n  ", "NOTE:"} {
		if _, err := ParseResponse(raw); err == nil {
			t.Errorf("expected an error for %q", raw)
		}
	}
}

// TestSignificanceIsSpreadGeometrically pins the mapping against the service's own guidance: every
// decay method divides significance by a function of age, so significance is compared as a RATIO.
// An evenly spread scale would leave the top bands - the ones the agent judged most important - the
// least distinguishable from each other.
func TestSignificanceIsSpreadGeometrically(t *testing.T) {
	const (
		low  int32 = 1000
		high int32 = 81000
	)

	got := make([]int32, 0, MaxRating)

	for r := MinRating; r <= MaxRating; r++ {
		got = append(got, SignificanceFor(r, low, high))
	}

	if got[0] != low {
		t.Errorf("the lowest band should land on the floor, got %d", got[0])
	}

	if got[len(got)-1] != high {
		t.Errorf("the highest band should land on the ceiling, got %d", got[len(got)-1])
	}

	// Each step multiplies by a constant, so consecutive ratios are equal - which is what "spread by
	// ratio" means, and what an even spread would not satisfy.
	for i := 1; i+1 < len(got); i++ {
		a := float64(got[i]) / float64(got[i-1])
		b := float64(got[i+1]) / float64(got[i])

		if diff := a - b; diff > 0.01 || diff < -0.01 {
			t.Errorf("bands %d..%d are not geometrically spread: ratios %.3f then %.3f", i-1, i+1, a, b)
		}
	}
}

func TestSignificanceForHandlesDegenerateRanges(t *testing.T) {
	if got := SignificanceFor(3, 5000, 5000); got != 5000 {
		t.Errorf("a collapsed range should return the floor, got %d", got)
	}

	if got := SignificanceFor(3, 0, 100); got < 1 {
		t.Errorf("a zero floor must not produce a non-positive significance, got %d", got)
	}
}
