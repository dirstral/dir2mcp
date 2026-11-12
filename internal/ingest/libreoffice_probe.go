package ingest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
)

// The libreoffice extraction engine (T2, spec 0.93.0) reads legacy binary Word
// (.doc). It converts the file to .docx with headless LibreOffice and reads the
// .docx with the pandoc docx path, so it is capability-activated on TWO
// binaries: a functional `soffice` (or `libreoffice`) AND a functional pandoc.
// When either is missing the engine is inactive, exactly as a missing pandoc
// deactivates the pandoc engine, and a .doc is a §7.4.B.2 coverage gap.

// sofficeProbeTimeout bounds the `soffice --version` functional check so a hung
// LibreOffice cannot stall startup or `dir2mcp doctor`. LibreOffice starts more
// slowly than pandoc (a few seconds on a cold disk), so the limit is longer than
// pandocProbeTimeout. The result is memoized per process.
const sofficeProbeTimeout = 30 * time.Second

// sofficeBinaryNames are the executable names the engine looks up on PATH, in
// order. Debian/Ubuntu and Homebrew install `soffice`; some distributions ship
// only a `libreoffice` launcher.
var sofficeBinaryNames = []string{"soffice", "libreoffice"}

// sofficeProbeEntry memoizes one binary's functional-check result; the sync.Once
// makes sure the probe runs at most once per binary under concurrent callers.
type sofficeProbeEntry struct {
	once sync.Once
	err  error
}

var (
	sofficeProbeMu      sync.Mutex
	sofficeProbeEntries = map[string]*sofficeProbeEntry{}
)

// resolveSofficeBinary returns the LibreOffice executable found on PATH (soffice
// first, then libreoffice) and whether one resolved. The path itself is never
// put in diagnostics.
func resolveSofficeBinary() (string, bool) {
	for _, name := range sofficeBinaryNames {
		if p, err := exec.LookPath(name); err == nil {
			return p, true
		}
	}
	return "", false
}

// sofficeFunctionalCheck reports whether a resolved LibreOffice binary runs
// (spec §7.4: a capability-activated engine is available only when it resolves
// AND passes a lightweight functional check). The result is cached per binary
// for the process lifetime; a LibreOffice installed mid-run is picked up on the
// next `dir2mcp up`.
func sofficeFunctionalCheck(bin string) error {
	sofficeProbeMu.Lock()
	entry, ok := sofficeProbeEntries[bin]
	if !ok {
		entry = &sofficeProbeEntry{}
		sofficeProbeEntries[bin] = entry
	}
	sofficeProbeMu.Unlock()
	entry.once.Do(func() {
		// Detached from any request context for the same reason as the pandoc
		// probe: the memoized verdict must not record a cancelled request as a
		// broken binary.
		entry.err = runSofficeVersionProbe(context.Background(), bin)
	})
	return entry.err
}

// runSofficeVersionProbe runs `<bin> --version` with a bounded timeout and
// discards the output. LibreOffice can start helper processes, so the probe
// runs in its own process group and a timeout kills the whole group.
func runSofficeVersionProbe(ctx context.Context, bin string) error {
	pctx, cancel := context.WithTimeout(ctx, sofficeProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, bin, "--version")
	cmd.Env = os.Environ()
	setRecognizeBackendProcAttr(cmd)
	cmd.Cancel = func() error { return signalRecognizeBackend(cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = pandocWaitDelay
	err := cmd.Run()
	reapLibreOfficeGroup(cmd)
	return err
}

// libreofficeMissing names, in a stable order, the binaries the libreoffice
// engine needs that do not resolve or fail their functional check. An empty
// result means both binaries are functional. It is the single source of the
// "what to install" wording in the startup banner, doctor and coverage hint.
func libreofficeMissing(ctx context.Context, cfg config.Config) []string {
	var missing []string
	if bin, ok := resolveSofficeBinary(); !ok || sofficeFunctionalCheck(bin) != nil {
		missing = append(missing, "soffice (LibreOffice)")
	}
	if !pandocAvailableContext(ctx, cfg) {
		missing = append(missing, "pandoc")
	}
	return missing
}

// libreofficeAvailable reports whether BOTH binaries of the libreoffice engine
// resolve and pass their functional checks.
func libreofficeAvailable(cfg config.Config) bool {
	return len(libreofficeMissing(context.Background(), cfg)) == 0
}

// libreofficePolicyAllows reports whether the ingest.extractor policy permits
// the libreoffice engine: `auto` (an additive T2 engine) and the explicit
// `libreoffice` pin. Under any other pin (including `pandoc`) it is not
// activated: a pin gets no cross-engine fallback.
func libreofficePolicyAllows(policy string) bool {
	p := strings.ToLower(strings.TrimSpace(policy))
	return p == "" || p == "auto" || p == "libreoffice"
}

// libreofficeEngineActive reports whether the libreoffice engine is active for
// cfg: the policy permits it AND both binaries are functional. NewService wiring
// (ActivateLibreOfficeEngine) and the doctor coverage check share it, so the two
// cannot drift.
func libreofficeEngineActive(cfg config.Config) bool {
	return libreofficePolicyAllows(cfg.IngestExtractor) && libreofficeAvailable(cfg)
}

// LibreOfficeActive is the exported wrapper over libreofficeEngineActive for
// out-of-package diagnostics (the doctor and `up` coverage checks).
func LibreOfficeActive(cfg config.Config) bool {
	return libreofficeEngineActive(cfg)
}

// LibreOfficeSupportsExt reports whether the libreoffice engine reads ext
// (lowercased, with its leading dot).
func LibreOfficeSupportsExt(ext string) bool {
	return engineSupportsExt(engineLibreOffice, ext)
}

// LibreOfficeInstallHint returns the coverage remedy for the libreoffice
// engine: what to install so legacy .doc files are read. It names only the
// binaries that are missing, so an operator with LibreOffice but no pandoc is
// told to install pandoc, and the reverse. It is empty when both are present.
func LibreOfficeInstallHint(cfg config.Config) string {
	missing := libreofficeMissing(context.Background(), cfg)
	if len(missing) == 0 {
		return ""
	}
	return "install " + strings.Join(missing, " and ") + " for legacy .doc"
}
