package ingest

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const defaultMaxFileSizeBytes int64 = 10 * 1024 * 1024

// DefaultMaxFileSizeBytes returns the default ingest file-size cap.
func DefaultMaxFileSizeBytes() int64 {
	return defaultMaxFileSizeBytes
}

var defaultExcludedDirs = map[string]struct{}{
	".git":         {},
	".dir2mcp":     {},
	"node_modules": {},
	"vendor":       {},
	"__pycache__":  {},
}

// DiscoveredFile holds metadata collected during file system discovery.
type DiscoveredFile struct {
	AbsPath   string
	RelPath   string
	SizeBytes int64
	MTimeUnix int64
	Mode      os.FileMode
}

// DiscoverOptions controls optional discovery behavior.
type DiscoverOptions struct {
	MaxSizeBytes   int64
	UseGitIgnore   bool
	FollowSymlinks bool
}

// DefaultDiscoverOptions returns discovery defaults used by ingestion.
func DefaultDiscoverOptions() DiscoverOptions {
	return DiscoverOptions{
		MaxSizeBytes:   defaultMaxFileSizeBytes,
		UseGitIgnore:   false,
		FollowSymlinks: false,
	}
}

// DiscoverFiles walks rootDir and returns regular files that pass default
// discovery policies (skip symlinks, known heavy dirs, and over-limit files).
func DiscoverFiles(ctx context.Context, rootDir string, maxSizeBytes int64) ([]DiscoveredFile, error) {
	options := DefaultDiscoverOptions()
	options.MaxSizeBytes = maxSizeBytes
	return DiscoverFilesWithOptions(ctx, rootDir, options)
}

// DiscoverFilesWithOptions walks rootDir and returns regular files that match
// discovery policies and caller-provided options.
func DiscoverFilesWithOptions(ctx context.Context, rootDir string, options DiscoverOptions) ([]DiscoveredFile, error) {
	if options.MaxSizeBytes <= 0 {
		options.MaxSizeBytes = defaultMaxFileSizeBytes
	}

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	rootInfo, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("stat root: %w", err)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("root is not a directory: %s", absRoot)
	}

	rootResolved := absRoot
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		rootResolved = resolved
	}
	rootResolved = filepath.Clean(rootResolved)

	files := make([]DiscoveredFile, 0, 256)
	walker := discoverWalker{
		rootAbs:      absRoot,
		rootResolved: rootResolved,
		options:      options,
		files:        &files,
		visitedDirs:  map[string]struct{}{rootResolved: {}},
	}
	err = walker.walkDir(ctx, absRoot, "", nil)
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, nil
}

func shouldSkipDirectory(name string) bool {
	_, ok := defaultExcludedDirs[strings.TrimSpace(name)]
	return ok
}

type discoverWalker struct {
	rootAbs      string
	rootResolved string
	options      DiscoverOptions
	files        *[]DiscoveredFile
	visitedDirs  map[string]struct{}
}

type gitIgnoreRule struct {
	baseRel  string
	pattern  string
	negated  bool
	dirOnly  bool
	anchored bool
}

func (w *discoverWalker) walkDir(ctx context.Context, absDir, relDir string, parentRules []gitIgnoreRule) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	rules := parentRules
	if w.options.UseGitIgnore {
		rules = append([]gitIgnoreRule(nil), parentRules...)
		localRules, err := parseGitIgnoreRules(absDir, relDir)
		if err != nil {
			return err
		}
		rules = append(rules, localRules...)
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}

		name := entry.Name()
		relPath := name
		if relDir != "" {
			relPath = relDir + "/" + name
		}
		fullPath := filepath.Join(absDir, name)

		lstat, err := os.Lstat(fullPath)
		if err != nil {
			return err
		}

		if lstat.Mode()&os.ModeSymlink != 0 {
			if !w.options.FollowSymlinks {
				continue
			}
			if err := w.handleSymlink(ctx, fullPath, relPath, rules); err != nil {
				return err
			}
			continue
		}

		if lstat.IsDir() {
			if shouldSkipDirectory(name) {
				continue
			}
			if w.options.UseGitIgnore && matchesGitIgnoreRules(rules, relPath, true) {
				continue
			}
			nextDir := filepath.Clean(fullPath)
			if w.options.FollowSymlinks {
				if resolved, err := filepath.EvalSymlinks(nextDir); err == nil {
					nextDir = filepath.Clean(resolved)
				}
			}
			if _, ok := w.visitedDirs[nextDir]; ok {
				continue
			}
			w.visitedDirs[nextDir] = struct{}{}
			if err := w.walkDir(ctx, nextDir, relPath, rules); err != nil {
				return err
			}
			continue
		}

		if !lstat.Mode().IsRegular() {
			continue
		}
		if w.options.UseGitIgnore && matchesGitIgnoreRules(rules, relPath, false) {
			continue
		}
		if lstat.Size() > w.options.MaxSizeBytes {
			continue
		}
		*w.files = append(*w.files, DiscoveredFile{
			AbsPath:   fullPath,
			RelPath:   relPath,
			SizeBytes: lstat.Size(),
			MTimeUnix: lstat.ModTime().Unix(),
			Mode:      lstat.Mode(),
		})
	}

	return nil
}

func (w *discoverWalker) handleSymlink(ctx context.Context, symlinkPath, relPath string, rules []gitIgnoreRule) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	resolvedPath, err := filepath.EvalSymlinks(symlinkPath)
	if err != nil {
		return nil
	}
	resolvedPath = filepath.Clean(resolvedPath)
	if !isWithinRoot(w.rootResolved, resolvedPath) {
		return nil
	}

	stat, err := os.Stat(symlinkPath)
	if err != nil {
		return nil
	}

	if stat.IsDir() {
		if shouldSkipDirectory(path.Base(relPath)) {
			return nil
		}
		if w.options.UseGitIgnore && matchesGitIgnoreRules(rules, relPath, true) {
			return nil
		}
		if _, ok := w.visitedDirs[resolvedPath]; ok {
			return nil
		}
		w.visitedDirs[resolvedPath] = struct{}{}
		return w.walkDir(ctx, resolvedPath, relPath, rules)
	}

	if !stat.Mode().IsRegular() {
		return nil
	}
	if w.options.UseGitIgnore && matchesGitIgnoreRules(rules, relPath, false) {
		return nil
	}
	if stat.Size() > w.options.MaxSizeBytes {
		return nil
	}

	*w.files = append(*w.files, DiscoveredFile{
		AbsPath:   resolvedPath,
		RelPath:   relPath,
		SizeBytes: stat.Size(),
		MTimeUnix: stat.ModTime().Unix(),
		Mode:      stat.Mode(),
	})
	return nil
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

func isWithinRoot(rootResolved, candidate string) bool {
	rootResolved = filepath.Clean(rootResolved)
	candidate = filepath.Clean(candidate)

	rel, err := filepath.Rel(rootResolved, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	if filepath.IsAbs(rel) {
		return false
	}
	return true
}
