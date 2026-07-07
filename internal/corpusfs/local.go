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
	if lstat.Size() > w.options.MaxSizeBytes {
		w.reportOversize(relPath, lstat.Size())
		return false
	}
	return true
}

// reportOversize surfaces a size-cap exclusion to the caller-provided
// Options.OnOversize hook (issue #497). It is a no-op when no hook is set, so
// the default discovery behavior is byte-for-byte unchanged.
func (w *discoverWalker) reportOversize(relPath string, size int64) {
	if w.options.OnOversize != nil {
		w.options.OnOversize(relPath, size)
	}
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

	// Fast path: when a scan cache is configured and this directory's own mtime
	// is unchanged, its set of direct children is unchanged too (POSIX bumps a
	// directory's mtime on any add/remove/rename of a direct child). Validate the
	// cached children with cheap per-file stats — which still catches an in-place
	// file modification — and skip re-reading and re-sorting the directory. Any
	// inconsistency makes served become false so we fall through to a full read.
	if served, err := w.walkDirFromCache(ctx, absDir, relDir, rules); err != nil {
		return err
	} else if served {
		return nil
	}

	return w.walkDirFull(ctx, absDir, relDir, rules)
}

// walkDirFull performs the authoritative directory read: it lists, sorts, and
// processes every entry, and (when caching is enabled and the read is clean)
// persists a fresh signature for next time.
func (w *discoverWalker) walkDirFull(ctx context.Context, absDir, relDir string, rules []gitIgnoreRule) error {
	// Capture the directory mtime BEFORE reading its entries. Storing the
	// pre-read mtime (and re-confirming it is unchanged after the read) closes a
	// TOCTOU: if a child is added concurrently with the read, the mtime bumps and
	// we decline to cache a possibly-partial snapshot rather than risk recording a
	// new mtime against a stale child set (which a later run would wrongly trust).
	caching := w.options.ScanCache != nil && !w.options.FollowSymlinks
	var preReadMTime int64
	if caching {
		if info, statErr := os.Stat(absDir); statErr == nil {
			preReadMTime = info.ModTime().Unix()
		} else {
			caching = false
		}
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	// sigEntries accumulates the freshly observed children so the directory's
	// signature can be persisted for the next run. Only populated when caching is
	// enabled and the directory is fully readable (no per-entry error aborts).
	var sigEntries []CachedDirEntry

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		sigEntry, ok, err := w.processFullEntry(ctx, absDir, relDir, entry.Name(), rules)
		if err != nil {
			return err
		}
		if !ok {
			// A symlink child makes the cached signature ambiguous (resolution and
			// cycle-detection are stateful), so do not persist a signature for this
			// directory: the next run re-reads it fully.
			caching = false
			continue
		}
		if caching {
			sigEntries = append(sigEntries, sigEntry)
		}
	}

	if caching {
		w.storeDirSignature(absDir, relDir, preReadMTime, sigEntries)
	}
	return nil
}

// processFullEntry handles a single freshly-read child of a directory: it stats
// it, recurses into directories, appends regular files that pass policy, and
// follows symlinks per options. It returns the entry's cache signature plus
// cacheable=false when the child (a symlink) makes the directory ineligible for
// caching.
func (w *discoverWalker) processFullEntry(ctx context.Context, absDir, relDir, name string, rules []gitIgnoreRule) (CachedDirEntry, bool, error) {
	relPath := name
	if relDir != "" {
		relPath = relDir + "/" + name
	}
	fullPath := filepath.Join(absDir, name)

	lstat, err := os.Lstat(fullPath)
	if err != nil {
		return CachedDirEntry{}, false, err
	}

	if lstat.Mode()&os.ModeSymlink != 0 {
		if !w.options.FollowSymlinks {
			return CachedDirEntry{}, false, nil
		}
		if err := w.handleSymlink(ctx, fullPath, relPath, rules); err != nil {
			return CachedDirEntry{}, false, err
		}
		return CachedDirEntry{}, false, nil
	}

	if lstat.IsDir() {
		if err := w.visitDir(ctx, fullPath, relPath, name, rules); err != nil {
			return CachedDirEntry{}, false, err
		}
		return CachedDirEntry{Name: name, IsDir: true}, true, nil
	}

	sig := CachedDirEntry{
		Name:      name,
		SizeBytes: lstat.Size(),
		MTimeUnix: lstat.ModTime().Unix(),
		Mode:      uint32(lstat.Mode()),
	}
	if w.shouldAddFile(lstat, relPath, rules) {
		*w.files = append(*w.files, DiscoveredFile{
			AbsPath:   fullPath,
			RelPath:   relPath,
			SizeBytes: lstat.Size(),
			MTimeUnix: lstat.ModTime().Unix(),
			Mode:      lstat.Mode(),
		})
	}
	return sig, true, nil
}

// cachedChild pairs a validated cache entry with the live stat used to confirm
// it, so the emit pass can reuse the stat without re-calling Lstat.
type cachedChild struct {
	entry    CachedDirEntry
	relPath  string
	fullPath string
	lstat    os.FileInfo
}

// walkDirFromCache attempts to serve absDir from the scan cache. It returns
// served=true only when the cache holds a signature whose recorded directory
// mtime equals the live directory mtime AND every cached child still matches the
// live filesystem (a regular file's size+mtime unchanged, a directory still a
// directory). On a hit it appends the surviving files (applying the same
// gitignore/size policies) and recurses into child directories exactly as the
// full path would. served=false means the caller must perform a full read; the
// cache is never trusted without these confirmations, so a stale entry can only
// cost a re-walk, never a missed change.
func (w *discoverWalker) walkDirFromCache(ctx context.Context, absDir, relDir string, rules []gitIgnoreRule) (bool, error) {
	if w.options.ScanCache == nil || w.options.FollowSymlinks {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	info, err := os.Stat(absDir)
	if err != nil {
		return false, nil //nolint:nilerr // unreadable dir is a miss; full path reports the error
	}
	sig, ok, err := w.options.ScanCache.LookupDir(relDir)
	if err != nil || !ok || sig.DirMTimeUnix != info.ModTime().Unix() {
		return false, nil //nolint:nilerr // a cache error/miss/mtime drift is a full-read fallback
	}

	confirmed, ok, err := w.validateCachedChildren(ctx, absDir, relDir, sig.Entries)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if err := w.emitCachedChildren(ctx, confirmed, rules); err != nil {
		return false, err
	}
	return true, nil
}

// validateCachedChildren confirms every cached child still matches the live
// filesystem WITHOUT mutating walker state, so a late mismatch leaves nothing
// half-applied before the full-read fallback. ok=false means the directory must
// be re-read in full.
func (w *discoverWalker) validateCachedChildren(ctx context.Context, absDir, relDir string, entries []CachedDirEntry) ([]cachedChild, bool, error) {
	confirmed := make([]cachedChild, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		fullPath := filepath.Join(absDir, e.Name)
		lstat, statErr := os.Lstat(fullPath)
		if statErr != nil || lstat.Mode()&os.ModeSymlink != 0 {
			return nil, false, nil // child vanished/changed identity or is a symlink.
		}
		if !cachedChildMatches(e, lstat) {
			return nil, false, nil
		}
		relPath := e.Name
		if relDir != "" {
			relPath = relDir + "/" + e.Name
		}
		confirmed = append(confirmed, cachedChild{entry: e, relPath: relPath, fullPath: fullPath, lstat: lstat})
	}
	return confirmed, true, nil
}

// cachedChildMatches reports whether a live stat is consistent with the cached
// entry: a directory must still be a directory; a regular file must still be a
// regular file with the same size and mtime (so an in-place modification fails).
func cachedChildMatches(e CachedDirEntry, lstat os.FileInfo) bool {
	if e.IsDir {
		return lstat.IsDir()
	}
	if lstat.IsDir() || !lstat.Mode().IsRegular() {
		return false
	}
	return lstat.Size() == e.SizeBytes && lstat.ModTime().Unix() == e.MTimeUnix
}

// emitCachedChildren appends the confirmed files and recurses into confirmed
// subdirectories using the identical policy as the full path.
func (w *discoverWalker) emitCachedChildren(ctx context.Context, confirmed []cachedChild, rules []gitIgnoreRule) error {
	for _, p := range confirmed {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.entry.IsDir {
			if err := w.visitDir(ctx, p.fullPath, p.relPath, p.entry.Name, rules); err != nil {
				return err
			}
			continue
		}
		if !w.shouldAddFile(p.lstat, p.relPath, rules) {
			continue
		}
		*w.files = append(*w.files, DiscoveredFile{
			AbsPath:   p.fullPath,
			RelPath:   p.relPath,
			SizeBytes: p.lstat.Size(),
			MTimeUnix: p.lstat.ModTime().Unix(),
			Mode:      p.lstat.Mode(),
		})
	}
	return nil
}

// storeDirSignature persists the freshly observed directory signature, but only
// if the directory's mtime has not changed since it was sampled before the read
// (preReadMTime). A changed mtime means a child was added/removed/renamed during
// the read, so the captured child set may be partial; in that case we skip the
// store rather than record a new mtime against a stale set (which a later run
// would wrongly trust). Errors are non-fatal: discovery already produced correct
// results; a failed store only forgoes the optimization next run.
func (w *discoverWalker) storeDirSignature(absDir, relDir string, preReadMTime int64, entries []CachedDirEntry) {
	info, err := os.Stat(absDir)
	if err != nil || info.ModTime().Unix() != preReadMTime {
		return
	}
	_ = w.options.ScanCache.StoreDir(relDir, CachedDirSignature{
		DirMTimeUnix: preReadMTime,
		Entries:      entries,
	})
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
		w.reportOversize(relPath, stat.Size())
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
	return parseGitIgnoreContent(content, relDir), nil
}

// parseGitIgnoreContent parses the raw bytes of a .gitignore file into rules
// anchored at relDir (the corpus-relative directory the .gitignore lives in). It
// is the backend-agnostic core of gitignore parsing so both the local walker
// (reading files) and the S3 walker (reading objects) apply identical semantics.
func parseGitIgnoreContent(content []byte, relDir string) []gitIgnoreRule {
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

	return rules
}

// keyIgnoredByGitignore reports whether an object key (a corpus-relative slash
// path to a regular file) is excluded by the gitignore rules. It mirrors the
// local walker, which prunes an ignored directory before descending: a key is
// ignored if any ancestor directory segment is ignored (a directory match, which
// git treats as un-re-includable) or the file path itself matches. rules must
// already be ordered root-first so gitignore's last-match-wins precedence holds.
func keyIgnoredByGitignore(rel string, rules []gitIgnoreRule) bool {
	segs := strings.Split(rel, "/")
	for i := 1; i < len(segs); i++ {
		if matchesGitIgnoreRules(rules, strings.Join(segs[:i], "/"), true) {
			return true
		}
	}
	return matchesGitIgnoreRules(rules, rel, false)
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
