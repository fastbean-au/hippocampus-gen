// Command agentfit measures the working-memory dynamics of a real agent from its session
// transcripts and writes them out as the parameter file the agent trace generator samples from.
//
// It exists so the retention benchmark is not circular. A benchmark whose author writes both the
// trace and the ground truth proves nothing, so the reuse distribution the synthetic trace
// reproduces is fitted to a real corpus instead of being chosen. The corpus is private working data
// and is not published; the fitted parameters are, and they are what a sceptical reader audits.
//
// Typical use, against a Claude Code project's transcripts:
//
//	go run ./cmd/agentfit \
//	  --transcripts ~/.claude/projects/-Users-me-src-myproject \
//	  --entity-prefix /myproject \
//	  --describe "myproject, Claude Code sessions" \
//	  --out data/params.json
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus-gen/internal/fit"
)

func main() {
	pflag.StringP("transcripts", "t", "", "directory of .jsonl agent session transcripts (required)")
	pflag.String("entity-prefix", "", "keep only referenced paths containing this substring (empty keeps all)")
	pflag.String("describe", "", "human description of the corpus, recorded in the parameter file")
	pflag.StringP("out", "o", "", "write the parameter file here (empty writes to stdout)")
	pflag.Parse()

	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		fail(err)
	}

	dir := viper.GetString("transcripts")
	if dir == "" {
		fail(fmt.Errorf("--transcripts is required"))
	}

	obs, err := fit.Scan(dir, viper.GetString("entity-prefix"))
	if err != nil {
		fail(err)
	}

	describe := viper.GetString("describe")
	if describe == "" {
		describe = dir
	}

	params := obs.Fit(describe, time.Now())

	data, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		fail(err)
	}

	data = append(data, '\n')

	if out := viper.GetString("out"); out != "" {
		if err := os.WriteFile(out, data, 0o644); err != nil {
			fail(err)
		}

		summarise(params, out)

		return
	}

	fmt.Print(string(data))
}

// summarise prints the handful of figures that decide whether a fit is worth keeping: the corpus
// size, how concentrated references are, and - the reason the benchmark exists - how much of the
// reuse lands beyond a recency window.
func summarise(params fit.Params, out string) {
	fmt.Printf("wrote %s\n", out)
	fmt.Printf("  corpus:     %d sessions, %d references, %d distinct entities, %.0f days\n",
		params.Corpus.Sessions, params.Corpus.References, params.Corpus.DistinctEntities, params.Corpus.SpanDays)
	fmt.Printf("  popularity: zipf alpha %.2f, %.0f%% referenced once, %.0f%% of entities carry %.0f%% of references\n",
		params.Popularity.ZipfAlpha, 100*params.Popularity.OnceOnlyFraction,
		100*params.Popularity.HeadFraction, 100*params.Popularity.HeadShare)
	fmt.Printf("  reuse:      %.0f%% same-session (median %.3fh), %.0f%% later (median %.1fh)\n",
		100*params.Reuse.BurstShare, params.Reuse.Burst.MedianHours,
		100*(1-params.Reuse.BurstShare), params.Reuse.Tail.MedianHours)
	fmt.Printf("  a recency window would miss %.1f%% of reuses at 24h, %.1f%% at 7d\n",
		100*params.Reuse.BeyondOneDay, 100*params.Reuse.BeyondOneWeek)
}

func fail(err error) {
	fmt.Printf("ERROR: %s\n", err.Error())
	os.Exit(1)
}
