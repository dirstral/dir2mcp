//go:build !linux && !darwin

package cli

// processStartToken has no portable implementation on this platform, so it
// reports ok=false and callers fall back to a bare pid-liveness check. This
// keeps behaviour byte-for-byte identical to the pre-#418 code on platforms
// where we cannot read a process start time (no pid-reuse protection, but no
// regression either).
func processStartToken(_ int) (string, bool) {
	return "", false
}
