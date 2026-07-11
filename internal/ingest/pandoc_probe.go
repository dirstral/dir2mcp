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

// pandocProbeTimeout bounds the pandoc functional check so a hung binary cannot
// stall startup or `dir2mcp doctor`. Unlike docling (which imports the whole
// torch/transformers stack and needs a 90s cold-start ceiling), pandoc is a fast
// native binary: `pandoc --version` returns in milliseconds, so a short 10s
// timeout is generous headroom while still bounding a genuinely hung binary. The
// probe result is memoized per process, so only the first probe pays it.
const pandocProbeTimeout = 10 * time.Second

// pandocProbeEntry memoizes one binary's functional-check result; the sync.Once
// ensures the probe runs at most once per binary even under concurrent callers.
type pandocProbeEntry struct {
	once sync.Once
	err  error
}

// pandocProbeEntries caches functional-check results per resolved binary for the
// process lifetime (spec §7.4: cache the result for the run rather than probing
// per document). A long-running daemon whose pandoc is installed mid-run picks up
// the change on the next `dir2mcp up`.
var (
	pandocProbeMu      sync.Mutex
	pandocProbeEntries = map[string]*pandocProbeEntry{}
)

// resolvePandocBinary returns the pandoc executable that would run for cfg, how it
// was resolved ("command" from ingest.pandoc.command, or "path" from PATH), and
// whether it resolved at all. The binary value itself is never surfaced in
// diagnostics (a user command/path may be sensitive); callers map source to a
// redacted reason via pandocResolvedReason.
func resolvePandocBinary(cfg config.Config) (bin, source string, ok bool) {
	if tpl := strings.TrimSpace(cfg.IngestPandocCommand); tpl != "" {
		if fields := strings.Fields(tpl); len(fields) > 0 {
			return fields[0], "command", true
		}
	}
	if p, err := exec.LookPath("pandoc"); err == nil {
		return p, "path", true
	}
	return "", "", false
}

// pandocResolvedReason maps a resolvePandocBinary source to the redacted reason
// used in diagnostics. The literal command/path is never embedded (mirrors
// doclingResolvedReason) so no user-supplied value flows into the banner or the
// support bundle.
func pandocResolvedReason(source string) string {
	if source == "path" {
		return "auto-detected on PATH"
	}
	return "configured pandoc command"
}

// pandocFunctionalCheck reports whether a resolved pandoc binary actually runs
// (spec §7.4: a capability-activated engine is available only when it resolves
// AND passes a lightweight functional check). A pandoc that resolves but crashes
// on `--version` must be treated as unavailable rather than selected and then
// failing every document.
//
// The check applies only to actual pandoc binaries (basename "pandoc"). A custom
// ingest.pandoc.command may point at a wrapper that does not understand
// --version, so such commands keep the prior "resolvable == available" behavior
// and are not probed (mirrors doclingFunctionalCheck).
func pandocFunctionalCheck(ctx context.Context, bin string) error {
	if !looksLikePandoc(bin) {
		return nil
	}

	pandocProbeMu.Lock()
	entry, ok := pandocProbeEntries[bin]
	if !ok {
		entry = &pandocProbeEntry{}
		pandocProbeEntries[bin] = entry
	}
	pandocProbeMu.Unlock()

	// At most one probe per binary; concurrent callers for the same bin wait,
	// callers for other bins proceed independently.
	entry.once.Do(func() {
		// Detach from the caller's context: this result is memoized process-wide, so
		// a cancelled or expired REQUEST context must not permanently cache a healthy
		// pandoc as unavailable for the rest of the process (CodeRabbit #586). The
		// fixed pandocProbeTimeout is the only deadline; a genuinely hung binary is
		// still bounded. ctx is retained on the signature for caller symmetry with
		// doclingFunctionalCheck and to document the cancellable intent of the call.
		_ = ctx
		entry.err = runPandocVersionProbe(context.Background(), bin)
	})
	return entry.err
}

// looksLikePandoc reports whether bin's base name identifies the real pandoc CLI
// (which supports `--version`).
func looksLikePandoc(bin string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(bin)))
	base = strings.TrimSuffix(base, ".exe")
	return base == "pandoc"
}

// runPandocVersionProbe runs `<bin> --version` with a plain environment and a
// bounded timeout, discarding output. pandoc is a self-contained native binary
// and needs no environment sanitization (unlike docling's Python venv). A non-nil
// return means the binary is present but non-functional.
func runPandocVersionProbe(ctx context.Context, bin string) error {
	pctx, cancel := context.WithTimeout(ctx, pandocProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, bin, "--version")
	cmd.Env = os.Environ()
	return cmd.Run()
}

// pandocAvailable reports whether pandoc resolves for cfg AND passes its
// functional check. It uses a background context; callers on hot paths that hold
// a request context should use pandocAvailableContext so the probe is cancellable.
func pandocAvailable(cfg config.Config) bool {
	return pandocAvailableContext(context.Background(), cfg)
}

// pandocAvailableContext is pandocAvailable with a caller-provided context threaded
// into the functional-check probe.
func pandocAvailableContext(ctx context.Context, cfg config.Config) bool {
	bin, _, ok := resolvePandocBinary(cfg)
	if !ok {
		return false
	}
	return pandocFunctionalCheck(ctx, bin) == nil
}

// pandocPolicyAllows reports whether the ingest.extractor policy permits pandoc as
// an active engine: only `auto` (pandoc is an additive secondary engine) and the
// explicit `pandoc` pin. Under any other pin (docling/docling-serve/mistral/off)
// pandoc is not activated.
func pandocPolicyAllows(policy string) bool {
	p := strings.ToLower(strings.TrimSpace(policy))
	return p == "" || p == "auto" || p == "pandoc"
}

// pandocEngineActive reports whether pandoc is an active extraction engine for
// cfg: the policy permits it AND a functional pandoc resolves. It is the single
// source of truth shared by NewService (which builds the extractor) and the
// doctor coverage check (via PandocActive), so the availability the doctor reports
// cannot drift from the engine indexing will actually run.
func pandocEngineActive(cfg config.Config) bool {
	return pandocPolicyAllows(cfg.IngestExtractor) && pandocAvailable(cfg)
}

// PandocActive is the exported wrapper over pandocEngineActive so out-of-package
// diagnostics (the doctor extraction-coverage check) can report pandoc's covered
// formats without duplicating the policy+availability logic.
func PandocActive(cfg config.Config) bool {
	return pandocEngineActive(cfg)
}

// PandocSupportsExt is the exported wrapper over the pandoc engine's readable set
// so the doctor coverage check can report pandoc-covered formats. ext is expected
// lowercased with its leading dot.
func PandocSupportsExt(ext string) bool {
	return engineSupportsExt(enginePandoc, ext)
}
