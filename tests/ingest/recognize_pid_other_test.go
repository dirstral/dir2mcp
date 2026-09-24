//go:build !unix

package tests

// pidAlive is a stub on platforms without unix signals. The tests that call it
// skip on these platforms before they reach it.
func pidAlive(_ int) bool {
	return false
}
