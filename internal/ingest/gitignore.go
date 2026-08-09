package ingest

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// defaultMaxFileSizeBytes mirrors corpusfs.DefaultMaxFileSizeBytes() as a
// package constant for representation generators that compare against it
// directly.
const defaultMaxFileSizeBytes int64 = 10 * 1024 * 1024

// shouldSkipDirectory reports whether the incremental file watch skips a
// directory. The set comes from the same DiscoverOptions the initial scan uses
// (DiscoverOptionsFromConfig), so the watcher applies the operator's
// `ingest.exclude_dirs` list and cannot hold a second copy of it: the watcher
// once kept its own map and the two drifted apart, so a directory could be
// excluded from the scan and still be picked up by the watcher (or the reverse).
// See #716 and #773.
func shouldSkipDirectory(excluded corpusfs.ExcludedDirSet, name string) bool {
	return excluded.Has(name)
}

type gitIgnoreRule struct {
	baseRel  string
	pattern  string
	negated  bool
	dirOnly  bool
	anchored bool
}

func parseGitIgnoreRules(absDir, relDir string) ([]gitIgnoreRule, error) {
	gitIgnorePath := filepath.Join(absDir, ".gitignore")
	content, err := os.ReadFile(gitIgnorePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", gitIgnorePath, err)
	}

	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(string(content), "\r\n", "\n"), "\r", "\n"), "\n")
	rules := make([]gitIgnoreRule, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		negated := false
		if strings.HasPrefix(line, "!") {
			negated = true
			line = strings.TrimSpace(strings.TrimPrefix(line, "!"))
		}
		if line == "" {
			continue
		}

		dirOnly := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}

		anchored := strings.HasPrefix(line, "/") || strings.Contains(line, "/")
		line = strings.TrimPrefix(filepath.ToSlash(line), "/")

		rules = append(rules, gitIgnoreRule{
			baseRel:  relDir,
			pattern:  line,
			negated:  negated,
			dirOnly:  dirOnly,
			anchored: anchored,
		})
	}

	return rules, nil
}

func matchesGitIgnoreRules(rules []gitIgnoreRule, relPath string, isDir bool) bool {
	relPath = strings.TrimPrefix(filepath.ToSlash(relPath), "./")
	relPath = strings.TrimPrefix(relPath, "/")
	if relPath == "" {
		return false
	}

	ignored := false
	for _, rule := range rules {
		if rule.dirOnly && !isDir {
			continue
		}
		targetPath, ok := trimRelPathToBase(relPath, rule.baseRel)
		if !ok {
			continue
		}
		if matchGitIgnoreRule(rule, targetPath) {
			ignored = !rule.negated
		}
	}
	return ignored
}

func trimRelPathToBase(relPath, baseRel string) (string, bool) {
	baseRel = strings.TrimPrefix(filepath.ToSlash(baseRel), "./")
	baseRel = strings.TrimPrefix(baseRel, "/")
	if baseRel == "" {
		return relPath, true
	}
	if relPath == baseRel {
		return ".", true
	}
	prefix := baseRel + "/"
	if !strings.HasPrefix(relPath, prefix) {
		return "", false
	}
	return strings.TrimPrefix(relPath, prefix), true
}

func matchGitIgnoreRule(rule gitIgnoreRule, relPath string) bool {
	relPath = strings.TrimPrefix(relPath, "./")
	relPath = strings.TrimPrefix(relPath, "/")
	if relPath == "" {
		return false
	}

	if rule.anchored {
		return matchPathExclude(rule.pattern, relPath)
	}

	baseName := path.Base(relPath)
	matched, err := path.Match(rule.pattern, baseName)
	if err != nil {
		return false
	}
	if matched {
		return true
	}
	return matchPathExclude("**/"+rule.pattern, relPath)
}
