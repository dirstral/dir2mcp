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
	// DroppedCached is set when the dropped windows' fallback text is still in
	// the transcript cache (a drop under on_route_error: fail). Without it the
	// dropped windows have no text left and are retried when due.
	DroppedCached bool
	// SkipReason is route_fallback_skip_reason: why the last due retry could
	// not run (the host could not extract, stage, probe or cut the audio, or
	// the decode schedule did not match). Empty when the last retry ran.
	SkipReason string
}
