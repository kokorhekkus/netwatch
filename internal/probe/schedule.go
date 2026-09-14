package probe

import (
	"context"
	"math"
	"math/rand"
	"time"
)

// Poisson sampling parameters.
const (
	DefaultMeanInterval = 3 * time.Second
	MinInterval         = 500 * time.Millisecond
	MaxInterval         = 15 * time.Second
)

// Schedule calls fn at Poisson-distributed intervals until ctx is cancelled.
//
// A fixed-period ticker is the obvious choice and the wrong one. Periodic
// sampling aliases with periodic network behaviour - Wi-Fi off-channel scans,
// DFS radar checks, a neighbour's cron job - so a fixed 3s tick can either
// systematically miss a recurring problem or systematically land on it, and
// the data cannot tell you which. RFC 2330 prescribes exponentially
// distributed inter-arrivals for exactly this reason.
//
// It also happens to sidestep macOS timer coalescing, which makes a fixed
// ticker drift anyway.
//
// startDelay staggers targets against each other. Without it every target
// fires on the same instant, and on a constrained uplink the probes queue
// behind one another so the last one measures our own serialisation delay
// rather than the network's.
func Schedule(ctx context.Context, mean, startDelay time.Duration, rng *rand.Rand, fn func(context.Context)) {
	select {
	case <-time.After(startDelay):
	case <-ctx.Done():
		return
	}

	for {
		fn(ctx)

		select {
		case <-time.After(nextInterval(mean, rng)):
		case <-ctx.Done():
			return
		}
	}
}

// nextInterval draws an exponentially distributed delay, clamped so that a
// tail draw cannot stall a probe for minutes or hammer the link.
func nextInterval(mean time.Duration, rng *rand.Rand) time.Duration {
	u := rng.Float64()
	if u <= 0 {
		u = math.SmallestNonzeroFloat64
	}
	d := time.Duration(-math.Log(u) * float64(mean))
	if d < MinInterval {
		return MinInterval
	}
	if d > MaxInterval {
		return MaxInterval
	}
	return d
}
