package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dir2mcp/internal/model"
)

func (a *App) runReindex(ctx context.Context, global globalOptions, args []string) int {
	if len(args) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("reindex command does not accept arguments: %s", strings.Join(args, " ")))
		return exitConfigInvalid
	}

	// load configuration first so that both the ingestor and any
	// auxiliary components (OCR client) share the same settings.  When
	// Load returns an error we treat it as fatal instead of silently
	// proceeding with defaults as was previously the case.
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
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
		writeCLIError(a.stderr, global.jsonOutput, exitRootInaccessible, fmt.Sprintf("create state dir: %v", err))
		return exitRootInaccessible
	}
	// update cfg so that the store factory uses the same baseDir
	cfg.StateDir = baseDir
	st := a.storeForConfig(cfg)
	defer a.closeStoreWithLog(st)
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		writeCLIError(a.stderr, global.jsonOutput, exitIndexLoadFailure, fmt.Sprintf("initialize metadata store: %v", err))
		return exitIndexLoadFailure
	}
	if exitCode := clearContentHashesIfSupported(ctx, st, a.stderr, global.jsonOutput); exitCode != exitSuccess {
		return exitCode
	}
	for _, indexPath := range []string{textIndexPath, codeIndexPath} {
		if err := os.Remove(indexPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("remove stale index file %s: %v", indexPath, err))
			return exitGeneric
		}
	}

	// use the factory hook (same as runUp) to allow tests to intercept
	ing, err := a.newIngestor(cfg, st)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("initialize ingestor: %v", err))
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
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("reindex failed: %v", err))
		return exitGeneric
	}
	return exitSuccess
}
