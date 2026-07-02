//go:build !race

package tests

import "time"

// raceScaled is the identity outside `-race` builds; see the race-tagged variant
// for why deadlines are expanded when the race detector is enabled.
func raceScaled(d time.Duration) time.Duration { return d }
