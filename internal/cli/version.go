package cli

// Build metadata injected at release time via ldflags.
// Defaults are used for local development builds.
var (
	Version = "v0.0.0-dev"
	Commit  = "none"
	Date    = "unknown"
)
