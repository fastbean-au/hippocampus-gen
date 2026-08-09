package main

import (
	"regexp"

	hippo "github.com/fastbean-au/hippocampus/contract"

	"github.com/fastbean-au/hippocampus-gen/internal/link"
)

// The book's associations are the novel's own: two paragraphs that name the same character belong
// together, whether they sit side by side or twenty chapters apart, and one chapter follows the one
// before it. So each paragraph links back to the previous paragraph naming each character it
// mentions - a thread per character, running the length of the book - and each chapter links back to
// its predecessor.
//
// That produces a graph with something to say. Paragraphs about the principals are densely
// connected and decay slowly; a paragraph naming nobody is connected to nothing and is the first to
// go. Recalling a Miss Havisham paragraph with include_linked returns the Satis House scene before
// it, which is what an associative recall is for.

// character is one figure in the novel: the pattern that recognises a mention, and the weight of the
// association between two paragraphs that both carry one.
//
// Significances rank the cast by prominence rather than by any absolute scale - Estella binds a pair
// of paragraphs more tightly than Startop does. Pip is deliberately cheap: he is named throughout,
// so his thread is the one association that says almost nothing about where in the book it lands.
type character struct {
	name         string
	pattern      *regexp.Regexp
	significance int32
}

// characters are matched on whole words, so "Joe" does not fire inside another word. Magwitch's
// entry takes all three names the novel gives him, which threads his early, middle and late
// appearances together as the one man he is. Mrs. Joe has no thread of her own: every mention of her
// also names Joe, so she would ride his thread anyway, and a paragraph at the forge is about the
// household either way.
var characters = []character{
	{"Estella", regexp.MustCompile(`\bEstella\b`), 24000},
	{"Miss Havisham", regexp.MustCompile(`\bHavisham\b`), 22000},
	{"Joe Gargery", regexp.MustCompile(`\bJoe\b`), 20000},
	{"Magwitch", regexp.MustCompile(`\b(?:Magwitch|Provis|Campbell)\b`), 20000},
	{"Mr. Jaggers", regexp.MustCompile(`\bJaggers\b`), 14000},
	{"Herbert Pocket", regexp.MustCompile(`\bHerbert\b`), 12000},
	{"Biddy", regexp.MustCompile(`\bBiddy\b`), 12000},
	{"Wemmick", regexp.MustCompile(`\bWemmick\b`), 10000},
	{"Orlick", regexp.MustCompile(`\bOrlick\b`), 8000},
	{"Compeyson", regexp.MustCompile(`\bCompeyson\b`), 6000},
	{"Pumblechook", regexp.MustCompile(`\bPumblechook\b`), 6000},
	{"Bentley Drummle", regexp.MustCompile(`\bDrummle\b`), 5000},
	{"Mr. Wopsle", regexp.MustCompile(`\bWopsle\b`), 5000},
	{"Startop", regexp.MustCompile(`\bStartop\b`), 4000},
	{"Pip", regexp.MustCompile(`\bPip\b`), 4000},
}

// chapterLinkSignificance weights a chapter's link to the one before it. It sits above the middle of
// the cast: the narrative order is a stronger association than any single minor character, and an
// event's link significance is weighed into all of its memories.
const chapterLinkSignificance = 15000

// maxCharacterThreads bounds the tracker. The cast is a fixed table, so the bound is only ever a
// backstop against the map growing on a key space that cannot actually grow.
const maxCharacterThreads = 64

// newCharacterThreads returns the tracker the character threads run on. The heads never expire: a
// character's reappearance after a long absence is exactly the association worth recording, and a
// head that has been consolidated away in the meantime is handled by link.StoreMemory retrying the
// write without it.
func newCharacterThreads() *link.Threads {
	return link.NewThreads(0, maxCharacterThreads)
}

// mentioned returns the characters named in a paragraph, in table order.
func mentioned(body string) []character {
	cast := make([]character, 0, len(characters))

	for _, c := range characters {
		if !c.pattern.MatchString(body) {
			continue
		}

		cast = append(cast, c)
	}

	return cast
}

// characterLinks resolves the head of each mentioned character's thread into the links the new
// paragraph declares. A character met for the first time contributes nothing - there is nothing yet
// to point at - so the first paragraph naming anyone is always the far end of someone else's link.
//
// Several threads commonly share a head, since characters who appear together tend to keep appearing
// together, and the same target must not be named twice in one write. The links are therefore
// deduplicated, which leaves a pair of paragraphs bound at the weight of the most significant
// character they have in common.
func characterLinks(threads *link.Threads, cast []character, clock int64) []*hippo.Link {
	links := make([]*hippo.Link, 0, len(cast))

	for _, c := range cast {
		head, ok := threads.Head(c.name, clock)

		if !ok {
			continue
		}

		links = append(links, link.New(head, c.significance))
	}

	return link.Dedupe(links)
}

// advanceCharacters moves every mentioned character's thread onto the paragraph just stored, so the
// next paragraph naming them links here. An empty id (a paragraph the service did not retain) breaks
// the thread rather than linking the next paragraph across it.
func advanceCharacters(threads *link.Threads, cast []character, id string, clock int64) {
	for _, c := range cast {
		threads.Advance(c.name, id, clock)
	}
}
