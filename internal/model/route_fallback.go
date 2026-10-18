package model

// RouteFallbackRecord is one live transcript that holds a SPEC §8.2.4
// fallback result (spec 0.82.0): route_fallback_from under item scope, or a
// coverage.languages entry with fallback_from under window scope. At and By
// are its route_fallback_at and route_fallback_by, empty when the transcript
// records none. Dropped is set when the transcript records a window dropped
// under on_route_error: fail (route_fallback_dropped): its cached fallback
// text is kept, so the transcript is processed on every run. The ingest gate
// and `dir2mcp doctor` both decide these records with
// ingest.DecideRouteFallback.
type RouteFallbackRecord struct {
	RelPath string
	RepType string
	At      string
	By      string
	Dropped bool
}
