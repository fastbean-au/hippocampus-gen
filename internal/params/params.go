// Package params carries the fitted workload parameters the agent trace generator samples from, as
// the single copy of them in this repository.
//
// They live behind a package rather than in a data directory because the generator ships as a
// distroless container image, which has no working directory to read a file from - and because a
// second copy checked in beside the first is exactly the kind of thing that drifts. cmd/agentfit
// writes this file, cmd/agent embeds it, and the tests read it through Default(); there is nowhere
// else for a stale copy to hide.
package params

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/fastbean-au/hippocampus-gen/internal/fit"
)

//go:embed params.json
var fitted []byte

// Default returns the committed parameters, fitted from a real agent's session transcripts by
// cmd/agentfit. See the repository README for what was measured and what it means.
func Default() (fit.Params, error) {
	var out fit.Params

	if err := json.Unmarshal(fitted, &out); err != nil {

		return out, fmt.Errorf("parsing the embedded parameters: %w", err)
	}

	return out, nil
}
