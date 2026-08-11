// Command logs is a log-shaped data generator for Hippocampus. Each synthetic log line becomes a
// memory whose significance is derived from the line's level — routine DEBUG/INFO lines are
// low-significance and are the first the sleep cycle forgets, while ERROR/FATAL lines are
// high-significance and survive — and is tagged with its emitting service through the group label.
// Lines are grouped into events per service per day, so an event is "one service's activity for a
// day". It is the log-file counterpart to the narrative (book) generator, exercising the same
// contract plus the group field and significance-driven retention.
package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"

	hippo "github.com/fastbean-au/hippocampus/contract"

	"github.com/fastbean-au/hippocampus-gen/internal/client"
	"github.com/fastbean-au/hippocampus-gen/internal/link"
	"github.com/fastbean-au/hippocampus-gen/internal/oidc"
	"github.com/fastbean-au/hippocampus-gen/internal/pace"
)

// level is one log severity, its relative frequency, the base memory significance a line at that
// level is stored with, and its severity ordinal. The significances span the int32 range so the
// decay/eviction machinery treats DEBUG noise and FATAL errors very differently; the rank is what
// the association rules in link.go compare against, since a level arrives by value and cannot be
// placed in the table by index.
type level struct {
	name         string
	weight       float64
	significance int32
	rank         int
}

// Severity ranks worth naming: the thresholds at which a line starts declaring links.
const (
	warnRank  = 3
	errorRank = 4
	fatalRank = 5
)

// levels are ordered least to most severe; weights are a rough real-world mix (mostly INFO/DEBUG,
// rarely FATAL). They need not sum to 1 — pickLevel normalises against their running total.
var levels = []level{
	{"DEBUG", 0.40, 2000, 1},
	{"INFO", 0.38, 6000, 2},
	{"WARN", 0.14, 16000, warnRank},
	{"ERROR", 0.07, 28000, errorRank},
	{"FATAL", 0.01, 32000, fatalRank},
}

// services are the emitting components; each becomes a group label on its memories and events.
var services = []string{"auth", "api-gateway", "database", "worker", "cache"}

// messageTemplates give each level a handful of realistic-looking lines. A %s is filled with a
// random short token so repeated lines are not identical.
var messageTemplates = map[string][]string{
	"DEBUG": {"cache lookup key=%s hit", "entering handler %s", "config key %s resolved", "span %s started"},
	"INFO":  {"request %s completed 200", "job %s finished", "connection to %s established", "user %s authenticated"},
	"WARN":  {"slow query on %s", "retrying %s", "queue %s above soft limit", "token for %s near expiry"},
	"ERROR": {"failed to reach %s", "unhandled exception in %s", "connection to %s refused", "write to %s timed out"},
	"FATAL": {"panic in %s", "out of memory in %s", "cannot bind %s", "corrupt state in %s"},
}

var tokens = []string{"u4821", "req-9f3a", "shard-3", "tx-71c", "sess-0b2", "node-a", "batch-12", "conn-88"}

func main() {
	pflag.StringP("server_address", "s", "localhost:50051", "address of hippocampus server")
	pflag.IntP("entries", "n", 5000, "number of log lines (memories) to generate (one-shot mode)")
	pflag.IntP("days", "d", 30, "how many days of history to spread the lines across, ending shortly before now (one-shot mode)")
	pflag.Bool("live", false, "trickle new lines continuously at the current time instead of loading a fixed back-dated batch")
	pflag.Int("rate", 60, "with --live, approximate lines per minute to emit")
	pflag.Duration("duration", 0, "with --live, how long to run before stopping (0 = until interrupted)")
	pflag.Bool("links", true, "associate lines sharing a correlation token, chain each service's errors and its daily events (--links=false emits them unlinked)")
	pflag.String("group", "", "group label for every record, overriding the per-service default; set this to the label a group-scoped token carries (the service name is recorded as metadata either way)")
	client.RegisterAuthFlags(pflag.CommandLine)
	pflag.Parse()

	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		fmt.Printf("ERROR: %s\n", err.Error())
		os.Exit(1)
	}

	// Build the dial options, adding the bearer-token interceptor when auth is configured. All viper
	// reads stay here in main; the client package takes the resolved values.
	opts, err := client.DialOptions(context.Background(), oidc.AuthConfig{
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
		fmt.Printf("ERROR: %s\n", err.Error())
		os.Exit(1)
	}

	conn, err := grpc.NewClient(viper.GetString("server_address"), opts...)
	if err != nil {
		fmt.Printf("ERROR: %s\n", err.Error())
		os.Exit(1)
	}
	defer conn.Close()

	client := hippo.NewHippocampusClient(conn)

	// All viper reads stay in main; the emitter takes the resolved values.
	cfg := emitConfig{
		links: viper.GetBool("links"),
		group: viper.GetString("group"),
	}

	if viper.GetBool("live") {
		// A continuous trickle at the current time: lines age in real wall-clock, and the service's
		// own sleep cycle plus capacity eviction reap the low-significance noise as the store fills.
		// Runs until --duration elapses or the process is interrupted.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		if d := viper.GetDuration("duration"); d > 0 {
			var cancel context.CancelFunc

			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}

		executeLive(ctx, client, viper.GetInt("rate"), cfg)

		return
	}

	execute(client, viper.GetInt("entries"), viper.GetInt("days"), cfg)
}

// dayNanos is the bucket a line's event is chosen by: one event per service per day.
const dayNanos = int64(24 * time.Hour)

// serviceState tracks the open per-service daily event so lines for the same service and day attach
// to one event, and the event is ended when the day rolls over.
type serviceState struct {
	eventId string
	day     int64
	endTs   int64
}

// line is one synthetic log line before it is written: what emitted it, how bad it is, the
// correlation token its message carries (the request it belongs to), and its ordinal in the run,
// which is the clock the association threads age against.
type line struct {
	service string
	level   level
	token   string
	seq     int64
}

// emitter is everything one run's lines are written through: the client, the open per-service daily
// events, the association threads, and the line counter those threads age against. A one-shot batch
// and a live trickle each build one, so they differ only in the timestamps they hand it.
// emitConfig carries the shaping options both the one-shot and live paths take. It is a struct
// rather than two more parameters because execute/executeLive already sit at the project's
// four-parameter limit.
type emitConfig struct {
	// links chains lines sharing a correlation token, each service's errors, and its daily events.
	links bool

	// group overrides the group label stamped on every record. Empty keeps the historical
	// behaviour - the service name doubles as the group - which is what a store with no group
	// scoping wants.
	//
	// It exists because group is no longer only a label: a service issuing group-scoped tokens
	// lets a token write only the groups it holds, so service-as-group and a scoped token are
	// mutually exclusive and every write would be refused. Setting this to the token's own label
	// resolves that, and the service name survives as metadata regardless (see emit).
	group string
}

type emitter struct {
	client  hippo.HippocampusClient
	states  map[string]*serviceState
	threads *threads
	cfg     emitConfig
	seq     int64
}

func newEmitter(client hippo.HippocampusClient, cfg emitConfig) *emitter {
	return &emitter{
		client:  client,
		states:  make(map[string]*serviceState, len(services)),
		threads: newThreads(),
		cfg:     cfg,
	}
}

// groupFor resolves the group label for a service's records: the configured override when there is
// one, otherwise the service name, which is what this generator has always used.
func (e *emitter) groupFor(service string) string {
	if e.cfg.group != "" {
		return e.cfg.group
	}

	return service
}

func execute(client hippo.HippocampusClient, entries int, days int, cfg emitConfig) {
	ctx := context.Background()

	if entries < 1 {
		entries = 1
	}

	if days < 1 {
		days = 1
	}

	window := int64(days) * dayNanos

	// Lay the lines across the window ending an hour before now, so no timestamp is future-dated
	// (the service rejects those), stepping forward by at most one increment per line.
	increment := window / int64(entries)
	ts := time.Now().Add(-time.Duration(window) - time.Hour).UnixNano()

	e := newEmitter(client, cfg)
	stored := 0

	for i := 0; i < entries; i++ {
		ts += step(increment)

		if e.emit(ctx, ts) {
			stored++
		}
	}

	e.closeEvents(ctx)

	fmt.Printf("stored %d log lines across %d services over %d days\n", stored, len(services), days)
}

// executeLive trickles new lines at the current time until ctx is cancelled (by --duration or a
// SIGINT/SIGTERM), pacing to roughly rate lines per minute. It never back-dates, so the memories age
// in real wall-clock and the service's sleep cycle reaps them as they fall below the threshold.
func executeLive(ctx context.Context, client hippo.HippocampusClient, rate int, cfg emitConfig) {
	if rate < 1 {
		rate = 1
	}

	// One Wait per line across a one-minute window gives roughly rate lines per minute.
	pacer := pace.NewPacer(time.Minute, rate)

	e := newEmitter(client, cfg)
	stored := 0

	fmt.Printf("trickling ~%d log lines/min across %d services (interrupt to stop)\n", rate, len(services))

	for ctx.Err() == nil {
		if e.emit(ctx, time.Now().UnixNano()) {
			stored++
		}

		if err := pacer.Wait(ctx); err != nil {

			break
		}
	}

	// Close the open daily events on the way out, using a fresh context since ctx is now cancelled.
	e.closeEvents(context.Background())

	fmt.Printf("stored %d log lines before stopping\n", stored)
}

// emit stores one log line at ts: it picks a service, a level and the correlation token its message
// carries, attaches the line to the service's current daily event, declares whatever associations
// that line has earned, and stores it. It reports whether the store succeeded.
func (e *emitter) emit(ctx context.Context, ts int64) bool {
	e.seq++

	l := line{
		service: services[rand.Intn(len(services))],
		level:   pickLevel(),
		token:   tokens[rand.Intn(len(tokens))],
		seq:     e.seq,
	}

	m := &hippo.Memory{
		EventId:      e.currentEvent(ctx, l.service, ts),
		Group:        e.groupFor(l.service),
		Significance: jitterSignificance(l.level.significance),
		TimeStamp:    ts,
		Body:         fmt.Sprintf("[%s] %s", l.level.name, renderMessage(l.level.name, l.token)),

		// The service and level go in as metadata as well as (in the default configuration) as the
		// group. Metadata is where multi-dimensional classification belongs - level could never
		// have been a group alongside the service - and it is what keeps the service filterable
		// when --group has taken the group over for access scoping.
		Metadata: map[string]string{
			"service": l.service,
			"level":   l.level.name,
		},
	}

	if e.cfg.links {
		m.Links = e.threads.memoryLinks(l)
	}

	id, err := link.StoreMemory(ctx, e.client, m)
	if err != nil {
		fmt.Printf("ERROR storing memory: %s\n", err.Error())
	}

	if e.cfg.links {
		e.threads.advanceMemory(l, id)
	}

	return err == nil
}

// currentEvent returns the event id the line should attach to, creating a new per-service daily
// event (linked to that service's previous day, and ending the previous one) when the day rolls
// over. Because ts increases monotonically, each service's day buckets are visited in order.
func (e *emitter) currentEvent(ctx context.Context, service string, ts int64) string {
	day := ts / dayNanos

	st, ok := e.states[service]

	if ok && st.day == day {
		st.endTs = ts

		return st.eventId
	}

	if ok && st.eventId != "" {
		e.endEvent(ctx, st.eventId, st.endTs, service)
	}

	name := fmt.Sprintf("%s — %s", service, time.Unix(0, ts).UTC().Format("2006-01-02"))

	ev := &hippo.Event{
		TimeStart:    ts,
		Significance: jitterSignificance(12000),
		Name:         name,
		Description:  fmt.Sprintf("%s service activity for %s", service, time.Unix(0, ts).UTC().Format("2006-01-02")),
		Group:        e.groupFor(service),

		Metadata: map[string]string{
			"service": service,
			"day":     time.Unix(0, ts).UTC().Format("2006-01-02"),
		},
	}

	if e.cfg.links {
		ev.Links = e.threads.eventLinks(service, day)
	}

	id, err := link.StoreEvent(ctx, e.client, ev)
	if err != nil {
		fmt.Printf("ERROR storing event: %s\n", err.Error())

		return ""
	}

	if e.cfg.links {
		e.threads.advanceEvent(service, id, day)
	}

	e.states[service] = &serviceState{eventId: id, day: day, endTs: ts}

	return id
}

// closeEvents ends every still-open daily event at the last line it saw.
func (e *emitter) closeEvents(ctx context.Context) {
	for service, st := range e.states {
		e.endEvent(ctx, st.eventId, st.endTs, service)
	}
}

// endEvent sets an event's end time, tolerating an empty id (an event whose creation failed).
func (e *emitter) endEvent(ctx context.Context, eventId string, endTs int64, service string) {
	if eventId == "" {
		return
	}

	ee := &hippo.EndEventRequest{
		Id:      eventId,
		TimeEnd: endTs,
	}

	if _, err := e.client.EndEvent(ctx, ee); err != nil {
		fmt.Printf("ERROR ending event for %s: %s\n", service, err.Error())
	}
}

// pickLevel chooses a level weighted by its frequency.
func pickLevel() level {
	total := 0.0
	for _, l := range levels {
		total += l.weight
	}

	r := rand.Float64() * total

	for _, l := range levels {
		r -= l.weight

		if r <= 0 {
			return l
		}
	}

	return levels[len(levels)-1]
}

// renderMessage fills a random template for the level with the line's correlation token. The token
// is chosen by the caller rather than here because it is also the key the request trace is threaded
// on - the same token in two lines is what says they belong to one request.
func renderMessage(levelName string, token string) string {
	templates := messageTemplates[levelName]
	tmpl := templates[rand.Intn(len(templates))]

	return fmt.Sprintf(tmpl, token)
}

// jitterSignificance spreads a base significance by ±1500, clamped to the valid 1..32767 range, so
// lines at the same level are not all identical.
func jitterSignificance(base int32) int32 {
	sig := base + int32(rand.Intn(3001)) - 1500

	if sig < 1 {
		sig = 1
	}

	if sig > 32767 {
		sig = 32767
	}

	return sig
}

// step returns a forward time increment in [base/2, base] nanoseconds, so timestamps stay strictly
// increasing and lightly jittered while never advancing more than base per call — keeping the
// accumulated timeline inside the pre-sized window and out of the future.
func step(base int64) int64 {
	if base < 2 {
		return 1
	}

	return base/2 + rand.Int63n(base/2) + 1
}
