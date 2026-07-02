//go:build !race

package tests

import "time"

// raceScaled is the identity outside `-race` builds; see the race-tagged variant.
func raceScaled(d time.Duration) time.Duration { return d }
