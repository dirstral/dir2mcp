//go:build race

package tests

import "time"

// raceScaled expands a fixed test deadline when the binary is built with the
// race detector. `-race` slows the CGO sqlite store-init/query paths roughly an
// order of magnitude, so deadlines that are generous in a normal build would
// otherwise expire under load and produce spurious "context deadline exceeded"
// failures that have nothing to do with a data race (issue #419).
func raceScaled(d time.Duration) time.Duration { return d * 10 }
