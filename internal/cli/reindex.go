package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"dir2mcp/internal/model"
)

func (a *App) runReindex(ctx context.Context, global globalOptions, args []string) int {
	if len(args) > 0 {
		writef(a.stderr, "reindex command does not accept arguments: %s\n", strings.Join(args, " "))
		return exitConfigInvalid
	}

	// load configuration first so that both the ingestor and any
	// auxiliary components (OCR client) share the same settings.  When
	// Load returns an error we treat it as fatal instead of silently
	// proceeding with defaults as was previously the case.
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writef(a.stderr, "load config: %v\n", err)
		return exitConfigInvalid
	}

	baseDir := strings.TrimSpace(cfg.StateDir)
	if baseDir == "" {
		baseDir = ".dir2mcp"
	}
	// ensure the directory exists before we let the store constructor write
	// to it
	textIndexPath := filepath.Join(baseDir, "vectors_text.hnsw")
	codeIndexPath := filepath.Join(baseDir, "vectors_code.hnsw")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		writef(a.stderr, "create state dir: %v\n", err)
		return exitRootInaccessible
	}
	// update cfg so that the store factory uses the same baseDir
	cfg.StateDir = baseDir
	st := a.storeForConfig(cfg)
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			writef(a.stderr, "close store: %v\n", closeErr)
		}
	}()
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		writef(a.stderr, "initialize metadata store: %v\n", err)
		return exitIndexLoadFailure
	}
	if resetter, ok := interface{}(st).(contentHashResetter); ok {
		if err := resetter.ClearDocumentContentHashes(ctx); err != nil {
			writef(a.stderr, "clear content hashes: %v\n", err)
			return exitGeneric
		}
	}
	for _, indexPath := range []string{textIndexPath, codeIndexPath} {
		if err := os.Remove(indexPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			writef(a.stderr, "remove stale index file %s: %v\n", indexPath, err)
			return exitGeneric
		}
	}

	// use the factory hook (same as runUp) to allow tests to intercept
	ing, err := a.newIngestor(cfg, st)
	if err != nil {
		writef(a.stderr, "initialize ingestor: %v\n", err)
		return exitConfigInvalid
	}

	err = ing.Reindex(ctx)
	if errors.Is(err, model.ErrNotImplemented) {
		if !global.quiet {
			writeln(a.stdout, "reindex is not available yet: ingestion pipeline not implemented")
		}
		return exitSuccess
	}
	if err != nil {
		writef(a.stderr, "reindex failed: %v\n", err)
		return exitGeneric
	}
	return exitSuccess
}
