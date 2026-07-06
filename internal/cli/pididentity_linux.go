//go:build linux

package cli

import (
	"fmt"
	"os"
	"strings"
)

// processStartToken returns an opaque token that identifies the specific
// running instance behind pid — field 22 (starttime, in clock ticks since
// boot) of /proc/<pid>/stat. Combined with the pid it distinguishes a live
// daemon from an unrelated process the OS assigned the same (recycled) pid
// after a crash without cleanup (issue #418). Returns ok=false when /proc
// is unavailable or the field can't be parsed, in which case callers fall
// back to a bare liveness check (pre-#418 behaviour, no pid-reuse
// protection).
func processStartToken(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false
	}
	// comm (field 2) is parenthesised and may itself contain spaces and
	// parentheses, so the pid+comm prefix runs up to the LAST ')'. The
	// remaining space-separated fields begin at field 3 (state).
	s := string(raw)
	lastParen := strings.LastIndexByte(s, ')')
	if lastParen < 0 || lastParen+2 >= len(s) {
		return "", false
	}
	fields := strings.Fields(s[lastParen+2:])
	// starttime is field 22; fields[0] here is field 3, so the offset is 19.
	const starttimeIdx = 19
	if len(fields) <= starttimeIdx {
		return "", false
	}
	return fields[starttimeIdx], true
}
