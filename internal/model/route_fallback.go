package model

// RouteFallbackRecord is one live transcript that holds a SPEC §8.2.4
// fallback result (spec 0.82.0): route_fallback_from under item scope, or a
// coverage.languages entry with fallback_from under window scope. At and By
// are its route_fallback_at and route_fallback_by, empty when the transcript
// records none. `dir2mcp doctor` counts these records and names the next due
// time.
type RouteFallbackRecord struct {
	RelPath string
	RepType string
	At      string
	By      string
}
