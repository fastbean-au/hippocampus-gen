package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus-gen/cmd/random/generator"
	"github.com/fastbean-au/hippocampus-gen/internal/client"
	"github.com/fastbean-au/hippocampus-gen/internal/oidc"
	hippo "github.com/fastbean-au/hippocampus/contract"
)

const MaxWordLength = 16

var dict []string
var dictByLen map[int][]string

var (
	//go:embed wordlist.10000.txt
	data []byte
)

func main() {
	pflag.IntP("events", "e", 100, "number of events to create")
	pflag.IntP("memories", "m", 10000, "number of memories to create")
	pflag.IntP("memory_length", "l", 256, "length of memories")
	pflag.IntP("memories_without_events", "p", 50, "percentage of memories without events")
	pflag.IntP("workers", "w", 5, "number of workers")
	pflag.IntP("links", "L", 0, "link each event and each standalone memory to up to this many earlier ones (0 = no links)")
	pflag.StringP("server_address", "s", "localhost:50051", "address of hippocampus server")
	pflag.String("group", "", "group label stamped on every record; set this to the label a group-scoped token carries (empty leaves it unset, which lets the service stamp the token's own group)")
	client.RegisterAuthFlags(pflag.CommandLine)
	pflag.Parse()

	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		fmt.Printf("ERROR: %s\n", err.Error())
		os.Exit(1)
	}

	dictByLen = make(map[int][]string)

	err := importDictionary()
	if err != nil {
		os.Exit(1)
	}

	// Create the gRPC client
	// Build the dial options, adding the bearer-token interceptor when auth is configured. All viper
	// reads stay here in main; the client package takes the resolved values. Mirrors logs and book -
	// this generator previously dialled plain gRPC only, and so could not be pointed at any
	// authenticated instance.
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

	// Setup the workers
	workerCount := viper.GetInt("workers")

	eventCount := viper.GetInt("events")
	memoryCount := viper.GetInt("memories")
	memoriesWithoutEventsCount := viper.GetInt("memories_without_events")
	memoryLength := viper.GetInt("memory_length")
	links := viper.GetInt("links")

	eventMemories := int(float64(memoryCount * memoriesWithoutEventsCount / 100))
	memoriesWithoutEvents := memoryCount - eventMemories

	// Calculate base worker's set
	epw := eventCount / workerCount
	epwR := eventCount % workerCount
	mpe := eventMemories / workerCount
	mpeR := eventMemories % workerCount
	mpw := memoriesWithoutEvents / workerCount
	mpwR := memoriesWithoutEvents % workerCount

	// Start the workers
	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)

		go func(i, epw, mpe, mpw int) {
			// Override work allocation
			if i < epwR {
				epw++
			}
			if i < mpeR {
				mpe++
			}
			if i < mpwR {
				mpw++
			}

			g := generator.New(generator.Config{
				Dict:          dict,
				DictByLen:     dictByLen,
				MaxWordLength: MaxWordLength,
				MemoryLength:  memoryLength,
				Links:         links,
				Client:        client,
				Group:         viper.GetString("group"),
			})

			g.Execute(epw, mpe, mpw, &wg)
		}(i, epw, mpe, mpw)
	}

	wg.Wait()
}

func importDictionary() error {
	for _, word := range strings.Split(string(data), "\n") {
		l := len(word)
		if l > MaxWordLength {
			continue
		}

		dict = append(dict, word)
		dictByLen[l] = append(dictByLen[l], word)

	}

	return nil
}
