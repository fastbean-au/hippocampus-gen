package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc"

	hippo "github.com/fastbean-au/hippocampus/contract"
)

// fakeClient is a minimal HippocampusClient: it counts the store/end calls the generator makes and
// lets a test observe or interrupt the run. Embedding the interface means only the handful of RPCs
// the log generator uses need implementing; any other call would panic (and none is made).
type fakeClient struct {
	hippo.HippocampusClient

	events   int
	memories int
	ended    int
	storeErr error
	onStore  func()

	// lastMemory/lastEvent keep the most recent request so a test can assert on how a record was
	// shaped, not merely that one was sent.
	lastMemory *hippo.Memory
	lastEvent  *hippo.Event
}

func (f *fakeClient) StoreEvent(_ context.Context, in *hippo.Event, _ ...grpc.CallOption) (*hippo.StoreEventResponse, error) {
	f.events++
	f.lastEvent = in

	return &hippo.StoreEventResponse{Id: fmt.Sprintf("ev-%d", f.events)}, nil
}

func (f *fakeClient) StoreMemory(_ context.Context, in *hippo.Memory, _ ...grpc.CallOption) (*hippo.StoreMemoryResponse, error) {
	if f.storeErr != nil {

		return nil, f.storeErr
	}

	f.memories++
	f.lastMemory = in

	if f.onStore != nil {
		f.onStore()
	}

	return &hippo.StoreMemoryResponse{Id: fmt.Sprintf("m-%d", f.memories)}, nil
}

func (f *fakeClient) EndEvent(_ context.Context, _ *hippo.EndEventRequest, _ ...grpc.CallOption) (*hippo.GeneralResponse, error) {
	f.ended++

	return &hippo.GeneralResponse{}, nil
}

func TestExecuteOneShot(t *testing.T) {
	f := &fakeClient{}

	// 50 lines across 3 days exercises the back-dated one-shot path, including the per-service daily
	// event rollover and the close-open-events pass at the end.
	execute(f, 50, 3, emitConfig{links: true})

	if f.memories != 50 {
		t.Fatalf("expected 50 memories stored, got %d", f.memories)
	}

	if f.events == 0 {
		t.Fatal("expected at least one event created")
	}

	if f.ended == 0 {
		t.Fatal("expected the open daily events to be closed at the end")
	}
}

func TestExecuteClampsNonPositiveArgs(t *testing.T) {
	f := &fakeClient{}

	// Non-positive entries/days are clamped to one, so a single line is still stored.
	execute(f, 0, 0, emitConfig{links: true})

	if f.memories != 1 {
		t.Fatalf("expected a single line for clamped args, got %d", f.memories)
	}
}

func TestEmit(t *testing.T) {
	f := &fakeClient{}
	e := newEmitter(f, emitConfig{links: true})

	if !e.emit(context.Background(), time.Now().UnixNano()) {
		t.Fatal("expected emit to succeed")
	}

	if f.memories != 1 {
		t.Fatalf("expected 1 memory stored, got %d", f.memories)
	}

	if f.events != 1 {
		t.Fatalf("expected 1 event created for the new service/day, got %d", f.events)
	}

	if len(e.states) != 1 {
		t.Fatalf("expected 1 tracked service state, got %d", len(e.states))
	}
}

func TestEmitReportsStoreError(t *testing.T) {
	f := &fakeClient{storeErr: fmt.Errorf("boom")}
	e := newEmitter(f, emitConfig{links: true})

	if e.emit(context.Background(), time.Now().UnixNano()) {
		t.Fatal("expected emit to report a store failure")
	}
}

func TestExecuteLiveStopsAndClosesEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := &fakeClient{}

	// executeLive runs on a single goroutine, so onStore is called serially - no synchronisation
	// needed. Cancel after the fifth line so the run is deterministic regardless of timing.
	f.onStore = func() {
		if f.memories == 5 {
			cancel()
		}
	}

	// A high rate keeps the per-line pacing delay tiny so the test is quick.
	executeLive(ctx, f, 6000, emitConfig{links: true})

	if f.memories != 5 {
		t.Fatalf("expected exactly 5 lines before cancellation, got %d", f.memories)
	}

	if f.ended < 1 {
		t.Fatalf("expected the open daily event(s) to be closed on stop, got %d EndEvent calls", f.ended)
	}
}

func TestExecuteLiveClampsRate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := &fakeClient{onStore: func() {}}

	// A non-positive rate is clamped to 1; cancel after the first line so a 60s pacing delay is
	// never actually waited out.
	f.onStore = func() {
		cancel()
	}

	executeLive(ctx, f, 0, emitConfig{links: true})

	if f.memories != 1 {
		t.Fatalf("expected one line emitted before cancellation, got %d", f.memories)
	}
}

// TestEmitGroupAndMetadata pins how a line is filed. The default keeps the service as the group,
// which is what this generator has always done; --group overrides it, which is what makes the
// generator usable against a service that issues group-scoped tokens (such a token may write only
// the groups it holds, so service-as-group would have every write refused). Either way the service
// survives as metadata, so a query can still select by it.
func TestEmitGroupAndMetadata(t *testing.T) {
	cases := []struct {
		name      string
		cfg       emitConfig
		wantGroup func(service string) string
	}{
		{
			"the service doubles as the group by default",
			emitConfig{},
			func(service string) string { return service },
		},
		{
			"an explicit group overrides it",
			emitConfig{group: "telemetry"},
			func(string) string { return "telemetry" },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeClient{}
			e := newEmitter(f, c.cfg)

			if !e.emit(context.Background(), time.Now().UnixNano()) {
				t.Fatal("expected emit to succeed")
			}

			service := f.lastMemory.GetMetadata()["service"]
			if service == "" {
				t.Fatal("the memory carries no service metadata - it must survive the group being reused for scoping")
			}

			if got, want := f.lastMemory.GetGroup(), c.wantGroup(service); got != want {
				t.Errorf("memory group = %q, want %q", got, want)
			}

			if f.lastMemory.GetMetadata()["level"] == "" {
				t.Error("the memory carries no level metadata")
			}

			// The event opened for the same line must be filed identically, or a scoped token would
			// be refused the event while accepting its memories.
			if got, want := f.lastEvent.GetGroup(), c.wantGroup(service); got != want {
				t.Errorf("event group = %q, want %q", got, want)
			}

			if f.lastEvent.GetMetadata()["service"] != service {
				t.Errorf("event service metadata = %q, want %q", f.lastEvent.GetMetadata()["service"], service)
			}
		})
	}
}
