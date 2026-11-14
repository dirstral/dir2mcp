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

// resolveSofficeBinary returns the LibreOffice executable for cfg and whether
// one resolved: ingest.libreoffice.command when it is set, else soffice on
// PATH, else libreoffice on PATH (spec 0.93.0 §7.4.B). The configured value is
// one path, not a command template: it is not split on spaces, so a path such
// as `C:\Program Files\LibreOffice\program\soffice.exe` works. A relative
// path (one with a separator) is made absolute against the working directory,
// so the probe and the conversion run the same file. The path itself is never
// put in diagnostics.
func resolveSofficeBinary(cfg config.Config) (string, bool) {
	if bin := strings.TrimSpace(cfg.IngestLibreOfficeCommand); bin != "" {
		if strings.ContainsAny(bin, `/\`) {
			if abs, err := filepath.Abs(bin); err == nil {
				bin = abs
			}
		}
		return bin, true
	}
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
// is stoppable as a whole tree (prepareLibreOfficeCmd).
func runSofficeVersionProbe(ctx context.Context, bin string) error {
	pctx, cancel := context.WithTimeout(ctx, sofficeProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, bin, "--version")
	cmd.Env = os.Environ()
	prepareLibreOfficeCmd(cmd)
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
	if bin, ok := resolveSofficeBinary(cfg); !ok || sofficeFunctionalCheck(bin) != nil {
		missing = append(missing, "soffice (LibreOffice)")
	}
	if !pandocAvailableContext(ctx, cfg) {
		missing = append(missing, "pandoc")
	}
	return missing
}

// libreofficeAvailable reports whether BOTH binaries of the libreoffice engine
// resolve and pass their functional checks AND a temporary directory outside
// the corpus root is usable (libreofficeMakeWorkDir, spec 0.93.0 §7.4.B).
func libreofficeAvailable(cfg config.Config) bool {
	return libreofficeUnavailableReason(context.Background(), cfg) == ""
}

// libreofficeNoWorkBaseReason is the unavailability reason when the binaries
// are functional but no temporary directory outside the corpus root is usable.
const libreofficeNoWorkBaseReason = "no temporary directory outside the corpus root (set TMPDIR to a directory outside the corpus root)"

// libreofficeUnavailableReason returns why the libreoffice engine cannot run
// for cfg, or "" when it can: the missing binaries first, then a missing
// temporary directory outside the corpus root.
func libreofficeUnavailableReason(ctx context.Context, cfg config.Config) string {
	if missing := libreofficeMissing(ctx, cfg); len(missing) > 0 {
		return strings.Join(missing, " and ") + " not found on PATH or failed its functional check"
	}
	if !libreofficeWorkDirUsable(libreofficeCorpusRoot(cfg)) {
		return libreofficeNoWorkBaseReason
	}
	return ""
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
// engine: what to do so legacy .doc files are read. It names every step that
// is still necessary, in this order:
//   - the binaries that are missing (an operator with LibreOffice but no
//     pandoc is told to install pandoc, and the reverse);
//   - else, when no temporary directory outside the corpus root is usable,
//     set TMPDIR;
//   - when the ingest.extractor policy pins another engine, set it to auto or
//     libreoffice, because installing the binaries alone does not turn the
//     engine on under such a pin.
//
// It is empty when nothing is necessary.
func LibreOfficeInstallHint(cfg config.Config) string {
	var steps []string
	if missing := libreofficeMissing(context.Background(), cfg); len(missing) > 0 {
		steps = append(steps, "install "+strings.Join(missing, " and "))
	} else if !libreofficeWorkDirUsable(libreofficeCorpusRoot(cfg)) {
		steps = append(steps, "set TMPDIR to a directory outside the corpus root")
	}
	if !libreofficePolicyAllows(cfg.IngestExtractor) {
		steps = append(steps, "set ingest.extractor to auto or libreoffice")
	}
	if len(steps) == 0 {
		return ""
	}
	return strings.Join(steps, " and ") + " for legacy .doc"
}
