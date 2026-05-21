package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/buildinfo"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// supportLogTailBytes caps how much of server.log is included in the
// bundle. Keeps bundles shareable over chat and email while still
// capturing a meaningful window of recent activity.
const supportLogTailBytes int64 = 2 * 1024 * 1024 // 2 MB

// runSupportBundle implements `dir2mcp support-bundle`: it gathers a
// small set of files (effective config snapshot, server log tail,
// status JSON, list-files JSON, version, OS info, routing decisions)
// into a single tar.gz suitable for sharing with a maintainer when
// diagnosing install/indexing problems. No secret values are included
// — the effective config snapshot only carries `secret_sources:`
// metadata (where each credential came from), not the credentials
// themselves.
func (a *App) runSupportBundle(ctx context.Context, global globalOptions, args []string) int {
	fs := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	// Route flag-parse output through writeCLIError so JSON callers get
	// a single structured error object on stdout/stderr instead of the
	// flag package's prose mixed with our JSON envelope.
	fs.SetOutput(io.Discard)
	output := fs.String("output", "", "destination tar.gz path (default: ./dir2mcp-support-<timestamp>.tar.gz)")
	if err := fs.Parse(args); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("support-bundle: %v", err))
		return exitConfigInvalid
	}
	if rest := fs.Args(); len(rest) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			fmt.Sprintf("support-bundle does not accept positional arguments: %s", strings.Join(rest, " ")))
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

	destPath := strings.TrimSpace(*output)
	if destPath == "" {
		destPath = filepath.Join(".", fmt.Sprintf("dir2mcp-support-%s.tar.gz", time.Now().UTC().Format("20060102-150405")))
	}

	files, warnings := collectSupportArtifacts(ctx, a, cfg)

	if err := writeSupportBundle(destPath, files); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("write support bundle: %v", err))
		return exitGeneric
	}

	if global.jsonOutput {
		_ = emitJSON(a.stdout, map[string]interface{}{
			"output":   destPath,
			"entries":  supportEntryNames(files),
			"warnings": warningStrings(warnings),
		})
		return exitSuccess
	}

	if !global.quiet {
		_, _ = fmt.Fprintf(a.stdout, "Wrote support bundle to %s\n", destPath)
		for _, w := range warnings {
			_, _ = fmt.Fprintf(a.stderr, "warning: %v\n", w)
		}
	}
	return exitSuccess
}

// supportFile is one entry in the tar.gz: a logical name (relative path
// inside the archive) and the bytes to write. Empty Bytes is allowed —
// the entry is still written, which is useful for marking "this artifact
// would have been collected but the source was missing".
type supportFile struct {
	Name  string
	Bytes []byte
}

// collectSupportArtifacts assembles every artifact for the bundle.
// Missing or unreadable sources are recorded as warnings rather than
// fatal errors so the bundle is always produced even when the daemon
// is down or the index is empty.
func collectSupportArtifacts(ctx context.Context, a *App, cfg config.Config) ([]supportFile, []error) {
	var files []supportFile
	var warnings []error

	add := func(name string, body []byte, err error) {
		if err != nil {
			warnings = append(warnings, fmt.Errorf("%s: %w", name, err))
			return
		}
		files = append(files, supportFile{Name: name, Bytes: body})
	}

	add("version.txt", []byte(buildinfo.Display()+"\n"), nil)
	add("os.txt", []byte(fmt.Sprintf("GOOS=%s\nGOARCH=%s\n", runtime.GOOS, runtime.GOARCH)), nil)

	cfgBytes, cfgErr := readFileBest(config.EffectiveSnapshotPath(cfg.StateDir))
	add("config.snapshot.yaml", cfgBytes, cfgErr)

	logBytes, logErr := readLogTail(serverLogPath(cfg.StateDir), supportLogTailBytes)
	add("server.log", logBytes, logErr)

	statusBytes, statusErr := marshalStatusJSON(ctx, a, cfg)
	add("status.json", statusBytes, statusErr)

	listBytes, listErr := marshalListFilesJSON(ctx, a, cfg)
	add("list-files.json", listBytes, listErr)

	routingBytes, routingErr := marshalRoutingJSON(cfg)
	add("routing.json", routingBytes, routingErr)

	return files, warnings
}

// readFileBest reads a file if it exists, returning (bytes, nil) on
// success, ([]byte(nil), nil) when the path is missing (no warning —
// the support bundle is expected to run on partial/clean state dirs),
// or ([]byte(nil), err) on a real read failure.
func readFileBest(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}

// readLogTail returns the last maxBytes of path, or the whole file if
// it is smaller. Missing log file is silent (returns nil bytes); other
// errors propagate as a warning.
func readLogTail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := stat.Size()
	if size <= maxBytes {
		return io.ReadAll(f)
	}
	if _, err := f.Seek(-maxBytes, io.SeekEnd); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

// marshalStatusJSON computes the same payload `dir2mcp status --json`
// would emit. Falls back to a placeholder when no state is present so
// the bundle still records "we tried and found nothing".
func marshalStatusJSON(ctx context.Context, a *App, cfg config.Config) ([]byte, error) {
	snapshotPath := filepath.Join(cfg.StateDir, "corpus.json")
	snapshot, err := readCorpusSnapshot(snapshotPath)
	source := "corpus_json"
	if err != nil {
		metaPath := filepath.Join(cfg.StateDir, "meta.sqlite")
		if _, statErr := os.Stat(metaPath); statErr != nil {
			return json.MarshalIndent(map[string]interface{}{
				"available": false,
				"reason":    "no state found",
			}, "", "  ")
		}
		st := a.storeForConfig(cfg)
		defer func() { _ = st.Close() }()
		if initErr := st.Init(ctx); initErr != nil && !errors.Is(initErr, model.ErrNotImplemented) {
			return nil, fmt.Errorf("initialize store: %w", initErr)
		}
		emitter := newNDJSONEmitter(io.Discard, false)
		snapshot, err = buildCorpusSnapshot(ctx, st, nil, io.Discard, emitter)
		if err != nil {
			return nil, fmt.Errorf("build snapshot: %w", err)
		}
		source = "computed"
	}
	return json.MarshalIndent(map[string]interface{}{
		"source":    source,
		"state_dir": cfg.StateDir,
		"snapshot":  snapshot,
	}, "", "  ")
}

// marshalListFilesJSON returns the same payload `dir2mcp list-files
// --json` would emit. Reads directly from the sqlite store so it works
// without the daemon running.
func marshalListFilesJSON(ctx context.Context, a *App, cfg config.Config) ([]byte, error) {
	metaPath := filepath.Join(cfg.StateDir, "meta.sqlite")
	if _, err := os.Stat(metaPath); err != nil {
		return json.MarshalIndent(map[string]interface{}{
			"available": false,
			"reason":    "no meta.sqlite",
		}, "", "  ")
	}
	st := a.storeForConfig(cfg)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		return nil, fmt.Errorf("initialize store: %w", err)
	}
	docs, total, err := st.ListFiles(ctx, "", "", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	return json.MarshalIndent(map[string]interface{}{
		"files": docs,
		"total": total,
	}, "", "  ")
}

// marshalRoutingJSON dumps the provider/extractor routing decisions
// (same data the `up` banner Models section shows). This is the
// fastest way for a maintainer to see whether the wrong OCR backend
// was selected without needing to ask the user to re-run `up`.
func marshalRoutingJSON(cfg config.Config) ([]byte, error) {
	return json.MarshalIndent(map[string]interface{}{
		"decisions": routingDecisions(cfg),
	}, "", "  ")
}

// writeSupportBundle writes files as a single gzip-compressed tarball
// at destPath. Directories in destPath must already exist. Mode 0o600
// keeps the bundle owner-readable only — its contents include the
// server log and diagnostic metadata that should not be exposed to
// other local users by an unfavourable umask.
func writeSupportBundle(destPath string, files []supportFile) error {
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	defer func() { _ = gz.Close() }()
	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close() }()

	now := time.Now()
	for _, file := range files {
		hdr := &tar.Header{
			Name:    file.Name,
			Mode:    0o644,
			Size:    int64(len(file.Bytes)),
			ModTime: now,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(file.Bytes); err != nil {
			return err
		}
	}
	return nil
}

func supportEntryNames(files []supportFile) []string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name)
	}
	return names
}

func warningStrings(errs []error) []string {
	if len(errs) == 0 {
		return nil
	}
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Error())
	}
	return out
}
