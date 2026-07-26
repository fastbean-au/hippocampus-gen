package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"

	hippo "github.com/fastbean-au/hippocampus/contract"

	"github.com/fastbean-au/hippocampus-gen/internal/client"
	"github.com/fastbean-au/hippocampus-gen/internal/oidc"
	"github.com/fastbean-au/hippocampus-gen/internal/pace"
)

var (
	//go:embed great_expectations.txt
	data []byte

	//go:embed gtexp.txt
	playData []byte
)

func main() {
	pflag.StringP("server_address", "s", "localhost:50051", "address of hippocampus server")
	pflag.BoolP("summarize", "S", false, "after loading the book, summarise ripe events using the stage-play adaptation")
	pflag.Bool("loop", false, "run continuously, reloading every --period instead of loading the book once")
	pflag.Duration("period", 24*time.Hour, "with --loop, how often to run a cycle")
	pflag.Bool("reset", false, "purge the store at the start of each cycle, for a clean reload each period (needs an admin token)")
	pflag.Duration("pace-window", 0, "spread each load across this wall-clock window instead of bursting (0 = burst)")
	pflag.Bool("live", false, "stamp writes at the current time so they age in real time, instead of back-dating across the book's timeline")
	client.RegisterAuthFlags(pflag.CommandLine)
	pflag.Parse()

	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		fmt.Printf("ERROR: %s\n", err.Error())
		os.Exit(1)
	}

	// Build the dial options, including the bearer-token interceptor when auth is configured. All
	// viper reads stay here in main; the client package takes the resolved values.
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
	defer func() {
		if err := conn.Close(); err != nil {
			fmt.Printf("ERROR closing connection: %s\n", err.Error())
		}
	}()

	client := hippo.NewHippocampusClient(conn)

	// A cycle is one pass of the showcase shape: optionally purge for a clean slate, load the book
	// (paced and/or live per the flags), then optionally summarise ripe events. With --loop it runs
	// every --period until interrupted; otherwise it runs exactly once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	loadOpts := loadOptions{
		live:       viper.GetBool("live"),
		paceWindow: viper.GetDuration("pace-window"),
	}

	reset := viper.GetBool("reset")
	summarise := viper.GetBool("summarize")

	cycle := func(ctx context.Context) error {
		if reset {
			if _, err := client.Purge(ctx, &hippo.EmptyRequest{}); err != nil {

				return fmt.Errorf("purging store: %w", err)
			}

			fmt.Println("purged store")
		}

		if err := execute(ctx, client, loadOpts); err != nil {

			return err
		}

		if summarise {
			summarize(client)
		}

		return nil
	}

	period := time.Duration(0)

	if viper.GetBool("loop") {
		period = viper.GetDuration("period")
	}

	pace.Loop(ctx, period, cycle, func(err error) {
		fmt.Printf("ERROR: cycle failed: %s\n", err.Error())
	})
}

var ChapterRegex = regexp.MustCompile(`^Chapter ([IVXLCDM]+)[.]$`)

// bookSpan is the wall-clock window the whole book is laid across, ending shortly before now. The
// service rejects a memory timestamp more than a few minutes in the future, so rather than starting
// in the past and stepping forward by an unbounded amount (which eventually overshoots now and every
// later write is rejected), the timeline is sized up front: count the paragraphs and chapters,
// divide the span between them, and advance by at most that increment each time.
const bookSpan = 2 * 365 * 24 * time.Hour

// loadOptions controls how execute lays the book down. The zero value reproduces the original
// behaviour: the whole timeline back-dated across bookSpan and streamed in a burst. live stamps each
// write at the current time instead, so the memories age in real wall-clock (what a live showcase
// wants); paceWindow spreads the writes across that window rather than bursting them.
type loadOptions struct {
	live       bool
	paceWindow time.Duration
}

// execute streams the book: an event per chapter, a memory per paragraph. It returns ctx.Err() if
// the context is cancelled mid-load (so a caller stops the run and skips summarising), and nil once
// the whole book is loaded.
func execute(ctx context.Context, client hippo.HippocampusClient, opts loadOptions) error {
	paragraphs, chapters := countSteps(data)

	// A chapter advances the clock ten times as far as a paragraph (a deliberate gap between
	// chapters), so weight chapters accordingly when dividing the span.
	units := int64(paragraphs) + int64(chapters)*10
	if units < 1 {
		units = 1
	}

	increment := bookSpan.Nanoseconds() / units

	// Pace across the window using one Wait per write (every paragraph and chapter is one write).
	pacer := pace.NewPacer(opts.paceWindow, paragraphs+chapters)

	memory := ""
	eventId := ""
	ts := time.Now().Add(-bookSpan - time.Hour).UnixNano()

	// nextTs advances the timeline for the next write: to now in live mode, or forward by the
	// pre-sized increment in back-dated mode.
	nextTs := func(inc int64) int64 {
		if opts.live {

			return time.Now().UnixNano()
		}

		return ts + step(inc)
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		// Strip a leading UTF-8 BOM (present on the first line) so "Chapter I." is recognised as a
		// chapter heading rather than mistaken for paragraph text.
		line := strings.TrimPrefix(scanner.Text(), "\ufeff")

		// Check for completed paragraph (memory) & store
		if line == "" {
			if memory != "" {
				ts = nextTs(increment)

				m := &hippo.Memory{
					EventId:      eventId,
					Significance: randomSignificance(),
					TimeStamp:    ts,
					Body:         memory,
				}

				_, err := client.StoreMemory(ctx, m)
				if err != nil {
					fmt.Printf("ERROR storing memory: %s\n", err.Error())
				}

				memory = ""

				if err := pacer.Wait(ctx); err != nil {

					return err
				}
			}
			continue
		}

		// Check for new chapter (event) & store
		if strings.HasPrefix(line, "Chapter ") {
			s := ChapterRegex.FindStringSubmatch(line)
			if s != nil {
				// Set the end time of the previous event. Skipped for the very first chapter, which
				// has no preceding event to close.
				if eventId != "" {
					ee := &hippo.EndEventRequest{
						Id:      eventId,
						TimeEnd: ts,
					}

					if _, err := client.EndEvent(ctx, ee); err != nil {
						fmt.Printf("ERROR ending event: %s\n", err.Error())
					}
				}

				// Create the start of the new event
				ts = nextTs(increment * 10)

				e := &hippo.Event{
					TimeStart:    ts,
					Significance: randomSignificance(),
					Name:         line,
					Description:  line,
				}

				r, err := client.StoreEvent(ctx, e)
				if err != nil {
					fmt.Printf("ERROR storing event: %s\n", err.Error())
					continue
				}

				eventId = r.GetId()

				if memory != "" {
					fmt.Printf("MEMORY NOT EMPTY!!! '%s'\n", line)
					memory = ""
				}

				if err := pacer.Wait(ctx); err != nil {

					return err
				}

				continue
			}
		}

		memory += " " + line

	}

	return nil
}

// countSteps counts the paragraphs (memories) and chapters (events) in the book using the same
// blank-line / "Chapter …" rules execute does, so the timeline can be sized before it is streamed.
func countSteps(data []byte) (int, int) {
	paragraphs := 0
	chapters := 0
	memory := ""

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimPrefix(scanner.Text(), "\ufeff")

		if line == "" {
			if memory != "" {
				paragraphs++
				memory = ""
			}

			continue
		}

		if strings.HasPrefix(line, "Chapter ") && ChapterRegex.MatchString(line) {
			chapters++
			memory = ""

			continue
		}

		memory += " " + line
	}

	return paragraphs, chapters
}

// step returns a forward time increment in [base/2, base] nanoseconds, so timestamps stay strictly
// increasing and lightly jittered while never advancing more than base per call — which keeps the
// accumulated timeline inside the pre-sized span and out of the future.
func step(base int64) int64 {
	if base < 2 {
		return 1
	}

	return base/2 + rand.Int63n(base/2) + 1
}

func randomSignificance() int32 {
	return int32(rand.Intn(32767)) + 1
}
