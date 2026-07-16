package main

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/fastbean-au/hippocampus-gen/cmd/random/generator"
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
	pflag.StringP("server_address", "s", "localhost:50051", "address of hippocampus server")
	pflag.Parse()

	viper.BindPFlags(pflag.CommandLine)

	dictByLen = make(map[int][]string)

	err := importDictionary()
	if err != nil {
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
	defer conn.Close()

	client := hippo.NewHippocampusClient(conn)

	// Setup the workers
	workerCount := viper.GetInt("workers")

	eventCount := viper.GetInt("events")
	memoryCount := viper.GetInt("memories")
	memoriesWithoutEventsCount := viper.GetInt("memories_without_events")
	memoryLength := viper.GetInt("memory_length")

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

			g := generator.New(dict, dictByLen, MaxWordLength, memoryLength, client)
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
