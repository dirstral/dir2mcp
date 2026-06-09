package ingest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
)

// doclingProbeTimeout bounds the docling functional check so a hung binary
// cannot stall startup or `dir2mcp doctor`.
const doclingProbeTimeout = 20 * time.Second

// doclingProbeEntry memoizes one binary's functional-check result; the
// sync.Once ensures the probe runs at most once per binary even under
// concurrent callers.
type doclingProbeEntry struct {
	once sync.Once
	err  error
}

// doclingProbeEntries caches functional-check results per resolved binary for
// the process lifetime. Spec 0.15.0 §7.4 says implementations SHOULD cache the
// result for the run rather than probing per document; a long-running daemon
// whose docling is repaired mid-run picks up the change on the next
// `dir2mcp up`.
var (
	doclingProbeMu      sync.Mutex
	doclingProbeEntries = map[string]*doclingProbeEntry{}
)

// resolveDoclingBinary returns the docling executable that would run for cfg,
// how it was resolved ("command" from ingest.docling.command, or "path" from
// PATH), and whether it resolved at all. The binary value itself is never
// surfaced in diagnostics (a user command/path may be sensitive); callers map
// source to a redacted reason via doclingResolvedReason.
func resolveDoclingBinary(cfg config.Config) (bin, source string, ok bool) {
	if tpl := strings.TrimSpace(cfg.DoclingCommand); tpl != "" {
		if fields := strings.Fields(tpl); len(fields) > 0 {
			return fields[0], "command", true
		}
	}
	if p, err := exec.LookPath("docling"); err == nil {
		return p, "path", true
	}
	return "", "", false
}

// doclingResolvedReason maps a resolveDoclingBinary source to the redacted
// reason used in diagnostics.
func doclingResolvedReason(source string) string {
	if source == "path" {
		return "auto-detected on PATH"
	}
	return "configured docling command"
}

// doclingFunctionalCheck reports whether a resolved docling binary actually
// runs (spec 0.15.0 §7.4: an extractor is available only when it resolves AND
// passes a lightweight functional check). A docling whose bundled virtualenv
// has ABI-incompatible dependencies resolves but crashes at import, so it must
// be treated as unavailable rather than selected and then failing every
// document.
//
// The check applies only to actual docling binaries (basename "docling"),
// which covers the bundled venv and a PATH docling — the cases where the import
// crash occurs. A custom ingest.docling.command may point at a wrapper that does
// not understand --version, so such commands keep the prior "resolvable ==
// available" behavior and are not probed.
func doclingFunctionalCheck(ctx context.Context, bin string) error {
	if !looksLikeDocling(bin) {
		return nil
	}

	doclingProbeMu.Lock()
	entry, ok := doclingProbeEntries[bin]
	if !ok {
		entry = &doclingProbeEntry{}
		doclingProbeEntries[bin] = entry
	}
	doclingProbeMu.Unlock()

	// At most one probe per binary; concurrent callers for the same bin wait,
	// callers for other bins proceed independently.
	entry.once.Do(func() {
		entry.err = runDoclingVersionProbe(ctx, bin)
	})
	return entry.err
}

// looksLikeDocling reports whether bin's base name identifies the real docling
// CLI (which supports `--version`).
func looksLikeDocling(bin string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(bin)))
	base = strings.TrimSuffix(base, ".exe")
	return base == "docling"
}

// runDoclingVersionProbe runs `<bin> --version` with a sanitized environment
// (see SanitizeDoclingEnv) and a bounded timeout, discarding output. A non-nil
// return means the binary is present but non-functional.
func runDoclingVersionProbe(ctx context.Context, bin string) error {
	pctx, cancel := context.WithTimeout(ctx, doclingProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, bin, "--version")
	cmd.Env = SanitizeDoclingEnv(os.Environ())
	return cmd.Run()
}
