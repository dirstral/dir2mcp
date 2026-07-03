package embedqueue

import (
	"context"
	"errors"
	"time"
)

// Default tuning for RunCoordinator.
const (
	defaultCoordinatorInterval = 2 * time.Second
	defaultStallThreshold      = 3
)

// CoordinatorLoopOptions configures RunCoordinator (SPEC §8.7.1; issue #435 C4).
type CoordinatorLoopOptions struct {
	// Interval is how often the loop re-enqueues the pending head. Non-positive
	// defaults to 2s.
	Interval time.Duration

	// StallThreshold is the number of CONSECUTIVE EnqueuePending failures after
	// which OnStall fires — the loop treats that many back-to-back failures as a
	// sustained stall (broker read-only / disk full), not a transient blip.
	// Non-positive defaults to 3.
	StallThreshold int

	// OnError, if set, is called on every enqueue failure (each tick). It is the
	// per-failure, best-effort notification hook. The loop invokes it SYNCHRONOUSLY
	// on its own goroutine, so the callback MUST be fast/non-blocking (or at most
	// context-bounded) — a callback that blocks indefinitely stalls the loop and
	// prevents further re-enqueues. Cancellation errors are not reported.
	OnError func(err error)

	// OnStall, if set, is called ONCE when the consecutive-failure count first
	// reaches StallThreshold, and not again until a later enqueue succeeds (which
	// resets the counter). It is the durable escalation signal for a sustained
	// stall — unlike OnError it is expected to guarantee delivery (e.g. a blocking,
	// context-aware channel send or a structured event), because otherwise
	// embedding makes no progress while the daemon still looks healthy (issue #435
	// C4). Like OnError it is invoked SYNCHRONOUSLY on the loop's goroutine, so it
	// too must be fast/non-blocking or at most context-bounded (a context-aware
	// blocking send is fine — it cannot outlive ctx). It receives the
	// consecutive-failure count and the latest error.
	OnStall func(consecutive int, err error)
}

func (o CoordinatorLoopOptions) interval() time.Duration {
	if o.Interval <= 0 {
		return defaultCoordinatorInterval
	}
	return o.Interval
}

func (o CoordinatorLoopOptions) stallThreshold() int {
	if o.StallThreshold <= 0 {
		return defaultStallThreshold
	}
	return o.StallThreshold
}

// RunCoordinator drives coord.EnqueuePending on a fixed interval until ctx is
// cancelled (SPEC §8.7.1). Re-enqueuing is safe: an already-embedded chunk is no
// longer pending, and a duplicate job is idempotent at the embed layer (§8.7.3).
//
// It owns the stall-detection the bare loop lacked (issue #435 C4): a transient
// enqueue error is reported via OnError and the loop keeps ticking (a momentary
// broker hiccup must not tear down embedding), but StallThreshold consecutive
// failures fire OnStall exactly once so a sustained failure surfaces a durable
// signal instead of a silently-idle daemon. A single success resets both the
// counter and the armed OnStall.
func RunCoordinator(ctx context.Context, coord *Coordinator, opts CoordinatorLoopOptions) {
	ticker := time.NewTicker(opts.interval())
	defer ticker.Stop()

	threshold := opts.stallThreshold()
	consecutive := 0
	stallSignaled := false

	for {
		_, err := coord.EnqueuePending(ctx, "")
		switch {
		case err == nil:
			// Progress: forget the failure streak and re-arm stall detection.
			consecutive = 0
			stallSignaled = false
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			// Shutdown, not a stall — return without signaling.
			return
		default:
			consecutive++
			if opts.OnError != nil {
				opts.OnError(err)
			}
			if consecutive >= threshold && !stallSignaled {
				stallSignaled = true
				if opts.OnStall != nil {
					opts.OnStall(consecutive, err)
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
