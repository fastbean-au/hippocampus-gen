// Package pace provides the timing primitives the showcase generators use: a Loop that repeats a
// unit of work on a fixed period (aligned to the start, skipping boundaries a long run overran), and
// a Pacer that spreads a run's writes across a window so data is created over time rather than in a
// burst. Both honour context cancellation so a SIGINT stops them promptly.
package pace

import (
	"context"
	"time"
)

// Loop invokes fn immediately, then again at each subsequent period boundary measured from the
// start, until ctx is done. A run that overruns one or more boundaries skips the missed ones rather
// than firing them back-to-back, so a slow cycle never causes a pile-up. fn receives ctx so a long
// run is cancelled at shutdown; its error is reported via onError (nil to ignore) and never stops
// the loop. A non-positive period runs fn exactly once (single-shot).
func Loop(ctx context.Context, period time.Duration, fn func(context.Context) error, onError func(error)) {
	start := time.Now()

	run := func() {
		if err := fn(ctx); err != nil && onError != nil {
			onError(err)
		}
	}

	run()

	if period <= 0 {

		return
	}

	for {
		// The next boundary strictly after now, so an overrun jumps to a future one.
		next := start.Add((time.Since(start)/period + 1) * period)

		timer := time.NewTimer(time.Until(next))

		select {

		case <-ctx.Done():
			timer.Stop()

			return

		case <-timer.C:
			run()
		}
	}
}

// Pacer spreads a run's writes across a window by sleeping a fixed delay between them. Its zero value
// (and any pacer built from a non-positive window or step count) does not pace at all, so Wait
// returns immediately - the burst behaviour.
type Pacer struct {
	delay time.Duration
}

// NewPacer divides window evenly across steps, so a caller that calls Wait once per write spreads
// them across roughly window. A non-positive window or step count yields a no-op pacer.
func NewPacer(window time.Duration, steps int) Pacer {
	if window <= 0 || steps <= 0 {

		return Pacer{}
	}

	return Pacer{delay: window / time.Duration(steps)}
}

// Delay is the per-step sleep the pacer applies, exposed for logging/inspection.
func (p Pacer) Delay() time.Duration {
	return p.delay
}

// Wait sleeps the per-step delay, returning early with ctx.Err() if ctx is cancelled first. A no-op
// pacer returns immediately.
func (p Pacer) Wait(ctx context.Context) error {
	if p.delay <= 0 {

		return nil
	}

	timer := time.NewTimer(p.delay)
	defer timer.Stop()

	select {

	case <-ctx.Done():

		return ctx.Err()

	case <-timer.C:

		return nil
	}
}
