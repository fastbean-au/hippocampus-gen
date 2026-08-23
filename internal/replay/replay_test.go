package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	hippo "github.com/fastbean-au/hippocampus/contract"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus-gen/internal/fit"
	"github.com/fastbean-au/hippocampus-gen/internal/trace"
)

// fakeClient is a store that remembers everything and forgets nothing, so a test can assert what the
// replay did rather than what a service decided.
type fakeClient struct {
	mu sync.Mutex

	events   []string
	memories map[string]*hippo.Memory
	order    []string
	recalls  []string

	// rejectEvery makes every nth store come back rejected, which is how a real instance answers a
	// memory below its minimum significance.
	rejectEvery int
	stores      int

	// unitsOfAge is what ExplainConsolidation reports as the instance's decay clock.
	unitsOfAge float64

	// forget names ids ExplainConsolidation will omit, standing in for memories consolidated away.
	forget map[string]bool
}

func newFake() *fakeClient {
	return &fakeClient{memories: map[string]*hippo.Memory{}, forget: map[string]bool{}}
}

func (c *fakeClient) StoreEvent(_ context.Context, in *hippo.Event, _ ...grpc.CallOption) (*hippo.StoreEventResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.events = append(c.events, in.GetId())

	return &hippo.StoreEventResponse{Id: in.GetId()}, nil
}

func (c *fakeClient) StoreMemory(_ context.Context, in *hippo.Memory, _ ...grpc.CallOption) (*hippo.StoreMemoryResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stores++

	// A link to an id the store does not hold fails the whole write, exactly as the service does -
	// which is the behaviour the replay's link filtering exists to avoid provoking.
	for _, v := range in.GetLinks() {
		if _, ok := c.memories[v.GetId()]; !ok {

			return nil, fmt.Errorf("link target %s not found", v.GetId())
		}
	}

	if c.rejectEvery > 0 && c.stores%c.rejectEvery == 0 {

		return &hippo.StoreMemoryResponse{Rejected: true}, nil
	}

	c.memories[in.GetId()] = in
	c.order = append(c.order, in.GetId())

	return &hippo.StoreMemoryResponse{Id: in.GetId()}, nil
}

func (c *fakeClient) RecallMemories(_ context.Context, in *hippo.RecallMemoriesRequest, _ ...grpc.CallOption) (*hippo.GetMemoriesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := &hippo.GetMemoriesResponse{}

	for _, v := range in.GetIds() {
		c.recalls = append(c.recalls, v)

		if m, ok := c.memories[v]; ok {
			out.Memories = append(out.Memories, m)
		}
	}

	return out, nil
}

func (c *fakeClient) ExplainConsolidation(_ context.Context, in *hippo.ExplainConsolidationRequest, _ ...grpc.CallOption) (*hippo.ExplainConsolidationResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := &hippo.ExplainConsolidationResponse{UnitsOfAgeInDays: c.unitsOfAge}

	for _, v := range in.GetMemoryIds() {
		if c.forget[v] {
			continue
		}

		if _, ok := c.memories[v]; ok {
			out.Valuations = append(out.Valuations, &hippo.MemoryValuation{Id: v})
		}
	}

	return out, nil
}

// SearchMemories returns every memory whose body carries any of the query's terms, ranked by id, so
// a test can assert what Rankings did with the answer rather than model relevance.
func (c *fakeClient) SearchMemories(_ context.Context, in *hippo.SearchMemoriesRequest, _ ...grpc.CallOption) (*hippo.GetMemoriesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if in.GetReinforce() {

		return nil, fmt.Errorf("a scoring search must never reinforce")
	}

	out := &hippo.GetMemoriesResponse{}

	for _, id := range c.order {
		m := c.memories[id]

		for _, term := range strings.Fields(in.GetQuery()) {
			if strings.Contains(m.GetBody(), term) {
				out.Memories = append(out.Memories, m)

				break
			}
		}

		if len(out.Memories) >= int(in.GetLimit()) {

			break
		}
	}

	return out, nil
}

func testTrace(t *testing.T, memories int) *trace.Trace {
	t.Helper()

	data, err := os.ReadFile("../../data/params.json")
	if err != nil {
		t.Fatalf("reading params: %v", err)
	}

	var params fit.Params

	if err := json.Unmarshal(data, &params); err != nil {
		t.Fatalf("parsing params: %v", err)
	}

	tr, err := trace.Generate(trace.Config{
		Params:               params,
		Seed:                 7,
		Memories:             memories,
		Days:                 30,
		Agents:               2,
		SignificanceSignal:   trace.SignalImportance,
		ImportanceShape:      trace.ShapeMeasured,
		SignificanceScale:    trace.ScaleLinear,
		MustKeepShare:        0.05,
		SignificanceNoise:    0.3,
		LinkScale:            0.2,
		RetrievalHorizonDays: 15,
		TermsPerMemory:       4,
		MemoriesPerTerm:      50,
		QueryTerms:           3,
		MinSignificance:      1000,
		MaxSignificance:      30000,
		BodyBytes:            128,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	return tr
}

// fastConfig runs the whole simulated span in a fraction of a second, so ordering is exercised
// without the test waiting for the clock.
func fastConfig() Config {
	return Config{
		SimDaysPerWallMinute: 60 * 60 * 24,
		Workers:              8,
		Tick:                 time.Millisecond,
		Events:               true,
	}
}

func TestRequiredUnitsOfAgeInDays(t *testing.T) {
	// One simulated day per wall minute means a day of decay every sixty seconds, which is one
	// 1440th of a wall day.
	got := Config{SimDaysPerWallMinute: 1}.RequiredUnitsOfAgeInDays()

	if diff := got - 1.0/1440; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("got %g, want %g", got, 1.0/1440)
	}

	if got := (Config{}).RequiredUnitsOfAgeInDays(); got != 0 {
		t.Errorf("an unset speed should not imply a setting, got %g", got)
	}
}

// TestVerifyRefusesAMisconfiguredInstance is the guard against the most expensive mistake available
// here: spending hours driving a trace into a store whose decay clock does not match the replay,
// and reporting the result as if it did.
func TestVerifyRefusesAMisconfiguredInstance(t *testing.T) {
	tr := testTrace(t, 200)
	cfg := fastConfig()
	cfg.SimDaysPerWallMinute = 1

	fake := newFake()
	fake.unitsOfAge = 1.0 / 1440

	if _, err := New(fake, tr, cfg).Verify(context.Background()); err != nil {
		t.Errorf("a correctly configured instance was refused: %v", err)
	}

	fake.unitsOfAge = 0.002

	_, err := New(fake, tr, cfg).Verify(context.Background())
	if err == nil {
		t.Fatal("a mismatched decay clock was accepted")
	}

	if !strings.Contains(err.Error(), "unitsOfAgeInDays") {
		t.Errorf("the error should name the setting to change, got %q", err)
	}
}

// TestRunStoresBeforeItRecalls is the ordering guarantee. A recall of a memory the store does not
// hold yet is silently lost reinforcement, and reinforcement is what the benchmark turns on.
func TestRunStoresBeforeItRecalls(t *testing.T) {
	tr := testTrace(t, 1500)
	fake := newFake()

	stats, err := New(fake, tr, fastConfig()).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats.Stored != len(tr.Memories) {
		t.Errorf("stored %d memories, want %d", stats.Stored, len(tr.Memories))
	}

	// One event per session that actually OWNS a memory, which is not the same as one per session:
	// events are created lazily, when a session's first memory needs one, and a session holding only
	// recalls of memories created elsewhere never needs an event at all.
	owning := map[int]bool{}

	for i := range tr.Memories {
		owning[tr.Memories[i].Session] = true
	}

	if stats.Events != len(owning) {
		t.Errorf("stored %d events, want %d (sessions owning at least one memory, of %d sessions)",
			stats.Events, len(owning), len(tr.Sessions))
	}

	if stats.Recalled == 0 {
		t.Fatal("no recalls were issued")
	}

	// Every recall must have named a memory the store held, which the fake reports by returning it.
	fake.mu.Lock()
	defer fake.mu.Unlock()

	held := map[string]bool{}

	for _, v := range fake.order {
		held[v] = true
	}

	for _, v := range fake.recalls {
		if !held[v] {
			t.Fatalf("recalled %s, which was never stored", v)
		}
	}
}

// TestLinksAreOnlyEverAttachedToConfirmedTargets pins the containment: the fake fails a write naming
// an absent target exactly as the service does, so if the replay ever offered one the run would
// error rather than merely lose an edge.
func TestLinksAreOnlyEverAttachedToConfirmedTargets(t *testing.T) {
	tr := testTrace(t, 1500)
	fake := newFake()

	// Rejecting some stores is the sharper case: a rejected memory is absent from the store but the
	// trace still links to it, so filtering on "confirmed" rather than "dispatched" is what holds.
	fake.rejectEvery = 7

	stats, err := New(fake, tr, fastConfig()).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats.Rejected == 0 {
		t.Fatal("the fake rejected nothing, so the case under test did not arise")
	}

	if stats.Links == 0 {
		t.Fatal("no links were attached at all")
	}

	// Some loss is expected and is reported rather than hidden. How MUCH is a property of the
	// batching rather than of the filter - see the test below - so it is not asserted here. What is
	// asserted is that the run completed at all: the fake fails any write naming an absent target,
	// exactly as the service does, so reaching this line means no unconfirmed target was ever
	// offered.
	if stats.LinksLost == 0 {
		t.Error("no losses were reported, so the counter is not being maintained")
	}
}

// TestWavesLoseNoLinkToAMemoryTheStoreAccepted pins what the dependency waves buy.
//
// Before them, a link whose target sat in the same batch was dropped, and since links come from
// within-session co-occurrence that was the common case - three quarters of the trace's edges went
// missing on a real run, leaving the store far less connected than the trace said and understating
// the very arm being measured. With waves the only edge that may be lost is one pointing at a
// memory the store REJECTED, which is a memory that does not exist and never will.
func TestWavesLoseNoLinkToAMemoryTheStoreAccepted(t *testing.T) {
	tr := testTrace(t, 1500)

	for _, tick := range []time.Duration{time.Millisecond, 100 * time.Millisecond, time.Second} {
		cfg := fastConfig()
		cfg.Tick = tick

		stats, err := New(newFake(), tr, cfg).Run(context.Background())
		if err != nil {
			t.Fatalf("Run at tick %s: %v", tick, err)
		}

		if stats.LinksLost != 0 {
			t.Errorf("tick %s: lost %d links with nothing rejected", tick, stats.LinksLost)
		}

		if stats.Links == 0 {
			t.Errorf("tick %s: no links attached at all", tick)
		}
	}
}

// TestEveryRecallInTheTraceReachesTheStore is the fairness guard on the other side.
//
// RecallMemories reinforces an id once however many times a single call names it, and the fitted
// trace makes repeats routine - 73% of re-references are same-session bursts a median of eleven
// seconds apart, so at any useful replay speed several land in one batch. A first version of this
// harness issued one call per batch and delivered barely a third of the trace's reinforcement, which
// silently stripped the store of its main differentiator while every baseline, computed from the
// trace, still saw every touch.
func TestEveryRecallInTheTraceReachesTheStore(t *testing.T) {
	tr := testTrace(t, 1500)

	want := 0

	for _, v := range tr.Memories {
		want += v.Recalls
	}

	if want == 0 {
		t.Fatal("the trace carries no recalls")
	}

	// fastConfig compresses the whole span into a single tick, which is the worst case for
	// collapsing and the one that must hold. (A wide Tick would express the same thing, but the
	// replay would then correctly PACE itself to that tick and the test would wait for it.)
	cfg := fastConfig()

	fake := newFake()

	stats, err := New(fake, tr, cfg).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats.Recalled != want {
		t.Errorf("the store saw %d reinforcements for a trace carrying %d", stats.Recalled, want)
	}
}

// forgetfulClient is a store that consolidates things away between one call and the next, which is
// what a real one under a tight capacity target does. It exists to drive the two recovery paths,
// both of which were found by running against a real instance rather than by writing a test.
type forgetfulClient struct {
	*fakeClient

	// dropEvent makes the next memory write fail as though its event had been consolidated.
	dropEvent bool

	// dropLinkTarget makes any write carrying links fail once with NotFound, as though a target had
	// been consolidated between being confirmed and being linked to.
	dropLinkTarget bool

	failures int
}

func (c *forgetfulClient) StoreMemory(ctx context.Context, in *hippo.Memory, opts ...grpc.CallOption) (*hippo.StoreMemoryResponse, error) {
	c.mu.Lock()

	if c.dropEvent && in.GetEventId() != "" {
		c.dropEvent = false
		c.failures++
		c.mu.Unlock()

		return nil, status.Error(codes.FailedPrecondition, "event 'session-000001' does not exist")
	}

	if c.dropLinkTarget && len(in.GetLinks()) > 0 {
		c.dropLinkTarget = false
		c.failures++
		c.mu.Unlock()

		return nil, status.Error(codes.NotFound, "no such memory: mem-00000042")
	}

	c.mu.Unlock()

	return c.fakeClient.StoreMemory(ctx, in, opts...)
}

// TestARecreatedEventLetsTheWriteThrough covers the first failure a real run hit. An event is
// deleted once the last of its memories has been consolidated, and its session may still have
// memories to come - so the writer re-creates it rather than giving up.
func TestARecreatedEventLetsTheWriteThrough(t *testing.T) {
	tr := testTrace(t, 400)
	fake := newFake()
	c := &forgetfulClient{fakeClient: fake, dropEvent: true}

	stats, err := New(c, tr, fastConfig()).Run(context.Background())
	if err != nil {
		t.Fatalf("a forgotten event should be recovered, not fatal: %v", err)
	}

	if c.failures != 1 {
		t.Fatalf("the case under test did not arise (%d failures injected)", c.failures)
	}

	if stats.Stored != len(tr.Memories) {
		t.Errorf("stored %d of %d memories", stats.Stored, len(tr.Memories))
	}
}

// TestAForgottenLinkTargetCostsTheEdgeAndNotTheRun covers the second, which is a race no ordering
// can remove: the store may consolidate a link target between confirming it and accepting the write
// that names it. Losing the edge is containable; losing the run is not.
func TestAForgottenLinkTargetCostsTheEdgeAndNotTheRun(t *testing.T) {
	tr := testTrace(t, 400)
	fake := newFake()
	c := &forgetfulClient{fakeClient: fake, dropLinkTarget: true}

	stats, err := New(c, tr, fastConfig()).Run(context.Background())
	if err != nil {
		t.Fatalf("a forgotten link target should cost an edge, not the run: %v", err)
	}

	if c.failures != 1 {
		t.Fatalf("the case under test did not arise (%d failures injected)", c.failures)
	}

	if stats.Stored != len(tr.Memories) {
		t.Errorf("stored %d of %d memories", stats.Stored, len(tr.Memories))
	}

	// The dropped edges are reported rather than silently absorbed.
	if stats.LinksLost == 0 {
		t.Error("the edges lost to the retry were not counted")
	}
}

// TestAnUnrecognisedFailureStillStopsTheRun pins the other side of that recovery: only the two
// understood refusals are absorbed. Anything else is a real fault and must surface rather than be
// retried into a quietly wrong result.
func TestAnUnrecognisedFailureStillStopsTheRun(t *testing.T) {
	tr := testTrace(t, 200)

	c := &erroringClient{fakeClient: newFake()}

	if _, err := New(c, tr, fastConfig()).Run(context.Background()); err == nil {
		t.Fatal("an unrecognised store failure was swallowed")
	}
}

type erroringClient struct {
	*fakeClient
}

func (c *erroringClient) StoreMemory(_ context.Context, _ *hippo.Memory, _ ...grpc.CallOption) (*hippo.StoreMemoryResponse, error) {
	return nil, status.Error(codes.Internal, "the store is on fire")
}

// TestAWriteLosingBothItsEventAndItsLinksStillLands is the case that killed a real run at a tight
// capacity target, where eviction is fast enough that one write trips over two separate refusals.
// Recovering from one per write is not enough, and the failure only appears at the small store sizes
// the result depends on most.
func TestAWriteLosingBothItsEventAndItsLinksStillLands(t *testing.T) {
	tr := testTrace(t, 600)
	c := &forgetfulClient{fakeClient: newFake(), dropEvent: true, dropLinkTarget: true}

	stats, err := New(c, tr, fastConfig()).Run(context.Background())
	if err != nil {
		t.Fatalf("a write hitting both refusals should still land: %v", err)
	}

	if c.failures != 2 {
		t.Fatalf("expected both refusals to fire, got %d", c.failures)
	}

	if stats.Stored != len(tr.Memories) {
		t.Errorf("stored %d of %d memories", stats.Stored, len(tr.Memories))
	}
}
