package tests

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// Issue #773 / SPEC §7.1: `ingest.exclude_dirs` replaces the directory ignore
// list. These tests cover the loader side: the key spelling, the replace
// semantics, the forced `.dir2mcp`, and the entry shape.

// writeExcludeDirsConfig writes body to a temp .dir2mcp.yaml and loads it.
func writeExcludeDirsConfig(t *testing.T, body string) (config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return config.LoadFile(path)
}

// TestExcludeDirs_DefaultsToTheSpecList pins the absent-key case. An existing
// corpus must not change.
func TestExcludeDirs_DefaultsToTheSpecList(t *testing.T) {
	want := []string{".dir2mcp", ".git", ".venv", "__pycache__", "build", "dist", "node_modules", "vendor"}
	if !reflect.DeepEqual(config.Default().IngestExcludeDirs, want) {
		t.Errorf("the default list must be the eight SPEC §7.1 names; got %v", config.Default().IngestExcludeDirs)
	}
	if !reflect.DeepEqual(corpusfs.DefaultExcludedDirs(), want) {
		t.Errorf("config and corpusfs must share one default list; got %v", corpusfs.DefaultExcludedDirs())
	}

	cfg, err := writeExcludeDirsConfig(t, "root_dir: .\n")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !reflect.DeepEqual(cfg.IngestExcludeDirs, want) {
		t.Errorf("an absent key must keep the default list; got %v", cfg.IngestExcludeDirs)
	}
}

// TestExcludeDirs_NestedKeyReplacesTheDefaultList covers the spec spelling
// (`ingest: exclude_dirs:`) and the replace semantics: the key does not add to
// the default list.
func TestExcludeDirs_NestedKeyReplacesTheDefaultList(t *testing.T) {
	cfg, err := writeExcludeDirsConfig(t, "ingest:\n  exclude_dirs:\n    - .git\n    - .dir2mcp\n    - notes\n")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := []string{".git", ".dir2mcp", "notes"}
	if !reflect.DeepEqual(cfg.IngestExcludeDirs, want) {
		t.Errorf("a present key must replace the default list in full; got %v", cfg.IngestExcludeDirs)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("a list that names .dir2mcp must load without a warning; got %v", cfg.Warnings)
	}
}

// TestExcludeDirs_InlineListForm covers the §16.2 template spelling.
func TestExcludeDirs_InlineListForm(t *testing.T) {
	cfg, err := writeExcludeDirsConfig(t, "ingest:\n  exclude_dirs: [\".git\", \".dir2mcp\", \"node_modules\"]\n")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := []string{".git", ".dir2mcp", "node_modules"}
	if !reflect.DeepEqual(cfg.IngestExcludeDirs, want) {
		t.Errorf("the inline list form must be read; got %v", cfg.IngestExcludeDirs)
	}
}

// TestExcludeDirs_StateDirIsAddedBackAndSaidSo pins the one name an operator
// cannot drop, and the SHOULD: the loader says that it added the name back.
func TestExcludeDirs_StateDirIsAddedBackAndSaidSo(t *testing.T) {
	cfg, err := writeExcludeDirsConfig(t, "ingest:\n  exclude_dirs:\n    - notes\n")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !reflect.DeepEqual(cfg.IngestExcludeDirs, []string{"notes", ".dir2mcp"}) {
		t.Errorf(".dir2mcp must be added back to the resolved list; got %v", cfg.IngestExcludeDirs)
	}
	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w.Error(), "exclude_dirs") && strings.Contains(w.Error(), ".dir2mcp") {
			found = true
		}
	}
	if !found {
		t.Errorf("the loader must say that it added .dir2mcp back; got %v", cfg.Warnings)
	}
}

// TestExcludeDirs_EmptyListKeepsOnlyTheStateDir covers `exclude_dirs: []`. It
// is a present key, so it replaces the default list with nothing.
func TestExcludeDirs_EmptyListKeepsOnlyTheStateDir(t *testing.T) {
	cfg, err := writeExcludeDirsConfig(t, "ingest:\n  exclude_dirs: []\n")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !reflect.DeepEqual(cfg.IngestExcludeDirs, []string{".dir2mcp"}) {
		t.Errorf("an empty list must resolve to the forced state directory only; got %v", cfg.IngestExcludeDirs)
	}
}

// TestExcludeDirs_RejectsAnEntryThatIsNotAName tells the operator at once that
// a path or a glob matches nothing, instead of leaving them to wonder why the
// directory is still indexed. SPEC §7.1: an entry is a plain directory name.
func TestExcludeDirs_RejectsAnEntryThatIsNotAName(t *testing.T) {
	for _, entry := range []string{"src/dist", "dist/**", "dist*", ".."} {
		cfg := config.Default()
		cfg.IngestExcludeDirs = []string{entry}
		if err := cfg.Validate(); err == nil {
			t.Errorf("entry %q must be rejected: it is not a plain directory name", entry)
		}
	}
	cfg := config.Default()
	cfg.IngestExcludeDirs = []string{".dir2mcp", "notes", "my dir"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a plain directory name must be accepted; got %v", err)
	}
}
