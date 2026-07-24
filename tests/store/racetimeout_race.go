//go:build race

package tests

import "time"

// raceScaled expands a fixed test deadline when the binary is built with the
// race detector. `-race` slows the CGO sqlite store paths roughly an order of
// magnitude, so deadlines that are generous in a normal build would otherwise
// expire under full-suite CI contention and produce spurious "context deadline
// exceeded" failures that have nothing to do with a data race (issue #614).
// Mirrors the tests/cli helper of the same name (issue #419).
func raceScaled(d time.Duration) time.Duration { return d * 10 }
