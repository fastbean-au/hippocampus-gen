package main

import (
	"strings"
	"testing"
)

func TestFromRoman(t *testing.T) {
	cases := map[string]int{
		"I":    1,
		"IV":   4,
		"IX":   9,
		"XIV":  14,
		"XL":   40,
		"LVII": 57,
		"LIX":  59,
	}

	for in, want := range cases {
		if got := fromRoman(in); got != want {
			t.Errorf("fromRoman(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestChapterNumber(t *testing.T) {
	if got := chapterNumber("Chapter XIV."); got != 14 {
		t.Errorf("chapterNumber(\"Chapter XIV.\") = %d, want 14", got)
	}

	if got := chapterNumber("not a chapter"); got != 0 {
		t.Errorf("chapterNumber of non-heading = %d, want 0", got)
	}
}

func TestCleanPlayText(t *testing.T) {
	in := "  first line   \n\n42\n  second line \n"

	if got := cleanPlayText(in); got != "first line second line" {
		t.Errorf("cleanPlayText = %q, want %q", got, "first line second line")
	}
}

// TestPlayPortionsCoverAllChapters confirms the curated ranges are contiguous and cover every
// chapter from 1 to 59 with no gaps or overlaps, so any candidate resolves to exactly one portion.
func TestPlayPortionsCoverAllChapters(t *testing.T) {
	next := 1

	for _, portion := range playPortions {
		if portion.firstChapter != next {
			t.Fatalf("portion for chapters %d-%d does not start at %d", portion.firstChapter, portion.lastChapter, next)
		}

		if portion.lastChapter < portion.firstChapter {
			t.Fatalf("portion for chapters %d-%d is inverted", portion.firstChapter, portion.lastChapter)
		}

		next = portion.lastChapter + 1
	}

	if next != 60 {
		t.Fatalf("portions cover chapters 1-%d, want 1-59", next-1)
	}
}

// TestSummaryForEveryChapter drives the real embedded play text: every chapter must resolve to a
// non-empty summary, proving all anchors are present and correctly ordered in gtexp.txt.
func TestSummaryForEveryChapter(t *testing.T) {
	play := string(playData)

	for chapter := 1; chapter <= 59; chapter++ {
		body := summaryForChapter(play, chapter)

		if body == "" {
			t.Errorf("chapter %d produced an empty summary", chapter)
		}

		if strings.Contains(body, "\n") {
			t.Errorf("chapter %d summary was not collapsed to a single line", chapter)
		}
	}
}

// TestPortionTextIsBounded checks that a portion stops at its end anchor rather than running on: the
// first portion must not contain text that belongs to a later scene.
func TestPortionTextIsBounded(t *testing.T) {
	play := string(playData)

	first := portionText(play, playPortions[0])

	if strings.Contains(first, "END OF PROLOGUE") {
		t.Errorf("first portion ran past its end anchor into later text")
	}

	if !strings.Contains(first, "Churchyard") {
		t.Errorf("first portion is missing expected churchyard text")
	}
}
