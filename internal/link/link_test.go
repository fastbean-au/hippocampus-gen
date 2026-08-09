package link

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hippo "github.com/fastbean-au/hippocampus/contract"
)

// fakeClient is a minimal HippocampusClient recording the store calls made through it, and able to
// fail the first of them. Embedding the interface means only the two RPCs this package calls need
// implementing.
type fakeClient struct {
	hippo.HippocampusClient

	memories []*hippo.Memory
	events   []*hippo.Event
	failNext error
}

func (f *fakeClient) StoreMemory(_ context.Context, m *hippo.Memory, _ ...grpc.CallOption) (*hippo.StoreMemoryResponse, error) {
	f.memories = append(f.memories, m)

	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil

		return nil, err
	}

	return &hippo.StoreMemoryResponse{Id: "m-1"}, nil
}

func (f *fakeClient) StoreEvent(_ context.Context, e *hippo.Event, _ ...grpc.CallOption) (*hippo.StoreEventResponse, error) {
	f.events = append(f.events, e)

	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil

		return nil, err
	}

	return &hippo.StoreEventResponse{Id: "ev-1"}, nil
}

func TestThreadsHeadAndAdvance(t *testing.T) {
	threads := NewThreads(0, 8)

	if _, ok := threads.Head("estella", 1); ok {
		t.Fatal("expected no head for an unseen thread")
	}

	threads.Advance("estella", "m-1", 1)

	head, ok := threads.Head("estella", 100)

	if !ok || head != "m-1" {
		t.Fatalf("expected head m-1, got %q (ok=%t)", head, ok)
	}
}

func TestThreadsHeadExpires(t *testing.T) {
	threads := NewThreads(10, 8)

	threads.Advance("req-1", "m-1", 100)

	if _, ok := threads.Head("req-1", 110); !ok {
		t.Fatal("expected a head exactly at the ttl boundary")
	}

	if _, ok := threads.Head("req-1", 111); ok {
		t.Fatal("expected the head to have expired one unit past the ttl")
	}

	if threads.Len() != 0 {
		t.Fatalf("expected the expired thread to be dropped, %d remain", threads.Len())
	}
}

func TestThreadsAdvanceWithoutIdBreaksThread(t *testing.T) {
	threads := NewThreads(0, 8)

	threads.Advance("joe", "m-1", 1)

	// A memory the service did not retain: the thread must break rather than leaving m-1 in place for
	// the next paragraph to link across the gap to.
	threads.Advance("joe", "", 2)

	if _, ok := threads.Head("joe", 3); ok {
		t.Fatal("expected the thread to be broken by an unretained item")
	}
}

func TestThreadsPruneOldest(t *testing.T) {
	threads := NewThreads(0, 2)

	threads.Advance("a", "m-1", 1)
	threads.Advance("b", "m-2", 2)
	threads.Advance("c", "m-3", 3)

	if threads.Len() != 2 {
		t.Fatalf("expected the tracker to hold its bound of 2, got %d", threads.Len())
	}

	if _, ok := threads.Head("a", 4); ok {
		t.Fatal("expected the oldest thread to have been pruned")
	}

	for _, key := range []string{"b", "c"} {
		if _, ok := threads.Head(key, 4); !ok {
			t.Fatalf("expected thread %q to survive pruning", key)
		}
	}
}

func TestRecentSample(t *testing.T) {
	recent := NewRecent(3)

	if got := recent.Sample(2); got != nil {
		t.Fatalf("expected no sample from an empty ring, got %v", got)
	}

	for _, id := range []string{"m-1", "m-2", "m-3", "m-4"} {
		recent.Add(id)
	}

	// The ring holds three, so a request for five comes back with three distinct ids, and the
	// overwritten m-1 is not among them.
	got := recent.Sample(5)

	if len(got) != 3 {
		t.Fatalf("expected 3 ids from a ring of 3, got %d", len(got))
	}

	seen := map[string]bool{}

	for _, id := range got {
		if seen[id] {
			t.Fatalf("expected distinct ids, %q repeated", id)
		}

		if id == "m-1" {
			t.Fatal("expected the oldest id to have been overwritten")
		}

		seen[id] = true
	}
}

func TestRecentIgnoresUnstoredId(t *testing.T) {
	recent := NewRecent(4)

	recent.Add("")

	if got := recent.Sample(1); got != nil {
		t.Fatalf("expected an empty id to be ignored, got %v", got)
	}
}

func TestDedupeKeepsStrongestPerTarget(t *testing.T) {
	links := Dedupe([]*hippo.Link{
		New("m-1", 4000),
		New("m-2", 12000),
		New("m-1", 20000),
		New("m-1", 6000),
	})

	if len(links) != 2 {
		t.Fatalf("expected two distinct targets, got %d", len(links))
	}

	// First-appearance order is kept, so m-1 stays ahead of m-2 despite its strongest weight arriving
	// third.
	if links[0].GetId() != "m-1" || links[1].GetId() != "m-2" {
		t.Fatalf("expected first-appearance order, got %q then %q", links[0].GetId(), links[1].GetId())
	}

	if links[0].GetSignificance() != 20000 {
		t.Fatalf("expected the strongest weight for m-1, got %d", links[0].GetSignificance())
	}
}

func TestDedupeLeavesDistinctLinksAlone(t *testing.T) {
	in := []*hippo.Link{New("m-1", 100), New("m-2", 200)}

	if got := Dedupe(in); len(got) != 2 {
		t.Fatalf("expected both links to survive, got %d", len(got))
	}
}

func TestDedupeHandlesEmpty(t *testing.T) {
	if got := Dedupe(nil); len(got) != 0 {
		t.Fatalf("expected nothing from no links, got %d", len(got))
	}
}

func TestStoreMemoryRetriesWithoutLinksOnNotFound(t *testing.T) {
	f := &fakeClient{failNext: status.Error(codes.NotFound, "link target 'gone' does not exist")}

	m := &hippo.Memory{
		Body:  "a paragraph",
		Links: []*hippo.Link{New("gone", 100)},
	}

	id, err := StoreMemory(context.Background(), f, m)

	if err != nil {
		t.Fatalf("expected the retry to store the memory, got %s", err.Error())
	}

	if id != "m-1" {
		t.Fatalf("expected the stored id, got %q", id)
	}

	if len(f.memories) != 2 {
		t.Fatalf("expected two attempts, got %d", len(f.memories))
	}

	if len(f.memories[1].GetLinks()) != 0 {
		t.Fatal("expected the retry to drop the links")
	}
}

func TestStoreMemoryDoesNotRetryOtherErrors(t *testing.T) {
	f := &fakeClient{failNext: status.Error(codes.InvalidArgument, "memory not valid")}

	_, err := StoreMemory(context.Background(), f, &hippo.Memory{Links: []*hippo.Link{New("other", 100)}})

	if err == nil {
		t.Fatal("expected an InvalidArgument to be returned rather than retried")
	}

	if len(f.memories) != 1 {
		t.Fatalf("expected a single attempt, got %d", len(f.memories))
	}
}

func TestStoreMemoryDoesNotRetryUnlinkedWrite(t *testing.T) {
	f := &fakeClient{failNext: status.Error(codes.NotFound, "event 'gone' does not exist")}

	// A NotFound on a write that declared no links was not caused by the links, so dropping them
	// changes nothing and the error stands.
	_, err := StoreMemory(context.Background(), f, &hippo.Memory{EventId: "gone"})

	if err == nil {
		t.Fatal("expected the NotFound to be returned for a write with no links")
	}

	if len(f.memories) != 1 {
		t.Fatalf("expected a single attempt, got %d", len(f.memories))
	}
}

func TestStoreEventRetriesWithoutLinksOnNotFound(t *testing.T) {
	f := &fakeClient{failNext: status.Error(codes.NotFound, "link target 'gone' does not exist")}

	e := &hippo.Event{
		Name:  "Chapter II.",
		Links: []*hippo.Link{New("gone", 100)},
	}

	id, err := StoreEvent(context.Background(), f, e)

	if err != nil {
		t.Fatalf("expected the retry to store the event, got %s", err.Error())
	}

	if id != "ev-1" {
		t.Fatalf("expected the stored id, got %q", id)
	}

	if len(f.events) != 2 {
		t.Fatalf("expected two attempts, got %d", len(f.events))
	}

	if len(f.events[1].GetLinks()) != 0 {
		t.Fatal("expected the retry to drop the links")
	}
}
