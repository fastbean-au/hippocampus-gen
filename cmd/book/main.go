package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	hippo "github.com/fastbean-au/hippocampus/contract"
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
	pflag.Parse()

	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		fmt.Printf("ERROR: %s\n", err.Error())
		os.Exit(1)
	}

	// Create the gRPC client
	var opts = []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
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

	execute(client)

	if viper.GetBool("summarize") {
		summarize(client)
	}
}

var ChapterRegex = regexp.MustCompile(`^Chapter ([IVXLCDM]+)[.]$`)

// bookSpan is the wall-clock window the whole book is laid across, ending shortly before now. The
// service rejects a memory timestamp more than a few minutes in the future, so rather than starting
// in the past and stepping forward by an unbounded amount (which eventually overshoots now and every
// later write is rejected), the timeline is sized up front: count the paragraphs and chapters,
// divide the span between them, and advance by at most that increment each time.
const bookSpan = 2 * 365 * 24 * time.Hour

func execute(client hippo.HippocampusClient) {
	ctx := context.Background()

	paragraphs, chapters := countSteps(data)

	// A chapter advances the clock ten times as far as a paragraph (a deliberate gap between
	// chapters), so weight chapters accordingly when dividing the span.
	units := int64(paragraphs) + int64(chapters)*10
	if units < 1 {
		units = 1
	}

	increment := bookSpan.Nanoseconds() / units

	memory := ""
	eventId := ""
	ts := time.Now().Add(-bookSpan - time.Hour).UnixNano()

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		// Strip a leading UTF-8 BOM (present on the first line) so "Chapter I." is recognised as a
		// chapter heading rather than mistaken for paragraph text.
		line := strings.TrimPrefix(scanner.Text(), "\ufeff")

		// Check for completed paragraph (memory) & store
		if line == "" {
			if memory != "" {
				ts += step(increment)

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
				ts += step(increment * 10)

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

				continue
			}
		}

		memory += " " + line

	}
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
