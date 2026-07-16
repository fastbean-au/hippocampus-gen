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
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	hippo "github.com/fastbean-au/hippocampus/contract"
)

// level is one log severity, its relative frequency, and the base memory significance a line at
// that level is stored with. The significances span the int32 range so the decay/eviction machinery
// treats DEBUG noise and FATAL errors very differently.
type level struct {
	name         string
	weight       float64
	significance int32
}

// levels are ordered least to most severe; weights are a rough real-world mix (mostly INFO/DEBUG,
// rarely FATAL). They need not sum to 1 — pickLevel normalises against their running total.
var levels = []level{
	{"DEBUG", 0.40, 2000},
	{"INFO", 0.38, 6000},
	{"WARN", 0.14, 16000},
	{"ERROR", 0.07, 28000},
	{"FATAL", 0.01, 32000},
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
	pflag.IntP("entries", "n", 5000, "number of log lines (memories) to generate")
	pflag.IntP("days", "d", 30, "how many days of history to spread the lines across, ending shortly before now")
	pflag.Parse()

	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		fmt.Printf("ERROR: %s\n", err.Error())
		os.Exit(1)
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	conn, err := grpc.NewClient(viper.GetString("server_address"), opts...)
	if err != nil {
		fmt.Printf("ERROR: %s\n", err.Error())
		os.Exit(1)
	}
	defer conn.Close()

	client := hippo.NewHippocampusClient(conn)

	execute(client, viper.GetInt("entries"), viper.GetInt("days"))
}

// serviceState tracks the open per-service daily event so lines for the same service and day attach
// to one event, and the event is ended when the day rolls over.
type serviceState struct {
	eventId string
	day     int64
	endTs   int64
}

func execute(client hippo.HippocampusClient, entries int, days int) {
	ctx := context.Background()

	if entries < 1 {
		entries = 1
	}

	if days < 1 {
		days = 1
	}

	dayNanos := int64(24 * time.Hour)
	window := int64(days) * dayNanos

	// Lay the lines across the window ending an hour before now, so no timestamp is future-dated
	// (the service rejects those), stepping forward by at most one increment per line.
	increment := window / int64(entries)
	ts := time.Now().Add(-time.Duration(window) - time.Hour).UnixNano()

	states := make(map[string]*serviceState, len(services))
	stored := 0

	for i := 0; i < entries; i++ {
		ts += step(increment)

		service := services[rand.Intn(len(services))]
		lvl := pickLevel()

		eventId := currentEvent(ctx, client, states, service, ts, dayNanos)

		body := fmt.Sprintf("[%s] %s", lvl.name, renderMessage(lvl.name))

		m := &hippo.Memory{
			EventId:      eventId,
			Group:        service,
			Significance: jitterSignificance(lvl.significance),
			TimeStamp:    ts,
			Body:         body,
		}

		if _, err := client.StoreMemory(ctx, m); err != nil {
			fmt.Printf("ERROR storing memory: %s\n", err.Error())

			continue
		}

		stored++
	}

	// Close every still-open daily event at the last line it saw.
	for service, st := range states {
		endEvent(ctx, client, st.eventId, st.endTs, service)
	}

	fmt.Printf("stored %d log lines across %d services over %d days\n", stored, len(services), days)
}

// currentEvent returns the event id the line should attach to, creating a new per-service daily
// event (and ending the previous one) when the day rolls over. Because ts increases monotonically,
// each service's day buckets are visited in order.
func currentEvent(ctx context.Context,
	client hippo.HippocampusClient,
	states map[string]*serviceState,
	service string,
	ts int64,
	dayNanos int64,
) string {
	day := ts / dayNanos

	st, ok := states[service]

	if ok && st.day == day {
		st.endTs = ts

		return st.eventId
	}

	if ok && st.eventId != "" {
		endEvent(ctx, client, st.eventId, st.endTs, service)
	}

	name := fmt.Sprintf("%s — %s", service, time.Unix(0, ts).UTC().Format("2006-01-02"))

	e := &hippo.Event{
		TimeStart:    ts,
		Significance: jitterSignificance(12000),
		Name:         name,
		Description:  fmt.Sprintf("%s service activity for %s", service, time.Unix(0, ts).UTC().Format("2006-01-02")),
		Group:        service,
	}

	r, err := client.StoreEvent(ctx, e)
	if err != nil {
		fmt.Printf("ERROR storing event: %s\n", err.Error())

		return ""
	}

	states[service] = &serviceState{eventId: r.GetId(), day: day, endTs: ts}

	return r.GetId()
}

// endEvent sets an event's end time, tolerating an empty id (an event whose creation failed).
func endEvent(ctx context.Context, client hippo.HippocampusClient, eventId string, endTs int64, service string) {
	if eventId == "" {
		return
	}

	ee := &hippo.EndEventRequest{
		Id:      eventId,
		TimeEnd: endTs,
	}

	if _, err := client.EndEvent(ctx, ee); err != nil {
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

// renderMessage fills a random template for the level with a random token.
func renderMessage(levelName string) string {
	templates := messageTemplates[levelName]
	tmpl := templates[rand.Intn(len(templates))]

	return fmt.Sprintf(tmpl, tokens[rand.Intn(len(tokens))])
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
