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

	hippo "github.com/fastbean-au/hippocampus/proto"
)

var (
	//go:embed great_expectations.txt
	data []byte
)

func main() {
	pflag.StringP("server_address", "s", "localhost:8000", "address of hippocampus server")
	pflag.Parse()

	viper.BindPFlags(pflag.CommandLine)

	// Create the gRPC client
	var opts = []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	conn, err := grpc.Dial(viper.GetString("server_address"), opts...)
	if err != nil {
		fmt.Printf("ERROR: %s\n", err.Error())
		os.Exit(1)
	}
	defer conn.Close()

	client := hippo.NewHippocampusClient(conn)

	execute(client)
}

var ChapterRegex = regexp.MustCompile(`^Chapter ([IVXLCDM]+)[.]$`)

func execute(client hippo.HippocampusClient) {
	ctx := context.Background()

	memory := ""
	eventId := ""
	ts := randomStartTimeNano()

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()

		// Check for completed paragraph (memory) & store
		if line == "" {
			if memory != "" {
				ts += randomTimeIncrement()

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
				// Set memory end time of previous event
				ee := &hippo.EndEventRequest{
					Id:      eventId,
					TimeEnd: ts,
				}
				_, err := client.EndEvent(ctx, ee)
				if err != nil {
					fmt.Printf("ERROR ending event: %s\n", err.Error())
				}

				// Create the start of the new event
				ts += randomTimeIncrement() * 10

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

func randomStartTimeNano() int64 {
	return time.Now().AddDate(-1*(rand.Intn(5)+1), rand.Intn(12), rand.Intn(31)).UnixNano()
}

func randomTimeIncrement() int64 {
	return int64(rand.Intn(86400*1000000000)) + 1
}

func randomSignificance() int32 {
	return int32(rand.Intn(32767)) + 1
}
