package pace

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLoopSingleShotWhenPeriodNonPositive(t *testing.T) {
	calls := 0

	Loop(context.Background(), 0, func(context.Context) error {
		calls++

		return nil
	}, nil)

	if calls != 1 {
		t.Fatalf("expected a single run for a non-positive period, got %d", calls)
	}
}

func TestLoopRepeatsUntilCancelled(t *testing.T) {
	// The fn cancels the loop on its third call, so the count is deterministic regardless of timing.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0

	Loop(ctx, time.Millisecond, func(context.Context) error {
		calls++

		if calls == 3 {
			cancel()
		}

		return nil
	}, nil)

	if calls != 3 {
		t.Fatalf("expected exactly 3 runs before cancellation, got %d", calls)
	}
}

func TestLoopReportsErrorsWithoutStopping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var reported int

	calls := 0

	Loop(ctx, time.Millisecond, func(context.Context) error {
		calls++

		if calls == 2 {
			cancel()
		}

		return errors.New("boom")
	}, func(error) {
		reported++
	})

	if reported != calls {
		t.Fatalf("expected every run's error reported (%d), got %d", calls, reported)
	}
}

func TestLoopStopsPromptlyOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0

	// Even with a cancelled context the immediate run happens once; then the loop exits at the
	// select rather than waiting out the (long) period.
	done := make(chan struct{})

	go func() {
		Loop(ctx, time.Hour, func(context.Context) error {
			calls++

			return nil
		}, nil)

		close(done)
	}()

	select {

	case <-done:

	case <-time.After(time.Second):
		t.Fatal("Loop did not return promptly on a cancelled context")
	}

	if calls != 1 {
		t.Fatalf("expected the single immediate run, got %d", calls)
	}
}

func TestNewPacerDelay(t *testing.T) {
	if got := NewPacer(100*time.Millisecond, 10).Delay(); got != 10*time.Millisecond {
		t.Fatalf("expected a 10ms per-step delay, got %s", got)
	}

	if got := NewPacer(0, 10).Delay(); got != 0 {
		t.Fatalf("expected a no-op pacer for a zero window, got %s", got)
	}

	if got := NewPacer(time.Second, 0).Delay(); got != 0 {
		t.Fatalf("expected a no-op pacer for zero steps, got %s", got)
	}
}

func TestPacerWaitSleeps(t *testing.T) {
	start := time.Now()

	if err := NewPacer(50*time.Millisecond, 5).Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %s", err)
	}

	// 50ms / 5 = 10ms; allow generous slack for a loaded CI machine.
	if elapsed := time.Since(start); elapsed < 8*time.Millisecond {
		t.Fatalf("expected Wait to sleep ~10ms, returned after %s", elapsed)
	}
}

func TestPacerWaitNoOpReturnsImmediately(t *testing.T) {
	if err := (Pacer{}).Wait(context.Background()); err != nil {
		t.Fatalf("expected a no-op pacer to return nil, got %s", err)
	}
}

func TestPacerWaitCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := NewPacer(time.Hour, 1).Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
