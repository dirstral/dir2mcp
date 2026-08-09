package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/fsnotify/fsnotify"
)

// Issue #773: the file watcher must apply the operator's `ingest.exclude_dirs`
// list, not a hardcoded default list. It is an internal test because it drives
// the unexported addWatchDirs directly: that registers the watches from the
// resolved list without waiting on real fsnotify event timing, so the parity
// check is deterministic.
//
// Parity with the initial scan is the point. The watcher once kept its own copy
// of the ignore list and the two drifted (#716), so a directory could be
// excluded from the scan and still picked up by the watcher. Both now read one
// resolved value: DiscoverOptionsFromConfig(cfg).ExcludedDirs().

// watchedDirs registers watches for absRoot under opts and returns the
// registered directory paths as a set.
func watchedDirs(t *testing.T, absRoot string, opts DiscoverOptions) map[string]bool {
	t.Helper()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	svc := &Service{}
	if err := svc.addWatchDirs(watcher, absRoot, opts); err != nil {
		t.Fatalf("addWatchDirs: %v", err)
	}
	registered := map[string]bool{}
	for _, dir := range watcher.WatchList() {
		registered[dir] = true
	}
	return registered
}

// watchExcludeCorpus creates one directory per name the tests care about.
func watchExcludeCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"dist", "node_modules", "notes", ".dir2mcp"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	return root
}

// TestAddWatchDirs_HonorsConfiguredExcludeDirs pins the watcher to the
// operator's list. `dist` leaves the list, so the watcher must watch it; the
// list names `notes`, so the watcher must skip it.
func TestAddWatchDirs_HonorsConfiguredExcludeDirs(t *testing.T) {
	root := watchExcludeCorpus(t)

	cfg := config.Default()
	cfg.RootDir = root
	cfg.IngestExcludeDirs = []string{"notes", "node_modules"}
	registered := watchedDirs(t, root, DiscoverOptionsFromConfig(cfg))

	if !registered[filepath.Join(root, "dist")] {
		t.Errorf("dist must be watched once it leaves ingest.exclude_dirs; watched %v", registered)
	}
	if registered[filepath.Join(root, "notes")] {
		t.Errorf("notes is listed, so it must not be watched; watched %v", registered)
	}
	if registered[filepath.Join(root, "node_modules")] {
		t.Errorf("node_modules is listed, so it must not be watched; watched %v", registered)
	}
	if registered[filepath.Join(root, ".dir2mcp")] {
		t.Errorf(".dir2mcp must not be watched even when the list omits it; watched %v", registered)
	}
}

// TestAddWatchDirs_DefaultsUnchangedWhenNoListIsSet guards the existing corpus:
// with no configured list the watcher keeps the default names.
func TestAddWatchDirs_DefaultsUnchangedWhenNoListIsSet(t *testing.T) {
	root := watchExcludeCorpus(t)

	cfg := config.Default()
	cfg.RootDir = root
	registered := watchedDirs(t, root, DiscoverOptionsFromConfig(cfg))

	if !registered[filepath.Join(root, "notes")] {
		t.Errorf("an ordinary directory must be watched; watched %v", registered)
	}
	for _, name := range []string{"dist", "node_modules", ".dir2mcp"} {
		if registered[filepath.Join(root, name)] {
			t.Errorf("%s is a default excluded name, so it must not be watched; watched %v", name, registered)
		}
	}
}

// TestWatcherAndScanShareOneExcludedSet states the invariant directly: the set
// the watcher applies is the set the scan applies, because both come from the
// same DiscoverOptions.
func TestWatcherAndScanShareOneExcludedSet(t *testing.T) {
	cfg := config.Default()
	cfg.IngestExcludeDirs = []string{"notes"}
	opts := DiscoverOptionsFromConfig(cfg)

	scanned := opts.corpusfsOptions().ExcludeDirs
	if len(scanned) != len(opts.ExcludeDirs) {
		t.Fatalf("the scan options must carry the configured list; got %v", scanned)
	}
	watched := opts.ExcludedDirs()
	if !watched.Has("notes") || !watched.Has(".dir2mcp") || watched.Has("dist") {
		t.Errorf("the watcher set must resolve the configured list; got %v", watched.Names())
	}
}
