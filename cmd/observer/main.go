// Command observer runs a small LLM-backed agent whose only memory is Hippocampus.
//
// Each cycle it reads what is new in a SOURCE store, recalls what it already concluded from its OWN
// store, asks a local model for one observation and how much that observation matters, and stores
// the result at a significance derived from that judgement. Then it forgets, like everything else in
// the store, unless what it wrote turns out to be worth keeping.
//
// It is the write side of what the retention benchmark models synthetically. The benchmark assumes a
// deployment that can say something useful about a memory when it writes it; this is an agent
// actually doing that, and being wrong about it sometimes.
//
// The model is deliberately a local one (Ollama). An agent whose notes cost money per cycle is not
// something to leave running on a demo site, and the judgement being asked for - five bands of
// importance - is within reach of a small model in a way that good prose is not.
//
//	go run ./cmd/observer \
//	  --source-address localhost:50053 --server_address localhost:50056 \
//	  --ollama-url http://localhost:11434 --model qwen2.5:3b
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"

	hippo "github.com/fastbean-au/hippocampus/contract"

	"github.com/fastbean-au/hippocampus-gen/internal/client"
	"github.com/fastbean-au/hippocampus-gen/internal/observer"
	"github.com/fastbean-au/hippocampus-gen/internal/oidc"
	"github.com/fastbean-au/hippocampus-gen/internal/pace"
)

func main() {
	pflag.StringP("server_address", "s", "localhost:50051", "the store the agent writes its own observations to")
	pflag.String("source-address", "", "the store the agent reads new material from (defaults to --server_address)")
	pflag.String("source-group", "", "restrict the material read to this group label")

	pflag.String("ollama-url", "http://localhost:11434", "base URL of the Ollama server")
	pflag.String("model", "qwen2.5:3b", "model to reason with")
	pflag.Float64("temperature", 0.3, "sampling temperature")
	pflag.Duration("model-timeout", 90*time.Second, "how long to wait for one generation")

	pflag.Duration("interval", 2*time.Minute, "how often to observe")
	pflag.Int("material", 8, "how many new source memories to consider per cycle")
	pflag.Int("recall", 4, "how many of its own prior observations to recall per cycle")
	pflag.String("group", "observer", "group label for the agent's own memories")

	pflag.Int32("min-significance", 1000, "significance for the least important band")
	pflag.Int32("max-significance", 81000, "significance for the most important band")

	pflag.Bool("once", false, "run a single cycle and exit, for a smoke test")

	client.RegisterAuthFlags(pflag.CommandLine)
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

func run(ctx context.Context) error {
	auth := oidc.AuthConfig{
		Token: viper.GetString("token"),
		ClientCredentialsConfig: oidc.ClientCredentialsConfig{
			Issuer:       viper.GetString("oidc-issuer"),
			TokenURL:     viper.GetString("oidc-token-url"),
			ClientID:     viper.GetString("oidc-client-id"),
			ClientSecret: viper.GetString("oidc-client-secret"),
			Scope:        viper.GetString("oidc-scope"),
			Audience:     viper.GetString("oidc-audience"),
		},
	}

	opts, err := client.DialOptions(ctx, auth)
	if err != nil {

		return err
	}

	own, err := grpc.NewClient(viper.GetString("server_address"), opts...)
	if err != nil {

		return fmt.Errorf("dialling the agent's own store: %w", err)
	}

	defer func() { _ = own.Close() }()

	source := own

	if address := viper.GetString("source-address"); address != "" && address != viper.GetString("server_address") {
		if source, err = grpc.NewClient(address, opts...); err != nil {

			return fmt.Errorf("dialling the source store: %w", err)
		}

		defer func() { _ = source.Close() }()
	}

	agent := &agent{
		own:    hippo.NewHippocampusClient(own),
		source: hippo.NewHippocampusClient(source),
		seen:   map[string]bool{},
	}

	if viper.GetBool("once") {

		return agent.cycle(ctx)
	}

	// A demonstration agent outlives its own failures: a model that times out, or a store that is
	// briefly unreachable, must not take the site's only source of new observations with it.
	pace.Loop(ctx, viper.GetDuration("interval"), agent.cycle, func(err error) {
		fmt.Printf("cycle failed, continuing: %s\n", err.Error())
	})

	return nil
}

// agent is one observing loop: two clients and a bounded memory of what it has already read.
type agent struct {
	own    hippo.HippocampusClient
	source hippo.HippocampusClient

	// seen bounds re-reading. The agent is otherwise stateless - it holds no cursor, because the
	// source store forgets and a cursor into a forgetting store is a bookmark into a book that is
	// losing pages.
	seen map[string]bool
}

// seenLimit bounds the re-read guard. Old ids are dropped wholesale rather than by age: the source
// store has almost certainly forgotten them by then, so re-reading one is harmless.
const seenLimit = 4096

func (a *agent) cycle(ctx context.Context) error {
	material, sources, err := a.read(ctx)
	if err != nil {

		return err
	}

	if len(material) == 0 {
		fmt.Println("nothing new to observe")

		return nil
	}

	recalled, err := a.recall(ctx, material)
	if err != nil {
		// Recall failing costs the agent its continuity, not its ability to observe.
		fmt.Printf("could not recall prior observations, continuing without them: %s\n", err.Error())
	}

	raw, err := a.generate(ctx, observer.BuildPrompt(material, recalled))
	if err != nil {

		return err
	}

	out, err := observer.ParseResponse(raw)
	if err != nil {

		return err
	}

	out.Sources = sources

	return a.store(ctx, out)
}

// read pulls the newest source memories the agent has not already considered.
func (a *agent) read(ctx context.Context) ([]string, []string, error) {
	limit := viper.GetInt("material")

	resp, err := a.source.GetMemories(ctx, &hippo.GetMemoriesRequest{
		// Over-fetch, because most of a page is usually already seen.
		Limit:   int32(limit * 4),
		OrderBy: "timestamp",
		Group:   viper.GetString("source-group"),
	})
	if err != nil {

		return nil, nil, fmt.Errorf("reading the source store: %w", err)
	}

	var material, sources []string

	for _, v := range resp.GetMemories() {
		if len(material) >= limit {

			break
		}

		if a.seen[v.GetId()] || strings.TrimSpace(v.GetBody()) == "" {
			continue
		}

		a.remember(v.GetId())

		material = append(material, collapse(v.GetBody()))
		sources = append(sources, v.GetId())
	}

	return material, sources, nil
}

// remember records an id as read, clearing the set wholesale when it grows past its bound.
func (a *agent) remember(id string) {
	if len(a.seen) >= seenLimit {
		a.seen = map[string]bool{}
	}

	a.seen[id] = true
}

// recall asks the agent's own store what it already concluded about material like this, so a cycle
// builds on the last one instead of restating it. This is the only reason the agent has a memory at
// all, and a reinforcing search is right here: what it keeps returning to should decay more slowly.
func (a *agent) recall(ctx context.Context, material []string) ([]string, error) {
	limit := viper.GetInt("recall")
	if limit <= 0 {

		return nil, nil
	}

	resp, err := a.own.SearchMemories(ctx, &hippo.SearchMemoriesRequest{
		Query:     strings.Join(material, " "),
		Limit:     int32(limit),
		Group:     viper.GetString("group"),
		Reinforce: true,
	})
	if err != nil {

		return nil, err
	}

	var out []string

	for _, v := range resp.GetMemories() {
		out = append(out, v.GetBody())
	}

	return out, nil
}

// store writes the observation at the significance the model's own rating implies.
func (a *agent) store(ctx context.Context, out observer.Observation) error {
	significance := observer.SignificanceFor(
		out.Rating,
		viper.GetInt32("min-significance"),
		viper.GetInt32("max-significance"),
	)

	metadata := map[string]string{
		"rating": fmt.Sprintf("%d", out.Rating),
		"model":  viper.GetString("model"),
	}

	// The material is in another store, so it cannot be linked to - recorded instead, so a note can
	// still be traced to what formed it. Bounded, because the contract caps a metadata VALUE at 512
	// bytes and eight ids comfortably exceed that: unbounded, the store refused the write and the
	// whole cycle - including the generation it had just paid for - was thrown away.
	if joined := boundedSources(out.Sources); joined != "" {
		metadata["sources"] = joined
	}

	resp, err := a.own.StoreMemory(ctx, &hippo.Memory{
		Body:         out.Note,
		Significance: significance,
		Group:        viper.GetString("group"),
		Metadata:     metadata,
	})
	if err != nil {

		return fmt.Errorf("storing the observation: %w", err)
	}

	if resp.GetRejected() {
		fmt.Printf("observation rejected as insignificant (rating %d)\n", out.Rating)

		return nil
	}

	fmt.Printf("rating %d (significance %d): %s\n", out.Rating, significance, out.Note)

	return nil
}

// maxMetadataValueBytes mirrors the contract's per-value metadata cap (types.MaxMetadataBytes).
const maxMetadataValueBytes = 512

// boundedSources joins as many source ids as fit the metadata cap, whole ids only. Recording some
// of what formed a note is worth having; losing the note because the list was too long is not.
func boundedSources(ids []string) string {
	var b strings.Builder

	for _, id := range ids {
		next := len(id)

		if b.Len() > 0 {
			next++
		}

		if b.Len()+next > maxMetadataValueBytes {

			break
		}

		if b.Len() > 0 {
			b.WriteString(" ")
		}

		b.WriteString(id)
	}

	return b.String()
}

// generate is a direct call to Ollama's completion endpoint. Hand-rolled for the same reason the
// service's own summariser is: it is one POST, and it keeps the LLM SDK dependency tree out of a
// repository that otherwise only needs a gRPC client.
func (a *agent) generate(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":  viper.GetString("model"),
		"prompt": prompt,
		"stream": false,
		"options": map[string]any{
			"temperature": viper.GetFloat64("temperature"),
		},
	})
	if err != nil {

		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, viper.GetDuration("model-timeout"))
	defer cancel()

	url := strings.TrimSuffix(viper.GetString("ollama-url"), "/") + "/api/generate"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {

		return "", err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {

		return "", fmt.Errorf("asking the model: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {

		return "", err
	}

	if resp.StatusCode != http.StatusOK {

		return "", fmt.Errorf("model returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	var out struct {
		Response string `json:"response"`
	}

	if err := json.Unmarshal(payload, &out); err != nil {

		return "", fmt.Errorf("parsing the model's reply: %w", err)
	}

	return out.Response, nil
}

// collapse flattens a body onto one line and bounds it, so a long post cannot crowd the prompt.
func collapse(body string) string {
	out := strings.Join(strings.Fields(body), " ")

	const limit = 300

	if len(out) > limit {
		out = out[:limit]
	}

	return out
}

func fail(err error) {
	fmt.Printf("ERROR: %s\n", err.Error())
	os.Exit(1)
}
