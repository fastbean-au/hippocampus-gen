package generator

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	hippo "github.com/fastbean-au/hippocampus/proto"
)

type Generator struct {
	dict          []string
	dictByLen     map[int][]string
	maxWordLength int
	memoryLength  int
	client        hippo.HippocampusClient
}

func New(dict []string, dictByLen map[int][]string, maxWordLength int, memoryLength int, client hippo.HippocampusClient) *Generator {
	return &Generator{
		dict:          dict,
		dictByLen:     dictByLen,
		maxWordLength: maxWordLength,
		memoryLength:  memoryLength,
		client:        client,
	}
}

func (g *Generator) Execute(eventCount int, eventMemoryCount int, memoryCount int, wg *sync.WaitGroup) {
	ctx := context.Background()
	defer wg.Done()

	fmt.Printf("Starting worker: events (memories): %d (%d), memories %d\n", eventCount, eventMemoryCount, memoryCount)

	// Create events
	i := 0
	for {
		if i == eventCount {
			break
		}

		e := g.buildEvent(eventMemoryCount)

		_, err := g.client.StoreEvent(ctx, e)
		if err != nil {
			fmt.Printf("ERROR storing event: %s\n", err.Error())
		} else {
			i++
		}
	}

	// Create memories without events
	i = 0
	for {
		if i == memoryCount {
			break
		}

		m := g.buildMemory(randomPastTimeNano())
		_, err := g.client.StoreMemory(ctx, m)
		if err != nil {
			fmt.Printf("ERROR storing memory: %s\n", err.Error())
		} else {
			i++
		}
	}
}

func (g *Generator) buildEvent(memCount int) *hippo.Event {
	// Basic event
	e := &hippo.Event{
		Significance: int32(rand.Intn(32768)),
		TimeStart:    randomPastTimeNano(),
		Name:         g.buildPhrase(32 + rand.Intn(224)),
		Description:  g.buildPhrase(64 + rand.Intn(960)),
	}

	// Build memories
	m := make([]*hippo.Memory, memCount)
	lastTimestamp := e.TimeStart

	for i := 0; i < memCount; i++ {
		m[i] = g.buildMemory(lastTimestamp)
	}

	e.Memories = m
	e.TimeEnd = m[memCount-1].TimeStamp

	return e
}

func (g *Generator) buildMemory(lastTimeStamp int64) *hippo.Memory {
	m := &hippo.Memory{
		Significance: int32(rand.Intn(32768)),
		TimeStamp:    lastTimeStamp + rand.Int63n(3600*1000000000),
		Body:         g.buildPhrase(g.memoryLength),
	}

	return m
}

func (g *Generator) buildPhrase(length int) string {
	wc := len(g.dict)
	str := ""

	for {
		r := length - len(str)
		switch {
		case r == 0:
			return str
		case r <= g.maxWordLength:
			i := rand.Intn(len(g.dictByLen[r]))
			str += g.dictByLen[r][i]
		default:
			i := rand.Intn(wc)
			str += g.dict[i] + " "
		}
	}
}

func randomPastTimeNano() int64 {
	return time.Now().AddDate(-1*(rand.Intn(5)+1), rand.Intn(12), rand.Intn(31)).UnixNano()
}
