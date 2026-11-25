package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dirstral/dir2mcp/internal/editorial"
)

// editorialImportPollInterval is how often a running daemon looks for a staged
// editorial import between scans. SPEC §7.11.1 asks a running daemon to apply
// a staged import without a restart, no later than its next scan pass; a
// daemon with ingest.watch off has no later scan pass, so it polls.
const editorialImportPollInterval = 5 * time.Second

// editorialImportNotifier is the ingestor capability that reports an applied
// editorial import.
type editorialImportNotifier interface {
	SetOnEditorialImport(fn func(level string, report editorial.Report))
}

// editorialImportPoller is the ingestor capability that applies staged
// editorial imports, and pending rebuilds, between scans.
type editorialImportPoller interface {
	RunEditorialPoller(ctx context.Context, interval time.Duration)
}

// newEditorialImportEmitter emits each applied import's report as the
// `editorial_import_applied` event (SPEC §3.2, §7.11.5), at level warn when a
// key was unmatched, non-media only, conflicting or withheld.
func newEditorialImportEmitter(emitter *ndjsonEmitter) func(level string, report editorial.Report) {
	return func(level string, report editorial.Report) {
		emitter.Emit(level, "editorial_import_applied", report)
	}
}

// wireReindexEditorialEvents reports the editorial imports a reindex applies.
// A reindex writes the index, so it applies staged imports and rebuilds the
// retained ones (SPEC §7.11.1); with --json each applied import is an
// editorial_import_applied event on stdout.
func (a *App) wireReindexEditorialEvents(global globalOptions, ing interface{}) {
	if notifier, ok := ing.(editorialImportNotifier); ok {
		notifier.SetOnEditorialImport(newEditorialImportEmitter(newNDJSONEmitter(a.stdout, global.jsonOutput)))
	}
}

// startEditorialImportPoller applies imports that are staged while the daemon
// runs, and rebuilds a reindex left pending. The ingestor owns the writer
// exclusion: a tick applies only after the first scan, and never while a scan
// or a watch job writes documents.
func startEditorialImportPoller(ctx context.Context, readOnly bool, ing interface{}, wg *sync.WaitGroup) {
	poller, ok := ing.(editorialImportPoller)
	if readOnly || !ok {
		return
	}
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		poller.RunEditorialPoller(ctx, editorialImportPollInterval)
	}()
}

// annotateImportOptions are the parsed operands of `annotate import`.
type annotateImportOptions struct {
	file    string
	source  string
	keyKind string
}

// runAnnotate dispatches the `annotate` subcommands. The only one is `import`
// (SPEC §7.11.1).
func (a *App) runAnnotate(global globalOptions, args []string) int {
	if len(args) == 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			"annotate command: the supported subcommand is import",
			"dir2mcp annotate import <file.jsonl> --source <name> [--key stem|rel_path|content_hash]")
		return exitConfigInvalid
	}
	switch args[0] {
	case "import":
		return a.runAnnotateImport(global, args[1:])
	default:
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("unknown annotate subcommand: %s", args[0]))
		return exitConfigInvalid
	}
}

// runAnnotateImport validates a file of external editorial annotations and
// stages it under the state directory's imports/ (SPEC §7.11.1). It never
// writes meta.sqlite or a vector index: the process that writes the index
// applies the staged import. An invalid record stages nothing and exits 1 with
// each invalid line and its reason.
func (a *App) runAnnotateImport(global globalOptions, args []string) int {
	opts, err := parseAnnotateImportOptions(args)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid annotate import flags: %v", err))
		return exitConfigInvalid
	}
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return exitConfigInvalid
	}
	a.emitConfigWarnings(cfg, global.quiet)
	stateDir := strings.TrimSpace(cfg.StateDir)
	if stateDir == "" {
		stateDir = filepath.Join(".", ".dir2mcp")
	}

	batch, problems, err := readAnnotationFile(opts)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("read %s: %v", opts.file, err))
		return exitGeneric
	}
	if len(problems) > 0 {
		a.reportInvalidAnnotationLines(global, opts.file, problems)
		return exitGeneric
	}

	now := time.Now()
	staged := editorial.NewStaged(batch, opts.source, opts.keyKind, now)
	path, err := editorial.Stage(stateDir, staged, now)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("stage import: %v", err))
		return exitGeneric
	}
	a.reportStagedImport(global, opts, staged, path)
	return exitSuccess
}

// readAnnotationFile opens and validates the import file.
func readAnnotationFile(opts annotateImportOptions) (editorial.Batch, []editorial.LineError, error) {
	f, err := os.Open(opts.file)
	if err != nil {
		return editorial.Batch{}, nil, err
	}
	defer func() { _ = f.Close() }()
	return editorial.ParseFile(f, opts.keyKind, opts.source)
}

// reportInvalidAnnotationLines lists each invalid line with its reason. Line
// reasons name fields, never field values, so no record text is echoed.
func (a *App) reportInvalidAnnotationLines(global globalOptions, file string, problems []editorial.LineError) {
	hints := make([]string, 0, len(problems))
	for _, p := range problems {
		hints = append(hints, fmt.Sprintf("line %d: %s", p.Line, p.Reason))
	}
	message := fmt.Sprintf("%s has %d invalid record(s); nothing was staged", file, len(problems))
	if global.jsonOutput {
		writeCLIError(a.stderr, true, exitGeneric, message, hints...)
		return
	}
	for _, h := range hints {
		writef(a.stderr, "%s\n", h)
	}
	writeCLIError(a.stderr, false, exitGeneric, message)
}

// reportStagedImport prints the staged import, or with --json emits the
// `editorial_import_staged` event (SPEC §3.2, §7.11.1).
func (a *App) reportStagedImport(global globalOptions, opts annotateImportOptions, staged editorial.Staged, path string) {
	if global.jsonOutput {
		emitter := newNDJSONEmitter(a.stdout, true)
		emitter.Emit("info", "editorial_import_staged", map[string]interface{}{
			"source":         opts.source,
			"key":            opts.keyKind,
			"records":        len(staged.Records),
			"ignored_fields": staged.IgnoredFields,
		})
		return
	}
	if global.quiet {
		return
	}
	writef(a.stdout, "staged %d record(s) from source %q (key %s) as %s\n", len(staged.Records), opts.source, opts.keyKind, filepath.Base(path))
	for _, field := range sortedIgnoredFields(staged.IgnoredFields) {
		writef(a.stdout, "ignored field %s in %d record(s)\n", field, staged.IgnoredFields[field])
	}
	writeln(a.stdout, "the process that writes the index applies it: a running `dir2mcp up` on its next pass, or the next `up` or `reindex`")
}

func sortedIgnoredFields(fields map[string]int) []string {
	out := make([]string, 0, len(fields))
	for k := range fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseAnnotateImportOptions parses `<file.jsonl> --source <name> [--key k]`.
// Flags and the file operand may come in any order.
func parseAnnotateImportOptions(args []string) (annotateImportOptions, error) {
	opts := annotateImportOptions{}
	fs := flag.NewFlagSet("annotate import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.source, "source", "", "source name recorded on every annotation (required)")
	fs.StringVar(&opts.keyKind, "key", editorial.KeyStem, "record field that names the document: stem|rel_path|content_hash")
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return annotateImportOptions{}, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	if len(positional) != 1 {
		return annotateImportOptions{}, errors.New("annotate import takes exactly one <file.jsonl> operand")
	}
	opts.file = positional[0]
	if err := editorial.ValidateSource(opts.source); err != nil {
		return annotateImportOptions{}, err
	}
	keyKind, err := editorial.ParseKeyKind(opts.keyKind)
	if err != nil {
		return annotateImportOptions{}, err
	}
	opts.keyKind = keyKind
	return opts, nil
}
