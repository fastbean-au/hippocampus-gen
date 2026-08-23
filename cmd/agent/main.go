// Command agent runs the retention benchmark: it generates an agent workload from the fitted
// parameters, replays it into a live Hippocampus instance in compressed simulated time, and then
// asks what the store still holds of everything the agent turned out to need afterwards.
//
// The result is a comparison at equal store size against the standard cache-replacement baselines -
// LRU, LFU, FIFO, a static priority, and random as the floor - because a forgetting store IS a cache
// replacement policy and that is how one is evaluated.
//
// Two instances are involved. The one at --server_address is the subject: bounded, running real
// sleep cycles, and measured directly. The one at --control_address keeps everything, and is both
// the ceiling and the search oracle the simulated baselines are ranked from. Running without a
// control is supported for a quick local pass, at the cost of the ceiling arm.
//
//	# a quick local pass against one SQLite instance
//	go run ./cmd/agent --memories 20000 --days 60 --sim-days-per-wall-minute 20 --dry-run
//
//	# the real thing, bounded instance plus unbounded control
//	go run ./cmd/agent -s localhost:50051 --control_address localhost:50052 \
//	  --memories 200000 --days 180 --sim-days-per-wall-minute 1 --out results.json
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"

	hippo "github.com/fastbean-au/hippocampus/contract"

	"github.com/fastbean-au/hippocampus-gen/internal/client"
	"github.com/fastbean-au/hippocampus-gen/internal/fit"
	"github.com/fastbean-au/hippocampus-gen/internal/oidc"
	"github.com/fastbean-au/hippocampus-gen/internal/params"
	"github.com/fastbean-au/hippocampus-gen/internal/replay"
	"github.com/fastbean-au/hippocampus-gen/internal/score"
	"github.com/fastbean-au/hippocampus-gen/internal/trace"
)

func main() {
	registerFlags()
	pflag.Parse()

	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		fail(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fail(err)
	}
}

func registerFlags() {
	pflag.StringP("server_address", "s", "localhost:50051", "the bounded instance under test")
	pflag.String("control_address", "", "an unbounded control instance, the ceiling arm and search oracle (empty runs without one)")
	pflag.String("params", "", "a fitted parameter file (see cmd/agentfit); empty uses the committed parameters compiled into the binary")

	pflag.Int("memories", 20000, "how many memories the trace stores")
	pflag.Float64("days", 60, "simulated days the trace spans")
	pflag.Int("agents", 4, "independent agents sharing the store, each a group label")
	pflag.Int64("seed", 1, "trace seed; the same seed always produces the same trace")

	pflag.String("importance-shape", "measured", "distribution latent importance is drawn from: 'measured' (the corpus's fitted mutation-share ladder) or 'graded' (uniform, preserving the measured independence from access but restoring spread)")
	pflag.Float64("must-keep-share", 0.05, "how many \"what matters regardless of access\" questions to ask, as a fraction of the store")
	pflag.String("significance-signal", "importance", "what write-time significance is a proxy for: 'importance' (latent worth, drawn independently of access), 'longevity' (distinct sessions) or 'frequency' (total recalls). The benchmark reports both; see internal/trace")
	pflag.Float64("significance-noise", 0.3, "how much of write-time significance is noise: 0 makes it an oracle for future recall, 1 makes it meaningless")
	pflag.Float64("link-scale", 0.15, "scales the fitted co-occurrence density down to a per-memory link count")
	pflag.Float64("retrieval-horizon-days", 30, "how far past the window a later reference may land and still count as needed")

	pflag.Int("terms-per-memory", 4, "topic terms carried by each memory")
	pflag.Int("memories-per-term", 100, "average memories sharing any one term; lower makes a term more discriminating")
	pflag.Int("query-terms", 3, "terms a question asks with")

	pflag.String("significance-scale", "log", "how the latent signal maps onto significance: 'log' (geometric, the default, and what docs/consolidation.md recommends since every decay method compares significance as a ratio) or 'linear' (evenly spread, which the store's own maths then compresses at the top)")
	pflag.Int32("min-significance", 1000, "significance floor")
	pflag.Int32("max-significance", 30000, "significance ceiling")
	pflag.Int("body-bytes", 256, "approximate memory body size")

	pflag.Float64("sim-days-per-wall-minute", 20, "replay speed; the instance's consolidation.unitsOfAgeInDays must agree and is checked")
	pflag.Int("workers", 16, "concurrent RPCs")
	pflag.Duration("tick", time.Second, "wall-clock batch width")
	pflag.String("group", "", "prefix for every group label written, so runs can share a store")
	pflag.Bool("events", true, "write one event per session and file memories under it")

	pflag.Int("top-k", 10, "the k in retrieval@k")
	pflag.String("curve", "", "comma-separated store sizes (as fractions, e.g. 0.05,0.1,0.2,0.4) to score every baseline at")
	pflag.Int("oracle-depth", 200, "how deep to rank each question; wants to be several times --top-k")

	pflag.Bool("live", false, "run continuously as a demonstration writer: generate a trace, replay it, generate the next, forever. No scoring, no control instance.")
	pflag.String("flat-address", "", "with --live, a second instance receiving byte-for-byte identical memories but a CONSTANT significance - the same workload stored by a deployment that never sets one")
	pflag.Int32("flat-significance", 10000, "the constant significance written to --flat-address")

	pflag.String("out", "", "write the result JSON here (empty prints the table only)")
	pflag.Bool("dry-run", false, "generate and describe the trace without contacting any service")
	pflag.Bool("force", false, "run even if the instance's decay clock disagrees with the replay speed")

	client.RegisterAuthFlags(pflag.CommandLine)
}

func run(ctx context.Context) error {
	params, err := loadParams(viper.GetString("params"))
	if err != nil {

		return err
	}

	if viper.GetBool("live") {

		return runLive(ctx, params)
	}

	tr, err := trace.Generate(traceConfig(params))
	if err != nil {

		return fmt.Errorf("generating the trace: %w", err)
	}

	replayCfg := replay.Config{
		SimDaysPerWallMinute: viper.GetFloat64("sim-days-per-wall-minute"),
		Workers:              viper.GetInt("workers"),
		Tick:                 viper.GetDuration("tick"),
		Group:                viper.GetString("group"),
		Events:               viper.GetBool("events"),
	}

	describe(tr, replayCfg)

	if viper.GetBool("dry-run") {
		fmt.Println("\ndry run: nothing was sent to any service")

		return nil
	}

	subject, err := dial(ctx, viper.GetString("server_address"))
	if err != nil {

		return err
	}

	defer subject.close()

	control, err := dialControl(ctx)
	if err != nil {

		return err
	}

	if control != nil {
		defer control.close()
	}

	if err := verify(ctx, subject.client, tr, replayCfg); err != nil {

		return err
	}

	subjectRun, err := drive(ctx, tr, replayCfg, subject, control)
	if err != nil {

		return err
	}

	// Every recency-based baseline is scored on the timings the replay actually produced, not on the
	// trace's simulated schedule - otherwise it reads a sharper clock than the store is allowed to
	// see, and wins on recency for a reason that is about the harness rather than the policy.
	inputs := score.Inputs{Seed: viper.GetInt64("seed"), Touched: subjectRun.Touched()}

	results, oracle, err := measure(ctx, tr, subject, control, inputs)
	if err != nil {

		return err
	}

	report(tr, results)

	curves := curve(tr, oracle, inputs)
	reportCurve(tr, curves)

	return write(viper.GetString("out"), tr, results, curves)
}

// runLive drives an endless workload into one or two instances, for a hosted demonstration rather
// than a measurement. It never scores, never reads survivors back, and never needs a control.
//
// Each pass generates a fresh trace and replays it; the pass number becomes the trace's id prefix,
// because ids are assigned from an index and a second trace would otherwise reuse the first's -
// turning every write into an update and leaving the store's size flat forever.
//
// With --flat-address the same memories go to a second instance with a constant significance. Both
// stores then hold byte-for-byte identical bodies, ids, events and links, arriving at the same
// moments and recalled at the same moments; the ONLY difference between them is whether anything
// ever said which memories mattered. That is the comparison worth showing, and it needs one writer
// rather than two so the two workloads cannot drift apart.
func runLive(ctx context.Context, params fit.Params) error {
	primary, err := dial(ctx, viper.GetString("server_address"))
	if err != nil {

		return err
	}

	defer primary.close()

	var flat *connection

	if address := viper.GetString("flat-address"); address != "" {
		if flat, err = dial(ctx, address); err != nil {

			return err
		}

		defer flat.close()
	}

	cfg := replay.Config{
		SimDaysPerWallMinute: viper.GetFloat64("sim-days-per-wall-minute"),
		Workers:              viper.GetInt("workers"),
		Tick:                 viper.GetDuration("tick"),
		Group:                viper.GetString("group"),
		Events:               viper.GetBool("events"),
	}

	if _, err := replay.New(primary.client, &trace.Trace{}, cfg).Verify(ctx); err != nil && !viper.GetBool("force") {

		return fmt.Errorf("%w\n\nset consolidation.unitsOfAgeInDays to %.9g, or pass --force to run anyway",
			err, cfg.RequiredUnitsOfAgeInDays())
	}

	for pass := 0; ; pass++ {
		if ctx.Err() != nil {

			return nil
		}

		tc := traceConfig(params)
		tc.Seed = viper.GetInt64("seed") + int64(pass)
		tc.IDPrefix = fmt.Sprintf("p%03d-", pass)

		tr, err := trace.Generate(tc)
		if err != nil {

			return fmt.Errorf("generating pass %d: %w", pass, err)
		}

		fmt.Printf("pass %d: %d memories over %.0f simulated days (%s)\n",
			pass, len(tr.Memories), tr.End.Sub(tr.Start).Hours()/24,
			replay.New(primary.client, tr, cfg).Duration())

		if err := livePass(ctx, tr, cfg, primary, flat); err != nil {
			if ctx.Err() != nil {

				return nil
			}

			// A demonstration writer outlives transient failures rather than exiting and taking the
			// store's only source of new memories with it - which is precisely the outage that goes
			// unnoticed for hours.
			fmt.Printf("pass %d failed, continuing: %s\n", pass, err.Error())
		}
	}
}

// livePass replays one generated trace into both instances at once.
func livePass(ctx context.Context, tr *trace.Trace, cfg replay.Config, primary *connection, flat *connection) error {
	done := make(chan error, 1)

	if flat != nil {
		flatCfg := cfg
		flatCfg.FlatSignificance = viper.GetInt32("flat-significance")

		go func() {
			_, err := replay.New(flat.client, tr, flatCfg).Run(ctx)
			done <- err
		}()
	}

	stats, err := replay.New(primary.client, tr, cfg).Run(ctx)
	if err != nil {

		return fmt.Errorf("primary: %w", err)
	}

	fmt.Printf("  stored %d, recalled %d, links %d\n", stats.Stored, stats.Recalled, stats.Links)

	if flat == nil {

		return nil
	}

	if err := <-done; err != nil {

		return fmt.Errorf("flat: %w", err)
	}

	return nil
}

// connection pairs a dialled client with the connection to close.
type connection struct {
	client replay.Client
	conn   *grpc.ClientConn
}

func (c *connection) close() {
	if c == nil || c.conn == nil {

		return
	}

	if err := c.conn.Close(); err != nil {
		fmt.Printf("WARNING: closing the connection: %s\n", err.Error())
	}
}

func dial(ctx context.Context, address string) (*connection, error) {
	opts, err := client.DialOptions(ctx, oidc.AuthConfig{
		Token: viper.GetString("token"),
		ClientCredentialsConfig: oidc.ClientCredentialsConfig{
			Issuer:       viper.GetString("oidc-issuer"),
			TokenURL:     viper.GetString("oidc-token-url"),
			ClientID:     viper.GetString("oidc-client-id"),
			ClientSecret: viper.GetString("oidc-client-secret"),
			Scope:        viper.GetString("oidc-scope"),
			Audience:     viper.GetString("oidc-audience"),
		},
	})
	if err != nil {

		return nil, err
	}

	conn, err := grpc.NewClient(address, opts...)
	if err != nil {

		return nil, fmt.Errorf("dialling %s: %w", address, err)
	}

	return &connection{client: hippo.NewHippocampusClient(conn), conn: conn}, nil
}

func dialControl(ctx context.Context) (*connection, error) {
	address := viper.GetString("control_address")
	if address == "" {
		fmt.Println("no control instance: running without the keep-everything ceiling, and ranking the simulated baselines from the bounded store")

		return nil, nil
	}

	return dial(ctx, address)
}

func loadParams(path string) (fit.Params, error) {
	if path == "" {

		return params.Default()
	}

	var out fit.Params

	data, err := os.ReadFile(path)
	if err != nil {

		return out, fmt.Errorf("reading %s: %w (run cmd/agentfit to produce one)", path, err)
	}

	if err := json.Unmarshal(data, &out); err != nil {

		return out, fmt.Errorf("parsing %s: %w", path, err)
	}

	return out, nil
}

func traceConfig(params fit.Params) trace.Config {
	return trace.Config{
		Params:               params,
		Seed:                 viper.GetInt64("seed"),
		Memories:             viper.GetInt("memories"),
		Days:                 viper.GetFloat64("days"),
		Agents:               viper.GetInt("agents"),
		SignificanceSignal:   trace.SignificanceSignal(viper.GetString("significance-signal")),
		ImportanceShape:      trace.ImportanceShape(viper.GetString("importance-shape")),
		MustKeepShare:        viper.GetFloat64("must-keep-share"),
		SignificanceNoise:    viper.GetFloat64("significance-noise"),
		LinkScale:            viper.GetFloat64("link-scale"),
		RetrievalHorizonDays: viper.GetFloat64("retrieval-horizon-days"),
		TermsPerMemory:       viper.GetInt("terms-per-memory"),
		MemoriesPerTerm:      viper.GetInt("memories-per-term"),
		QueryTerms:           viper.GetInt("query-terms"),
		SignificanceScale:    trace.SignificanceScale(viper.GetString("significance-scale")),
		MinSignificance:      viper.GetInt32("min-significance"),
		MaxSignificance:      viper.GetInt32("max-significance"),
		BodyBytes:            viper.GetInt("body-bytes"),
	}
}

// verify refuses a run whose instance is configured for a different decay rate than the replay
// speed implies, since the alternative is spending hours measuring something nobody chose.
func verify(ctx context.Context, c replay.Client, tr *trace.Trace, cfg replay.Config) error {
	settings, err := replay.New(c, tr, cfg).Verify(ctx)

	if err != nil && !viper.GetBool("force") {

		return fmt.Errorf("%w\n\nset consolidation.unitsOfAgeInDays to %.9g, or pass --force to run anyway",
			err, cfg.RequiredUnitsOfAgeInDays())
	}

	if err != nil {
		fmt.Printf("WARNING: %s (continuing under --force)\n", err)

		return nil
	}

	fmt.Printf("instance: method %d, aggressiveness %g, unitsOfAgeInDays %g, capacity %d memories / %d bytes\n",
		settings.GetMethod(), settings.GetAggressiveness(), settings.GetUnitsOfAgeInDays(),
		settings.GetCapacityMemories(), settings.GetCapacityBytes())

	if settings.GetCapacityMemories() == 0 && settings.GetCapacityBytes() == 0 {
		fmt.Println("WARNING: the instance has no capacity target, so eviction will never run and the bounded arm may not be bounded")
	}

	return nil
}

// drive replays the trace into the subject and, when present, the control. Both get the identical
// trace: the control is the same workload against a store that forgets nothing.
func drive(ctx context.Context, tr *trace.Trace, cfg replay.Config, subject *connection, control *connection) (*replay.Replay, error) {
	subjectRun := replay.New(subject.client, tr, cfg)

	fmt.Printf("\nreplaying %.0f simulated days over %s...\n", tr.End.Sub(tr.Start).Hours()/24, subjectRun.Duration())

	done := make(chan error, 1)

	if control != nil {
		go func() {
			stats, err := replay.New(control.client, tr, cfg).Run(ctx)

			if err == nil {
				fmt.Printf("control:  %d stored, %d recalled\n", stats.Stored, stats.Recalled)
			}

			done <- err
		}()
	}

	stats, err := subjectRun.Run(ctx)
	if err != nil {

		return nil, fmt.Errorf("replaying into the subject: %w", err)
	}

	fmt.Printf("subject:  %d stored, %d rejected, %d recalled, %d links (%d dropped), in %s\n",
		stats.Stored, stats.Rejected, stats.Recalled, stats.Links, stats.LinksLost, stats.Elapsed.Round(time.Second))

	if control == nil {

		return subjectRun, nil
	}

	if err := <-done; err != nil {

		return nil, fmt.Errorf("replaying into the control: %w", err)
	}

	return subjectRun, nil
}

// measure asks both stores what they have, and scores every arm at the size the subject settled at.
func measure(ctx context.Context, tr *trace.Trace, subject *connection, control *connection, inputs score.Inputs) ([]score.Result, [][]int, error) {
	workers := viper.GetInt("workers")
	depth := viper.GetInt("oracle-depth")

	fmt.Println("\nmeasuring...")

	survivors, err := replay.Survivors(ctx, subject.client, tr, workers)
	if err != nil {

		return nil, nil, fmt.Errorf("reading the subject's survivors: %w", err)
	}

	// The subject's own ranking, over its own surviving store - the product measured rather than
	// approximated.
	measured, err := replay.Rankings(ctx, subject.client, tr, depth, workers)
	if err != nil {

		return nil, nil, fmt.Errorf("ranking against the subject: %w", err)
	}

	oracle := measured

	if control != nil {
		oracle, err = replay.Rankings(ctx, control.client, tr, depth, workers)
		if err != nil {

			return nil, nil, fmt.Errorf("ranking against the control: %w", err)
		}
	}

	// Every baseline is given exactly the budget the subject settled at, so the comparison is at
	// equal store size - a number the run produced rather than one chosen here.
	budget := len(survivors)

	arms := []score.Arm{{
		Name:     "hippocampus",
		Why:      "the store under test: decay, recall reinforcement, links and capacity eviction",
		Retained: survivors,
		Ranked:   measured,
	}}

	for _, v := range score.Baselines(inputs) {
		if v.Name == "keep-everything" && control == nil {
			continue
		}

		arms = append(arms, score.Arm{Name: v.Name, Why: v.Why, Retained: v.Retain(tr, budget)})
	}

	return score.Score(tr, arms, oracle, viper.GetInt("top-k")), oracle, nil
}

// curve scores every baseline across the requested ladder of store sizes.
func curve(tr *trace.Trace, oracle [][]int, inputs score.Inputs) map[string][]score.Result {
	spec := viper.GetString("curve")
	if spec == "" {

		return nil
	}

	var budgets []int

	for _, v := range strings.Split(spec, ",") {
		share, err := strconv.ParseFloat(strings.TrimSpace(v), 64)

		if err != nil || share <= 0 || share > 1 {
			fmt.Printf("WARNING: ignoring curve point %q - wanted a fraction between 0 and 1\n", v)

			continue
		}

		budgets = append(budgets, int(share*float64(len(tr.Memories))))
	}

	if len(budgets) == 0 {

		return nil
	}

	return score.Curve(tr, oracle, budgets, score.CurveOptions{Inputs: inputs, K: viper.GetInt("top-k")})
}

// reportCurve prints each baseline's retention across store sizes, overall and on the long tail.
//
// The two columns say different things and the gap between them is the finding. Overall retention
// rises smoothly with budget for every policy. The long tail is where a recency window's hard cutoff
// shows: its score there tracks whether its window happens to be wider than the question, and it
// collapses the moment it is not.
func reportCurve(tr *trace.Trace, curves map[string][]score.Result) {
	if len(curves) == 0 {

		return
	}

	names := make([]string, 0, len(curves))

	for k := range curves {
		names = append(names, k)
	}

	sort.Strings(names)

	fmt.Println("\nRetention against store size - overall, and for answers last touched over a week before the close:")
	fmt.Printf("\n  %-16s %26s %26s\n", "", "overall", "long tail (over 7d)")
	fmt.Printf("  %-16s", "policy")

	for _, v := range curves[names[0]] {
		fmt.Printf(" %7.0f%%", 100*v.KeptShare)
	}

	fmt.Print("  ")

	for _, v := range curves[names[0]] {
		fmt.Printf(" %7.0f%%", 100*v.KeptShare)
	}

	fmt.Print("\n")

	for _, name := range names {
		fmt.Printf("  %-16s", name)

		for _, v := range curves[name] {
			fmt.Printf(" %7.1f%%", 100*v.Retention)
		}

		fmt.Print("  ")

		for _, v := range curves[name] {
			fmt.Printf(" %7.1f%%", 100*v.ByLag[score.LongTailBucket].Retention)
		}

		fmt.Print("\n")
	}

	fmt.Println()
}

// describe prints what the trace asks of the store before any of it is sent.
func describe(tr *trace.Trace, cfg replay.Config) {
	recalls := 0
	links := 0
	cold := 0

	for _, v := range tr.Memories {
		recalls += v.Recalls
		links += len(v.Links)
	}

	for _, v := range tr.Retrievals {
		if tr.Memories[v.Needle].Recalls == 0 {
			cold++
		}
	}

	fmt.Printf("trace:    %d memories, %d recalls, %d events, %d links, over %.0f simulated days\n",
		len(tr.Memories), recalls, len(tr.Sessions), links, tr.End.Sub(tr.Start).Hours()/24)
	fmt.Printf("question: %d held-out retrievals, of which %d (%.0f%%) name a memory never recalled during the run\n",
		len(tr.Retrievals), cold, 100*float64(cold)/float64(max(len(tr.Retrievals), 1)))
	fmt.Printf("clock:    %g simulated days per wall minute, needing consolidation.unitsOfAgeInDays %.9g\n",
		cfg.SimDaysPerWallMinute, cfg.RequiredUnitsOfAgeInDays())
}

// report prints the result table, most retentive first.
func report(tr *trace.Trace, results []score.Result) {
	fmt.Printf("\nAt equal store size, of %d things the agent needed after the window closed:\n\n", len(tr.Retrievals))
	fmt.Printf("  %-16s %8s %12s %12s\n", "policy", "kept", "retention", fmt.Sprintf("retrieval@%d", viper.GetInt("top-k")))
	fmt.Printf("  %-16s %8s %12s %12s\n", "----------------", "--------", "------------", "------------")

	for _, v := range results {
		fmt.Printf("  %-16s %7.1f%% %11.1f%% %11.1f%%\n",
			v.Name, 100*v.KeptShare, 100*v.Retention, 100*v.Retrieval)
	}

	reportByLag(results)
	reportByKind(results)
}

// reportByKind splits the score by which question was asked.
//
// This is the table the benchmark exists for. "What will be looked up next" is a cache question and
// recency answers it almost by definition; "what matters regardless of access" is the one a memory
// store is for, and no policy reading only the access log can see it at all. A single averaged
// number hides the difference, which is how an earlier version of this benchmark concluded a recency
// window was the best policy available.
func reportByKind(results []score.Result) {
	if len(results) == 0 || len(results[0].ByKind) == 0 {

		return
	}

	fmt.Println("\nRetention by which question was asked:")
	fmt.Print("\n  policy          ")

	for _, v := range results[0].ByKind {
		fmt.Printf(" %14s", v.Kind)
	}

	fmt.Print("\n  ----------------")

	for range results[0].ByKind {
		fmt.Print(" --------------")
	}

	fmt.Print("\n")

	for _, v := range results {
		fmt.Printf("  %-16s", v.Name)

		for _, k := range v.ByKind {
			if k.Needles == 0 {
				fmt.Printf(" %14s", "-")

				continue
			}

			fmt.Printf(" %13.1f%%", 100*k.Retention)
		}

		fmt.Print("\n")
	}

	fmt.Print("\n  questions       ")

	for _, v := range results[0].ByKind {
		fmt.Printf(" %14d", v.Needles)
	}

	fmt.Println()
}

// reportByLag breaks retention down by how stale each needle was when the window closed.
//
// This is the table worth reading. The aggregate above is dominated by the short buckets simply
// because that is where most questions fall, and a recency policy is close to unbeatable there - "the
// most recently touched" is a restatement of the question. What a bounded store is actually for shows
// up on the right-hand columns, where the thing wanted next week was last touched a fortnight ago.
func reportByLag(results []score.Result) {
	if len(results) == 0 || len(results[0].ByLag) == 0 {

		return
	}

	fmt.Println("\nRetention by how long before the window closed the answer was last touched:")
	fmt.Print("\n  policy          ")

	for _, v := range results[0].ByLag {
		fmt.Printf(" %12s", v.Name)
	}

	fmt.Print("\n  ----------------")

	for range results[0].ByLag {
		fmt.Print(" ------------")
	}

	fmt.Print("\n")

	for _, v := range results {
		fmt.Printf("  %-16s", v.Name)

		for _, b := range v.ByLag {
			if b.Needles == 0 {
				fmt.Printf(" %12s", "-")

				continue
			}

			fmt.Printf(" %11.1f%%", 100*b.Retention)
		}

		fmt.Print("\n")
	}

	fmt.Print("\n  questions       ")

	for _, v := range results[0].ByLag {
		fmt.Printf(" %12d", v.Needles)
	}

	fmt.Println()
}

// result is the on-disk record of a run: enough to reproduce it and enough to argue with it.
type result struct {
	GeneratedAt string         `json:"generated_at"`
	Trace       traceSummary   `json:"trace"`
	Config      trace.Config   `json:"config"`
	Results     []score.Result `json:"results"`

	// Curve is each baseline scored across store sizes, when one was asked for.
	Curve map[string][]score.Result `json:"curve,omitempty"`
}

type traceSummary struct {
	Memories   int     `json:"memories"`
	Recalls    int     `json:"recalls"`
	Sessions   int     `json:"sessions"`
	Retrievals int     `json:"retrievals"`
	Days       float64 `json:"days"`
}

func write(path string, tr *trace.Trace, results []score.Result, curves map[string][]score.Result) error {
	if path == "" {

		return nil
	}

	recalls := 0

	for _, v := range tr.Memories {
		recalls += v.Recalls
	}

	out := result{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Trace: traceSummary{
			Memories:   len(tr.Memories),
			Recalls:    recalls,
			Sessions:   len(tr.Sessions),
			Retrievals: len(tr.Retrievals),
			Days:       tr.End.Sub(tr.Start).Hours() / 24,
		},
		Config:  tr.Config,
		Results: results,
		Curve:   curves,
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {

		return err
	}

	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {

		return fmt.Errorf("writing %s: %w", path, err)
	}

	fmt.Printf("wrote %s\n", path)

	return nil
}

func fail(err error) {
	fmt.Printf("ERROR: %s\n", err.Error())
	os.Exit(1)
}
