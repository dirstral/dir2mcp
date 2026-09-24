//go:build !race

package tests

import (
	"runtime"
	"time"
)

// raceScaled is the identity outside `-race` builds; see the race-tagged variant
// for why deadlines are expanded when the race detector is enabled. On Windows
// it expands the deadline four times: a Windows runner flushes files much more
// slowly, and the sqlite store init alone can take over two seconds there.
func raceScaled(d time.Duration) time.Duration {
	if runtime.GOOS == "windows" {
		return d * 4
	}
	return d
}
