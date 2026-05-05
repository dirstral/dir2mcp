package tests

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoSplitBoundary_CmdDirectories(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatalf("read cmd/: %v", err)
	}

	allowed := map[string]struct{}{
		"dir2mcp":            {},
		"elevenlabs-bridge": {},
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, ok := allowed[entry.Name()]; !ok {
			t.Fatalf("unexpected command directory %q under cmd/; client/orchestrator binaries must live in separate repos", entry.Name())
		}
	}
}

func TestRepoSplitBoundary_NoDirstralCLIImports(t *testing.T) {
	root := repoRoot(t)

	var forbiddenHits []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == ".dir2mcp" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == filepath.ToSlash("tests/security/repo_split_boundary_test.go") {
			return nil
		}

		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(raw)
		if strings.Contains(text, "github.com/dirstral/dirstral-cli") ||
			strings.Contains(text, "github.com/Dirstral/dirstral-cli") {
			forbiddenHits = append(forbiddenHits, rel)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repository: %v", walkErr)
	}
	if len(forbiddenHits) > 0 {
		t.Fatalf("found forbidden dirstral-cli imports in dir2mcp repo: %v", forbiddenHits)
	}
}
