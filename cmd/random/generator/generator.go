package generator

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	hippo "github.com/fastbean-au/hippocampus/contract"

	"github.com/fastbean-au/hippocampus-gen/internal/link"
)

// recentSize is how many of a worker's own ids stay eligible as link targets. Big enough that the
// graph is not a chain of near-neighbours, small enough that the whole run's ids are not held.
const recentSize = 512

// Config is what a worker needs to generate: the wordlist in both the forms buildPhrase wants, the
// shape of what it writes, the links it declares, and the client to write through.
type Config struct {
	Dict          []string
	DictByLen     map[int][]string
	MaxWordLength int
	MemoryLength  int
	Links         int
	Client        hippo.HippocampusClient

	// Group is stamped on every record. Empty leaves it unset, which is both the historical
	// behaviour and what a group-scoped token wants: writing no group has the service fill in the
	// token's own. It only needs setting when a token carries several groups (the service cannot
	// choose between them) or when the data should be filed under a specific label.
	Group string
}

type Generator struct {
	config Config

	// Ids this worker has stored and can therefore link back to. Per worker rather than shared: the
	// workers run concurrently, and a link target must already exist when the write naming it lands.
	events   *link.Recent
	memories *link.Recent
}

func New(config Config) *Generator {
	return &Generator{
		config:   config,
		events:   link.NewRecent(recentSize),
		memories: link.NewRecent(recentSize),
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

		id, err := link.StoreEvent(ctx, g.config.Client, e)
		if err != nil {
			fmt.Printf("ERROR storing event: %s\n", err.Error())
		} else {
			g.events.Add(id)

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
		m.Links = g.links(g.memories)

		id, err := link.StoreMemory(ctx, g.config.Client, m)
		if err != nil {
			fmt.Printf("ERROR storing memory: %s\n", err.Error())
		} else {
			g.memories.Add(id)

			i++
		}
	}
}

// links draws up to --links targets at random from ids this worker has already stored. The
// associations mean nothing - this generator's data means nothing either - but they put a graph of
// roughly the right shape and density under a load test, which is what exercises the link tables and
// the damped link contribution in the decay maths.
//
// The memories nested inside an event get none: StoreEvent stores them as a batch and reports only
// how many were retained, so there is no id to link them by or to. Only the standalone memories,
// stored one at a time, take part.
func (g *Generator) links(recent *link.Recent) []*hippo.Link {
	if g.config.Links < 1 {

		return nil
	}

	ids := recent.Sample(rand.Intn(g.config.Links + 1))

	links := make([]*hippo.Link, 0, len(ids))

	for _, id := range ids {
		links = append(links, link.New(id, randomSignificance()))
	}

	return links
}

func (g *Generator) buildEvent(memCount int) *hippo.Event {
	// Basic event
	e := &hippo.Event{
		Significance: randomSignificance(),
		TimeStart:    randomPastTimeNano(),
		Name:         g.buildPhrase(32 + rand.Intn(224)),
		Description:  g.buildPhrase(64 + rand.Intn(960)),
		Links:        g.links(g.events),
		Group:        g.config.Group,
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
		Significance: randomSignificance(),
		TimeStamp:    lastTimeStamp + rand.Int63n(3600*1000000000),
		Body:         g.buildPhrase(g.config.MemoryLength),
		Group:        g.config.Group,
	}

	return m
}

func (g *Generator) buildPhrase(length int) string {
	wc := len(g.config.Dict)
	str := ""

	for {
		r := length - len(str)
		switch {
		case r == 0:
			return str
		case r <= g.config.MaxWordLength:
			i := rand.Intn(len(g.config.DictByLen[r]))
			str += g.config.DictByLen[r][i]
		default:
			i := rand.Intn(wc)
			str += g.config.Dict[i] + " "
		}
	}
}

func randomPastTimeNano() int64 {
	return time.Now().AddDate(-1*(rand.Intn(5)+1), rand.Intn(12), rand.Intn(31)).UnixNano()
}

func randomSignificance() int32 {
	return int32(rand.Intn(32767)) + 1
}
