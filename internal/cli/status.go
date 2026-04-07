package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dir2mcp/internal/model"
)

func (a *App) runStatus(ctx context.Context, global globalOptions, args []string) int {
	if len(args) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("status command does not accept arguments: %s", strings.Join(args, " ")))
		return exitConfigInvalid
	}

	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return exitConfigInvalid
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		cfg.StateDir = filepath.Join(".", ".dir2mcp")
	}

	snapshotPath := filepath.Join(cfg.StateDir, "corpus.json")
	snapshot, err := readCorpusSnapshot(snapshotPath)
	source := "corpus_json"
	if err != nil {
		metaPath := filepath.Join(cfg.StateDir, "meta.sqlite")
		if _, statErr := os.Stat(metaPath); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("no state found in %s; run: dir2mcp up", cfg.StateDir))
				return exitGeneric
			}
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("read state: %v", statErr))
			return exitGeneric
		}

		st := a.storeForConfig(cfg)
		defer func() { _ = st.Close() }()
		if initErr := st.Init(ctx); initErr != nil && !errors.Is(initErr, model.ErrNotImplemented) {
			writeCLIError(a.stderr, global.jsonOutput, exitIndexLoadFailure, fmt.Sprintf("initialize metadata store: %v", initErr))
			return exitIndexLoadFailure
		}
		// status --json must emit a single JSON object, not an NDJSON stream.
		// Keep the emitter disabled so computed-snapshot warnings go to stderr.
		emitter := newNDJSONEmitter(a.stdout, false)
		snapshot, err = buildCorpusSnapshot(ctx, st, nil, a.stderr, emitter)
		if err != nil {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("build status snapshot: %v", err))
			return exitGeneric
		}
		source = "computed"
	}

	return a.renderStatusOutput(global, cfg.StateDir, snapshot, source)
}

func (a *App) renderStatusOutput(global globalOptions, stateDir string, snapshot corpusSnapshot, source string) int {
	if global.jsonOutput {
		payload := map[string]interface{}{
			"source":    source,
			"state_dir": stateDir,
			"snapshot":  snapshot,
		}
		if err := emitJSON(a.stdout, payload); err != nil {
			writeCLIError(a.stderr, true, exitGeneric, fmt.Sprintf("encode status json: %v", err))
			return exitGeneric
		}
		return exitSuccess
	}

	if global.quiet {
		return exitSuccess
	}
	s := a.sty(false)
	writeln(a.stdout)
	writeln(a.stdout, s.kv("State", stateDir))
	writeln(a.stdout, s.kv("Source", source))
	writeln(a.stdout, s.kv("Timestamp", snapshot.Timestamp))
	writeln(a.stdout)

	runningLabel := s.dim("stopped")
	if snapshot.Indexing.Running {
		runningLabel = s.Green.Render("running")
	}

	writef(a.stdout, "  %s  %s  %s\n", s.sectionHeader("Indexing"), s.dim("mode="+snapshot.Indexing.Mode), runningLabel)
	writef(a.stdout, "    %s  %s  %s  %s\n",
		s.stat("scanned", snapshot.Indexing.Scanned),
		s.stat("indexed", snapshot.Indexing.Indexed),
		s.stat("skipped", snapshot.Indexing.Skipped),
		s.stat("deleted", snapshot.Indexing.Deleted),
	)
	writef(a.stdout, "    %s  %s  %s  %s",
		s.stat("reps", snapshot.Indexing.Representations),
		s.stat("chunks", snapshot.Indexing.ChunksTotal),
		s.stat("embedded", snapshot.Indexing.EmbeddedOK),
		s.stat("unknown", snapshot.Indexing.Unknown),
	)
	if snapshot.Indexing.Errors > 0 {
		writef(a.stdout, "  %s", s.Red.Render(fmt.Sprintf("errors=%d", snapshot.Indexing.Errors)))
	} else {
		writef(a.stdout, "  %s", s.stat("errors", snapshot.Indexing.Errors))
	}
	writeln(a.stdout)
	writeln(a.stdout)

	writef(a.stdout, "  %s  %s  %s\n",
		s.sectionHeader("Documents"),
		s.stat("total", snapshot.TotalDocs),
		s.stat("code_ratio", fmt.Sprintf("%.4f", snapshot.CodeRatio)),
	)
	if len(snapshot.DocCounts) > 0 {
		keys := make([]string, 0, len(snapshot.DocCounts))
		for key := range snapshot.DocCounts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writef(a.stdout, "    %s\n", s.stat(key, snapshot.DocCounts[key]))
		}
	}
	writeln(a.stdout)
	return exitSuccess
}
