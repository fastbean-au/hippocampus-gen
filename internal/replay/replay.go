// Package replay drives a generated trace into a running Hippocampus instance, in compressed
// simulated time.
//
// # Why the store is driven and not simulated
//
// The whole value of the benchmark is that the shipped consolidation code made the decisions. So the
// trace is replayed into a real service running real sleep cycles, and never bulk-imported with
// back-dated timestamps for a single cycle to sort out - that would measure a scan, not a policy.
//
// # How simulated time reaches the store
//
// Memories are stored with no timestamp, so the service stamps them with its own clock, and the
// replay compresses the trace's calendar into wall time by a fixed factor. A simulated day of decay
// is then a chosen number of wall seconds, which the service's consolidation.unitsOfAgeInDays must
// agree with. That agreement is not left to hope: RequiredUnitsOfAgeInDays computes what the setting
// must be, and Verify reads back what the instance is actually configured with and refuses a run
// that would have measured a decay rate nobody intended.
//
// # Ordering
//
// A recall of a memory the store does not hold yet is a lost reinforcement and the benchmark turns
// on reinforcement, so ordering is enforced rather than assumed. Operations are dispatched in
// batches of one wall-clock tick: every store in the batch completes before any recall in it is
// issued, and a batch completes before the next begins. Within that, work is concurrent.
package replay

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	hippo "github.com/fastbean-au/hippocampus/contract"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus-gen/internal/trace"
)

// Client is the slice of the service a replay may use. Declaring it here rather than accepting the
// generated client is the harness's statement of what it can do to a store: it writes, recalls,
// links and asks. It cannot sleep, purge, clear or delete, and no amount of misconfiguration can
// make it.
type Client interface {
	StoreEvent(ctx context.Context, in *hippo.Event, opts ...grpc.CallOption) (*hippo.StoreEventResponse, error)
	StoreMemory(ctx context.Context, in *hippo.Memory, opts ...grpc.CallOption) (*hippo.StoreMemoryResponse, error)
	RecallMemories(ctx context.Context, in *hippo.RecallMemoriesRequest, opts ...grpc.CallOption) (*hippo.GetMemoriesResponse, error)
	ExplainConsolidation(ctx context.Context, in *hippo.ExplainConsolidationRequest, opts ...grpc.CallOption) (*hippo.ExplainConsolidationResponse, error)
	SearchMemories(ctx context.Context, in *hippo.SearchMemoriesRequest, opts ...grpc.CallOption) (*hippo.GetMemoriesResponse, error)
}

// recallBatchSize bounds one RecallMemories call. Recalls dominate the operation count - the fitted
// corpus has roughly five per memory - so batching them is what keeps the replay's RPC rate
// governed by the store rate rather than by the reuse rate.
const recallBatchSize = 200

// explainBatchSize is the contract's own per-call cap on ExplainConsolidation
// (ExplainConsolidationRequest.memory_ids, "at most 200 per call").
const explainBatchSize = 200

// Config is how a trace is driven. Workers bounds concurrency within a batch; Tick is the wall-clock
// batch width, and trades ordering granularity against RPC batching.
type Config struct {
	SimDaysPerWallMinute float64
	Workers              int
	Tick                 time.Duration

	// Group prefixes every group label the replay writes, so several runs can share one store
	// without their scoring sets colliding.
	Group string

	// Events writes one event per session and files each memory under it. Off makes every memory
	// event-less, which is a materially different decay regime - an event contributes its own
	// significance to its memories' value.
	Events bool
}

// Stats is what a replay did, reported so a run that quietly achieved less than it claimed is
// visible.
type Stats struct {
	Events    int
	Stored    int
	Rejected  int
	Recalled  int
	Links     int
	LinksLost int

	Elapsed time.Duration
}

// Replay drives one trace into one client.
type Replay struct {
	client Client
	trace  *trace.Trace
	cfg    Config

	// stored records which memories the store has confirmed, which is what lets a link be attached
	// only when its target is certainly present.
	mu     sync.RWMutex
	stored map[int]string

	// events records which sessions currently have an event in the store. It is a cache of a fact
	// the store may revoke at any moment - see ensureEvent.
	eventsMu sync.Mutex
	events   map[int]bool

	// touched is the WALL-CLOCK moment each memory was last written or recalled - see Touched.
	touchedMu sync.Mutex
	touched   []time.Time

	stats Stats
}

// New builds a replay. It does not contact the service.
func New(client Client, tr *trace.Trace, cfg Config) *Replay {
	if cfg.Workers <= 0 {
		cfg.Workers = 8
	}

	if cfg.Tick <= 0 {
		cfg.Tick = time.Second
	}

	return &Replay{
		client:  client,
		trace:   tr,
		cfg:     cfg,
		stored:  map[int]string{},
		events:  map[int]bool{},
		touched: make([]time.Time, len(tr.Memories)),
	}
}

// Touched is the wall-clock time each memory was last written or recalled during the replay, and it
// exists to make the comparison with the recency baselines honest.
//
// The store's decay clock runs on ITS OWN clock: it stamps a memory when written and re-stamps it
// when recalled, so what it can distinguish is bounded by wall time. The replay compresses simulated
// time, so two recalls eleven simulated seconds apart - the median burst gap in the fitted corpus -
// land a fraction of a millisecond apart in wall time and are, to the store, simultaneous.
//
// A baseline scored on the TRACE's simulated times would therefore be reading a sharper signal than
// the store is allowed to see, and would win on recency for a reason that has nothing to do with
// either policy. Scoring both on these timings puts them on equal terms at any replay speed, which
// is otherwise only achievable by replaying in something close to real time.
//
// Recorded at dispatch rather than on the response, so it is the moment the operation was issued
// rather than the moment it was acknowledged - the closer of the two to what the server stamped.
func (r *Replay) Touched() []time.Time {
	r.touchedMu.Lock()
	defer r.touchedMu.Unlock()

	out := make([]time.Time, len(r.touched))
	copy(out, r.touched)

	return out
}

// markTouched records that a memory was written or recalled just now.
func (r *Replay) markTouched(memories ...int) {
	now := time.Now()

	r.touchedMu.Lock()
	defer r.touchedMu.Unlock()

	for _, v := range memories {
		if v >= 0 && v < len(r.touched) {
			r.touched[v] = now
		}
	}
}

// RequiredUnitsOfAgeInDays is the consolidation.unitsOfAgeInDays an instance must be configured with
// for one unit of the decay clock to equal one simulated day at this replay speed. A unit is
// unitsOfAgeInDays of WALL time; a simulated day is 1/(1440 x SimDaysPerWallMinute) wall days.
func (c Config) RequiredUnitsOfAgeInDays() float64 {
	if c.SimDaysPerWallMinute <= 0 {

		return 0
	}

	return 1 / (1440 * c.SimDaysPerWallMinute)
}

// Duration is how long driving the whole trace will take in wall time.
func (r *Replay) Duration() time.Duration {
	span := r.trace.End.Sub(r.trace.Start)

	return time.Duration(float64(span) / (r.cfg.SimDaysPerWallMinute * 1440))
}

// Verify reads the instance's own decay configuration and reports any disagreement with what this
// replay needs. It is a separate call rather than part of Run so a caller can decide whether a
// mismatch is fatal - it usually should be, since the alternative is spending hours measuring a
// decay rate nobody chose.
func (r *Replay) Verify(ctx context.Context) (*hippo.ExplainConsolidationResponse, error) {
	resp, err := r.client.ExplainConsolidation(ctx, &hippo.ExplainConsolidationRequest{})
	if err != nil {

		return nil, fmt.Errorf("reading the instance's consolidation settings: %w", err)
	}

	want := r.cfg.RequiredUnitsOfAgeInDays()

	// A tenth of a percent: the value is written into a config file by hand, so exact equality would
	// fail on a rounded literal, while anything looser would let a genuinely different clock pass.
	if diff := abs(resp.GetUnitsOfAgeInDays() - want); want > 0 && diff > want*0.001 {

		return resp, fmt.Errorf(
			"instance has consolidation.unitsOfAgeInDays %g, but %g simulated days per wall minute needs %.9g - "+
				"the run would measure a decay rate nobody chose",
			resp.GetUnitsOfAgeInDays(), r.cfg.SimDaysPerWallMinute, want)
	}

	return resp, nil
}

// Run drives the whole trace, returning what it did. It blocks for Duration.
func (r *Replay) Run(ctx context.Context) (Stats, error) {
	started := time.Now()

	for _, batch := range r.batches() {
		if err := r.waitFor(ctx, started, batch.until); err != nil {

			return r.stats, err
		}

		// Stores first and to completion: a recall in this batch may name a memory stored in it.
		if err := r.runStores(ctx, batch.stores); err != nil {

			return r.stats, err
		}

		if err := r.runRecalls(ctx, batch.recalls); err != nil {

			return r.stats, err
		}
	}

	r.stats.Elapsed = time.Since(started)

	return r.stats, nil
}

// batch is one wall-clock tick's worth of operations.
type batch struct {
	until   time.Duration
	stores  []int
	recalls []int
}

// batches slices the trace's operations by the wall-clock instant they are due, so each batch is one
// tick wide.
func (r *Replay) batches() []batch {
	total := r.Duration()

	var out []batch

	current := batch{until: r.cfg.Tick}

	for _, v := range r.trace.Ops {
		offset := time.Duration(float64(v.At.Sub(r.trace.Start)) / float64(r.trace.End.Sub(r.trace.Start)) * float64(total))

		for offset > current.until {
			out = append(out, current)
			current = batch{until: current.until + r.cfg.Tick}
		}

		if v.Kind == trace.OpStore {
			current.stores = append(current.stores, v.Memory)

			continue
		}

		current.recalls = append(current.recalls, v.Memory)
	}

	return append(out, current)
}

// waitFor sleeps until the batch is due, honouring cancellation. A batch already overdue - the
// service could not keep up - returns immediately, so the replay degrades into running as fast as
// it can rather than silently skipping work.
func (r *Replay) waitFor(ctx context.Context, started time.Time, until time.Duration) error {
	delay := time.Until(started.Add(until))
	if delay <= 0 {

		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {

	case <-ctx.Done():

		return ctx.Err()

	case <-timer.C:

		return nil
	}
}

// ensureEvent creates a session's event if this replay has not already seen it created.
//
// Events are created lazily - when a session's first memory needs one - rather than all up front,
// and the difference is not tidiness. An event written at the start of a run ages through the whole
// of it, and an event holding no memories is exactly what the consolidation cycle's third pass
// deletes; the first version of this code wrote them all up front and the run died two thousand
// memories in, storing a memory against an event that had already been forgotten.
//
// force re-creates one the store has since dropped. An AlreadyExists is success, not an error: two
// workers in the same batch routinely need the same session.
func (r *Replay) ensureEvent(ctx context.Context, session int, force bool) error {
	r.eventsMu.Lock()

	if r.events[session] && !force {
		r.eventsMu.Unlock()

		return nil
	}

	r.eventsMu.Unlock()

	in := r.trace.Sessions[session]

	_, err := r.client.StoreEvent(ctx, &hippo.Event{
		Id:           in.ID,
		Name:         fmt.Sprintf("%s working session", in.Group),
		Significance: eventSignificance,
		Group:        r.cfg.Group + in.Group,
	})

	if err != nil && status.Code(err) != codes.AlreadyExists {

		return fmt.Errorf("storing event %s: %w", in.ID, err)
	}

	r.eventsMu.Lock()

	if !r.events[session] {
		r.stats.Events++
	}

	r.events[session] = true
	r.eventsMu.Unlock()

	return nil
}

// eventSignificance is what every generated event is stored with. One value for all of them is
// deliberate: an event contributes its significance to each of its memories' effective
// significance, so a varying one would be a second, unfitted signal shaping what survives, and the
// benchmark would no longer be measuring the memory-level policy it claims to.
const eventSignificance = 1000

// runStores writes a batch's memories, in dependency order.
//
// A link may only name a target the store has already confirmed, and a batch's stores run
// concurrently - so a memory linking to another in the SAME batch would have its link dropped. That
// is not rare: links come from within-session co-occurrence and a session is contiguous in time, so
// source and target usually land together. Dropping them wholesale would leave the store far less
// connected than the trace says, and a link protects what it connects, so it would understate
// exactly the arm being measured.
//
// Instead the batch is written in WAVES: everything whose targets are already confirmed goes first,
// concurrently, then everything unblocked by that, and so on. The trace only ever links backwards in
// time, so this always terminates; in practice it converges in two or three waves.
func (r *Replay) runStores(ctx context.Context, memories []int) error {
	for len(memories) > 0 {
		ready, blocked := r.partition(memories)

		// Nothing is unblocked, which the trace's backwards-only linking should make impossible.
		// Writing the remainder anyway - losing their in-batch links - beats looping forever.
		if len(ready) == 0 {
			ready, blocked = memories, nil
		}

		if err := r.storeWave(ctx, ready); err != nil {

			return err
		}

		memories = blocked
	}

	return nil
}

// partition splits a batch into memories whose link targets are all confirmed and those still
// waiting on one.
func (r *Replay) partition(memories []int) ([]int, []int) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var ready, blocked []int

	for _, v := range memories {
		waiting := false

		for _, target := range r.trace.Memories[v].Links {
			if _, ok := r.stored[target]; !ok {
				waiting = true

				break
			}
		}

		if waiting {
			blocked = append(blocked, v)

			continue
		}

		ready = append(ready, v)
	}

	return ready, blocked
}

// storeWave writes one wave of a batch concurrently.
func (r *Replay) storeWave(ctx context.Context, memories []int) error {
	if len(memories) == 0 {

		return nil
	}

	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(r.cfg.Workers)

	var mu sync.Mutex

	for _, v := range memories {
		memory := v

		group.Go(func() error {
			m := r.trace.Memories[memory]

			links, lost := r.linksFor(m)

			in := &hippo.Memory{
				Id:           m.ID,
				Significance: m.Significance,
				Body:         m.Body,
				Group:        r.cfg.Group + m.Group,
				Links:        links,
			}

			if r.cfg.Events {
				in.EventId = r.trace.Sessions[m.Session].ID

				if err := r.ensureEvent(ctx, m.Session, false); err != nil {

					return err
				}
			}

			r.markTouched(memory)

			resp, err := r.storeMemory(ctx, in, m.Session)
			if err != nil {

				return fmt.Errorf("storing memory %s: %w", m.ID, err)
			}

			mu.Lock()

			r.stats.Links += len(links)
			r.stats.LinksLost += lost

			if resp.GetRejected() {
				r.stats.Rejected++
			} else {
				r.stats.Stored++
			}

			mu.Unlock()

			// Only a memory the store confirmed may be a link target. A rejected one is absent, and
			// linking to it later would fail the write that named it.
			if !resp.GetRejected() {
				r.mu.Lock()
				r.stored[memory] = m.ID
				r.mu.Unlock()
			}

			return nil
		})
	}

	return group.Wait()
}

// storeMemory writes a memory, recovering from the ways a forgetting store can refuse a write that
// was correct when it was built. Both refusals are the store behaving properly, and neither is a
// reason to abandon a run.
//
// FailedPrecondition: the session's event is gone. Once the last of an event's memories has been
// consolidated away the cycle deletes the event too - and the session may still have memories left
// to arrive. The event is re-created and the write retried.
//
// NotFound: a link target has been forgotten. This one cannot be designed away, only handled: the
// target is checked against what the store confirmed, but the store may consolidate it in the
// interval between that check and the write, and under a tight capacity target that interval is
// routinely enough. The memory is rewritten with no links and the edges counted as lost, because
// losing an edge is containable and failing the run is not.
//
// The retries are a LOOP rather than a single attempt because under a tight budget both happen to
// the same write: the event is re-created, and the retry then trips over a link target that has gone
// in the meantime. Handling one refusal per write left the run dying a few thousand memories in, and
// only at the small store sizes that matter most to the result. The bound is what keeps a genuinely
// broken store from being retried forever.
//
// This is the same trap the event-source bridges hit, and it is why they attach links AFTER the
// write rather than with it. The replay keeps them inline because an extra RPC per memory would
// change what is being measured, and pays for it here.
func (r *Replay) storeMemory(ctx context.Context, in *hippo.Memory, session int) (*hippo.StoreMemoryResponse, error) {
	var err error

	for attempt := 0; attempt < storeAttempts; attempt++ {
		var resp *hippo.StoreMemoryResponse

		resp, err = r.client.StoreMemory(ctx, in)
		if err == nil {

			return resp, nil
		}

		switch status.Code(err) {

		case codes.FailedPrecondition:
			if !r.cfg.Events {

				return nil, err
			}

			if err := r.ensureEvent(ctx, session, true); err != nil {

				return nil, err
			}

		case codes.NotFound:
			if len(in.GetLinks()) == 0 {

				return nil, err
			}

			r.mu.Lock()
			r.stats.LinksLost += len(in.GetLinks())
			r.stats.Links -= len(in.GetLinks())
			r.mu.Unlock()

			in.Links = nil

		default:

			return nil, err
		}
	}

	return nil, fmt.Errorf("storing after %d attempts: %w", storeAttempts, err)
}

// storeAttempts bounds storeMemory's recovery loop. Three covers a write that loses both its event
// and a link target and then succeeds, which is the worst case the store can actually present.
const storeAttempts = 3

// linksFor filters a memory's generated links down to targets the store has confirmed, and reports
// how many were dropped.
//
// The trace only ever links backwards in time, so in principle every target exists by now. In
// practice a target stored in the SAME batch may not have been confirmed when this memory is
// dispatched, and a link to an id the store does not hold does not merely lose an edge - it fails
// the whole write with NotFound. Dropping the edge is the containable failure, and the count is
// reported so a run losing many of them is visible rather than quietly less connected than intended.
func (r *Replay) linksFor(m trace.Memory) ([]*hippo.Link, int) {
	if len(m.Links) == 0 {

		return nil, 0
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*hippo.Link, 0, len(m.Links))
	lost := 0

	for _, v := range m.Links {
		id, ok := r.stored[v]

		if !ok {
			lost++

			continue
		}

		out = append(out, &hippo.Link{Id: id, Significance: trace.LinkSignificance})
	}

	return out, lost
}

// runRecalls reinforces a batch's memories, batched into as few calls as the contract allows.
//
// The subtlety is that RecallMemories is an UPDATE over an id set, so naming the same memory twice
// in one call reinforces it ONCE. The fitted trace makes that the common case rather than a corner:
// 73% of re-references are same-session bursts a median of eleven seconds apart, so at any useful
// replay speed a memory is routinely touched several times within one batch. Collapsing those would
// quietly strip the store of most of the reinforcement the trace specifies - and reinforcement is
// exactly what distinguishes it from the baselines, which are computed from the trace and see every
// touch. The benchmark would have been measuring the harness.
//
// So a batch is split into ROUNDS: every distinct id in the first, ids touched at least twice in the
// second, and so on. Almost every batch needs one round; the rounds cost nothing when they are not
// needed and are the difference between a fair comparison and a flattering one when they are.
//
// Recalling an id the store no longer holds is not an error - it is a memory that has been
// forgotten, which is the entire point - so a batch is never split to find out which.
func (r *Replay) runRecalls(ctx context.Context, memories []int) error {
	if len(memories) == 0 {

		return nil
	}

	r.mu.RLock()

	counts := map[string]int{}
	most := 0

	for _, v := range memories {
		if id, ok := r.stored[v]; ok {
			counts[id]++

			if counts[id] > most {
				most = counts[id]
			}
		}
	}

	r.mu.RUnlock()

	r.markTouched(memories...)

	for round := 0; round < most; round++ {
		ids := make([]string, 0, len(counts))

		for k, v := range counts {
			if v > round {
				ids = append(ids, k)
			}
		}

		// Sorted so a run is reproducible: map iteration order would otherwise decide which
		// memories share a call, and with it the exact interleaving of reinforcement.
		sort.Strings(ids)

		if err := r.recallEach(ctx, ids); err != nil {

			return err
		}
	}

	return nil
}

// recallEach issues one pass of recalls over a set of distinct ids, in contract-sized chunks.
func (r *Replay) recallEach(ctx context.Context, ids []string) error {
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(r.cfg.Workers)

	var mu sync.Mutex

	for i := 0; i < len(ids); i += recallBatchSize {
		last := i + recallBatchSize

		if last > len(ids) {
			last = len(ids)
		}

		chunk := ids[i:last]

		group.Go(func() error {
			resp, err := r.client.RecallMemories(ctx, &hippo.RecallMemoriesRequest{Ids: chunk})
			if err != nil {

				return fmt.Errorf("recalling %d memories: %w", len(chunk), err)
			}

			mu.Lock()
			r.stats.Recalled += len(resp.GetMemories())
			mu.Unlock()

			return nil
		})
	}

	return group.Wait()
}

// Survivors reports which of the trace's memories the store still holds, as trace indices.
//
// It asks with ExplainConsolidation rather than by paging the store or by recalling the ids. Paging
// would read every surviving body for nothing; recalling would REINFORCE, resetting the very decay
// clocks the measurement is about. Explain answers about ids the caller already knows, returns no
// bodies, and - the property this relies on - reports one valuation per memory FOUND, so an id it
// omits is an id the store no longer holds.
func Survivors(ctx context.Context, client Client, tr *trace.Trace, workers int) (map[int]bool, error) {
	byID := make(map[string]int, len(tr.Memories))
	ids := make([]string, 0, len(tr.Memories))

	for i := range tr.Memories {
		byID[tr.Memories[i].ID] = i
		ids = append(ids, tr.Memories[i].ID)
	}

	if workers <= 0 {
		workers = 8
	}

	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(workers)

	var mu sync.Mutex

	out := map[int]bool{}

	for i := 0; i < len(ids); i += explainBatchSize {
		last := i + explainBatchSize

		if last > len(ids) {
			last = len(ids)
		}

		chunk := ids[i:last]

		group.Go(func() error {
			resp, err := client.ExplainConsolidation(ctx, &hippo.ExplainConsolidationRequest{MemoryIds: chunk})
			if err != nil {

				return fmt.Errorf("explaining %d memories: %w", len(chunk), err)
			}

			mu.Lock()

			for _, v := range resp.GetValuations() {
				if i, ok := byID[v.GetId()]; ok {
					out[i] = true
				}
			}

			mu.Unlock()

			return nil
		})
	}

	if err := group.Wait(); err != nil {

		return nil, err
	}

	return out, nil
}

// SortedIndices is a stable ordering helper for reporting.
func SortedIndices(in map[int]bool) []int {
	out := make([]int, 0, len(in))

	for k := range in {
		out = append(out, k)
	}

	sort.Ints(out)

	return out
}

func abs(v float64) float64 {
	if v < 0 {

		return -v
	}

	return v
}

// Rankings runs every held-out question against an instance and returns what it ranked, best first,
// as trace memory indices.
//
// Two things it must not do. It must not REINFORCE - SearchMemories can route its matches through
// recall, which would reset the decay clocks the measurement is about - so reinforce is left false
// explicitly rather than by omission. And it must not ask shallowly: a bounded arm's ranking is
// computed by filtering this list down to what that arm retained, so a list truncated at the answer
// depth would leave nothing to promote once the rows above the needle were forgotten. depth is
// therefore the search limit, and wants to be several times the k finally reported.
func Rankings(ctx context.Context, client Client, tr *trace.Trace, depth int, workers int) ([][]int, error) {
	byID := make(map[string]int, len(tr.Memories))

	for i := range tr.Memories {
		byID[tr.Memories[i].ID] = i
	}

	if workers <= 0 {
		workers = 8
	}

	out := make([][]int, len(tr.Retrievals))

	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(workers)

	for i := range tr.Retrievals {
		at := i

		group.Go(func() error {
			resp, err := client.SearchMemories(ctx, &hippo.SearchMemoriesRequest{
				Query:     tr.Retrievals[at].Query,
				Limit:     int32(depth),
				Reinforce: false,
			})
			if err != nil {

				return fmt.Errorf("searching for question %d: %w", at, err)
			}

			ranked := make([]int, 0, len(resp.GetMemories()))

			for _, v := range resp.GetMemories() {
				if i, ok := byID[v.GetId()]; ok {
					ranked = append(ranked, i)
				}
			}

			out[at] = ranked

			return nil
		})
	}

	if err := group.Wait(); err != nil {

		return nil, err
	}

	return out, nil
}
