//go:build race

package tests

import "time"

// raceScaled expands a fixed test deadline when the binary is built with the
// race detector. `-race` slows the CGO sqlite paths (and WAL lock contention)
// roughly an order of magnitude, so a deadline that is ample normally would
// otherwise expire and produce a spurious "context deadline exceeded" failure
// unrelated to any data race (issue #419).
func raceScaled(d time.Duration) time.Duration { return d * 10 }
