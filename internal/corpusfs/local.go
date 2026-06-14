package corpusfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ErrPathEscapesRoot is returned by Open/Localize when a relPath resolves
// outside the corpus root (a traversal in a stored ref). Callers can treat it as
// a permanent, non-retryable failure: re-running with the same ref cannot
// succeed. It is a sentinel so consumers (e.g. the embedding worker) can map it
// to their own fatal-error classification without string matching.
var ErrPathEscapesRoot = errors.New("corpusfs: path escapes the corpus root")

// LocalFS is the default CorpusFS backed by the local filesystem. An NFS mount
// is a local path, so it is served by LocalFS as well. It is behavior-preserving
// relative to the historical ingest discovery walker.
type LocalFS struct {
	root string
}

// NewLocalFS returns a LocalFS rooted at root.
func NewLocalFS(root string) *LocalFS {
	return &LocalFS{root: root}
}

// Walk enumerates regular files under the receiver's root. The root argument is
// accepted for CorpusFS interface compatibility but ignored: the receiver's
// configured root is authoritative, so Walk, Open, and Localize always resolve
// against the same corpus root (callers construct a LocalFS rooted where they
// intend to walk).
func (l *LocalFS) Walk(ctx context.Context, _ string, opts Options) ([]DiscoveredFile, error) {
	root := l.root
	if opts.MaxSizeBytes <= 0 {
		opts.MaxSizeBytes = defaultMaxFileSizeBytes
	}

	absRoot, err := filepath.Abs(root)
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
		options:      opts,
		files:        &files,
		visitedDirs:  map[string]struct{}{rootResolved: {}},
	}
	if err := walker.walkDir(ctx, absRoot, "", nil); err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, nil
}

// Open returns a seekable reader over the in-root file at relPath. The path is
// resolved with the same EvalSymlinks-within-root containment check media reads
// use, so a stored ref cannot escape the corpus root.
func (l *LocalFS) Open(ctx context.Context, relPath string) (io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolved, err := resolveWithinRoot(l.root, relPath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(resolved) //nolint:gosec // path is contained to root by resolveWithinRoot
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Localize returns the resolved in-root path and a no-op cleanup: a local file
// is already a real filesystem path that ffmpeg/archive extraction can read.
func (l *LocalFS) Localize(ctx context.Context, relPath string) (string, func(), error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	resolved, err := resolveWithinRoot(l.root, relPath)
	if err != nil {
		return "", nil, err
	}
	return resolved, func() {}, nil
}

// resolveWithinRoot resolves relPath against root and enforces that the result
// stays within root, resolving symlinks on both so a symlink inside the corpus
// that points outside it cannot smuggle out-of-root bytes (a lexical Join+prefix
// check alone would miss that). It is the shared containment helper for Open and
// Localize, extracted from the former loadMediaInput logic.
func resolveWithinRoot(root, relPath string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("corpusfs: empty corpus root")
	}
	ref := strings.TrimSpace(relPath)
	if ref == "" {
		return "", fmt.Errorf("corpusfs: empty rel path")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		realRoot = absRoot // root should exist; fall back to the lexical form
	}
	// Lexical containment check first, so a ref that escapes the root returns
	// the ErrPathEscapesRoot sentinel even when the (out-of-root) target does
	// not exist — otherwise EvalSymlinks would fail first with a generic
	// resolve error, weakening the sentinel contract for invalid refs.
	lexical := filepath.Join(realRoot, filepath.FromSlash(ref))
	if lexical != realRoot && !strings.HasPrefix(lexical, realRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscapesRoot, relPath)
	}
	resolved, err := filepath.EvalSymlinks(lexical)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", relPath, err)
	}
	if resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscapesRoot, relPath)
	}
	return resolved, nil
}

func shouldSkipDirectory(name string) bool {
	_, ok := defaultExcludedDirs[strings.TrimSpace(name)]
	return ok
}

type discoverWalker struct {
	rootAbs      string
	rootResolved string
	options      Options
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

// loadEffectiveRules returns the gitignore rules to apply in absDir.  When
// UseGitIgnore is false the parent rules are returned unchanged.
func loadEffectiveRules(absDir, relDir string, parentRules []gitIgnoreRule, useGitIgnore bool) ([]gitIgnoreRule, error) {
	if !useGitIgnore {
		return parentRules, nil
	}
	rules := append([]gitIgnoreRule(nil), parentRules...)
	localRules, err := parseGitIgnoreRules(absDir, relDir)
	if err != nil {
		return nil, err
	}
	return append(rules, localRules...), nil
}

// shouldAddFile reports whether a regular file should be included in the
// discovered file set.
func (w *discoverWalker) shouldAddFile(lstat os.FileInfo, relPath string, rules []gitIgnoreRule) bool {
	if !lstat.Mode().IsRegular() {
		return false
	}
	if w.options.UseGitIgnore && matchesGitIgnoreRules(rules, relPath, false) {
		return false
	}
	return lstat.Size() <= w.options.MaxSizeBytes
}

// visitDir processes a single directory entry that is known to be a directory.
func (w *discoverWalker) visitDir(ctx context.Context, fullPath, relPath, name string, rules []gitIgnoreRule) error {
	if shouldSkipDirectory(name) {
		return nil
	}
	if w.options.UseGitIgnore && matchesGitIgnoreRules(rules, relPath, true) {
		return nil
	}
	nextDir := filepath.Clean(fullPath)
	if w.options.FollowSymlinks {
		if resolved, err := filepath.EvalSymlinks(nextDir); err == nil {
			nextDir = filepath.Clean(resolved)
		}
	}
	if _, ok := w.visitedDirs[nextDir]; ok {
		return nil
	}
	w.visitedDirs[nextDir] = struct{}{}
	return w.walkDir(ctx, nextDir, relPath, rules)
}

func (w *discoverWalker) walkDir(ctx context.Context, absDir, relDir string, parentRules []gitIgnoreRule) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	rules, err := loadEffectiveRules(absDir, relDir, parentRules, w.options.UseGitIgnore)
	if err != nil {
		return err
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
			if err := w.visitDir(ctx, fullPath, relPath, name, rules); err != nil {
				return err
			}
			continue
		}

		if !w.shouldAddFile(lstat, relPath, rules) {
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
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if filepath.IsAbs(rel) {
		return false
	}
	return true
}
