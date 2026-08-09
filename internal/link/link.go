// Package link builds the associative graph the generators lay over the data they load. What
// associating means belongs to each generator - paragraphs naming the same character, log lines
// carrying the same correlation token, a chapter and the one before it - but every such scheme needs
// the same two pieces, and they live here: Threads, the bookkeeping of "what did this key last
// attach to", and the store calls that declare the resulting links.
//
// Links are declared inline on StoreMemory/StoreEvent rather than through LinkMemories/LinkEvents
// afterwards: a generator only ever links a new item back to items it has already written, so the
// targets exist by the time the write lands, and one RPC per item keeps the paced and live modes
// honest about how much load they are producing.
package link

import (
	"context"
	"math/rand"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hippo "github.com/fastbean-au/hippocampus/contract"
)

// New is one outbound edge to an already-stored item, weighted by significance. The weight is a
// client-supplied input into the decay maths - the service damps an item's summed link significance
// with log1p and scales it by consolidation.linkSignificanceWeight - so the generators choose values
// on the same scale as the significances they already assign, and it is the relative ordering
// between them, not the absolute size, that carries the meaning.
func New(id string, significance int32) *hippo.Link {
	return &hippo.Link{
		Id:           id,
		Significance: significance,
	}
}

// Dedupe collapses links naming the same target into one, keeping the highest significance declared
// for it at the position of its first appearance.
//
// Two threads legitimately arrive at the same item - a paragraph naming both Joe and Biddy follows
// the last paragraph that named them both, so both character threads have it as their head - and the
// service rejects a write declaring the same target twice rather than silently letting the last one
// win, since it upserts per pair. Collapsing is therefore the caller's job, and the strongest weight
// is the one to keep: a pair is as closely associated as its closest thread makes it.
func Dedupe(links []*hippo.Link) []*hippo.Link {
	if len(links) < 2 {

		return links
	}

	positions := make(map[string]int, len(links))
	out := make([]*hippo.Link, 0, len(links))

	for _, l := range links {
		i, seen := positions[l.GetId()]

		if !seen {
			positions[l.GetId()] = len(out)
			out = append(out, l)

			continue
		}

		if l.GetSignificance() > out[i].GetSignificance() {
			out[i] = l
		}
	}

	return out
}

// head is the most recent item recorded on one thread: the id a new member of that thread links back
// to, and the clock reading at which it was recorded.
type head struct {
	id    string
	clock int64
}

// Threads tracks the head of each association thread - the item most recently stored for a key - so
// the next item sharing that key can declare a link back to it, chaining the thread together as the
// data is written.
//
// The clock is whatever monotonically increasing measure the caller's data already has: the book
// passes memory timestamps, the log generator a line counter. Threads never reads a wall clock
// itself, so a back-dated load and a live trickle age their threads on identical terms.
type Threads struct {
	ttl   int64
	max   int
	heads map[string]head
}

// NewThreads returns a tracker holding at most max threads, each head expiring ttl clock units after
// it was recorded. A non-positive ttl never expires, for associations that stay meaningful however
// far apart their ends are (a character returning to the novel twenty chapters later); a
// non-positive max holds threads without bound, which is only safe for a key space that is bounded
// by construction.
func NewThreads(ttl int64, max int) *Threads {
	return &Threads{
		ttl:   ttl,
		max:   max,
		heads: make(map[string]head),
	}
}

// Head returns the id a new member of the thread should link back to, and whether there is one at
// all: an unknown thread, and one whose head is older than the ttl, both report false.
func (t *Threads) Head(key string, clock int64) (string, bool) {
	h, ok := t.heads[key]

	if !ok || h.id == "" {

		return "", false
	}

	if t.ttl > 0 && clock-h.clock > t.ttl {
		delete(t.heads, key)

		return "", false
	}

	return h.id, true
}

// Advance moves a thread onto a newly stored item. An empty id - an item the service did not retain,
// so nothing can point at it - drops the thread instead, rather than leaving the previous head in
// place and linking across the gap as though the missing item had never been written.
func (t *Threads) Advance(key string, id string, clock int64) {
	if id == "" {
		delete(t.heads, key)

		return
	}

	t.heads[key] = head{id: id, clock: clock}

	t.prune()
}

// Len is the number of threads currently held.
func (t *Threads) Len() int {
	return len(t.heads)
}

// prune drops the oldest head once the tracker is over its bound. One at a time is enough: Advance
// adds at most one thread per call, so the map never runs more than a single head over max.
func (t *Threads) prune() {
	if t.max <= 0 || len(t.heads) <= t.max {

		return
	}

	oldestKey := ""
	oldestClock := int64(0)

	for k, v := range t.heads {
		if oldestKey != "" && v.clock >= oldestClock {
			continue
		}

		oldestKey = k
		oldestClock = v.clock
	}

	delete(t.heads, oldestKey)
}

// Recent is a bounded ring of the ids most recently stored, for a generator whose associations carry
// no meaning beyond "these two things are connected" - the random loader, which wants a graph of
// roughly the right shape and density rather than one that says anything. Sample draws from it.
type Recent struct {
	ids  []string
	next int
	size int
}

// NewRecent returns a ring holding the last size ids. A non-positive size holds none, so Sample
// always comes back empty and no links are declared.
func NewRecent(size int) *Recent {
	if size < 0 {
		size = 0
	}

	return &Recent{
		ids:  make([]string, 0, size),
		size: size,
	}
}

// Add records a stored id, overwriting the oldest once the ring is full. An empty id (an item the
// service did not retain) is ignored - nothing can link to it.
func (r *Recent) Add(id string) {
	if id == "" || r.size == 0 {

		return
	}

	if len(r.ids) < r.size {
		r.ids = append(r.ids, id)

		return
	}

	r.ids[r.next] = id
	r.next = (r.next + 1) % r.size
}

// Sample draws up to n distinct ids at random, in no particular order. Fewer are returned when the
// ring holds fewer, and nil when it is empty or n is non-positive.
func (r *Recent) Sample(n int) []string {
	if n <= 0 || len(r.ids) == 0 {

		return nil
	}

	if n > len(r.ids) {
		n = len(r.ids)
	}

	// Partial Fisher-Yates over a copy of the indices: distinct draws without rejection sampling,
	// which degrades badly once n approaches the ring's size.
	indexes := rand.Perm(len(r.ids))[:n]

	out := make([]string, 0, n)

	for _, i := range indexes {
		out = append(out, r.ids[i])
	}

	return out
}

// StoreMemory stores m and returns the id the service assigned it, which is empty when the memory
// was not retained (significance below the configured floor).
//
// A link target can be forgotten between being recorded as a thread head and this write landing -
// that is the point of the service - and the write is then refused with NotFound for naming a target
// that no longer exists. Rather than lose the memory to a stale association, it is stored again with
// its links dropped, so the cost of an aged-out target is the association and not the data.
func StoreMemory(ctx context.Context, client hippo.HippocampusClient, m *hippo.Memory) (string, error) {
	r, err := client.StoreMemory(ctx, m)

	if err == nil {

		return r.GetId(), nil
	}

	if !retryUnlinked(err, len(m.GetLinks())) {

		return "", err
	}

	m.Links = nil

	r, err = client.StoreMemory(ctx, m)
	if err != nil {

		return "", err
	}

	return r.GetId(), nil
}

// StoreEvent is StoreMemory for events, including the same drop-the-links retry: an event whose
// predecessor has been consolidated away must still be created, or every memory that would have
// hung off it is lost too.
func StoreEvent(ctx context.Context, client hippo.HippocampusClient, e *hippo.Event) (string, error) {
	r, err := client.StoreEvent(ctx, e)

	if err == nil {

		return r.GetId(), nil
	}

	if !retryUnlinked(err, len(e.GetLinks())) {

		return "", err
	}

	e.Links = nil

	r, err = client.StoreEvent(ctx, e)
	if err != nil {

		return "", err
	}

	return r.GetId(), nil
}

// retryUnlinked reports whether a failed write is worth retrying without its links: only a NotFound
// on a write that actually declared some, since that is the one failure the links themselves can
// have caused.
func retryUnlinked(err error, links int) bool {
	return links > 0 && status.Code(err) == codes.NotFound
}
