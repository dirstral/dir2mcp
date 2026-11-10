package ingest

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/model"
)

// This file implements dir2mcp_recheck_path (SPEC §15.13): a client asks the
// running file watcher to reconcile one path under the corpus root now, instead
// of at the next periodic safety rescan.
//
// The motivating case is an NFS corpus written by another host. inotify
// reports no event for such a write, so the file waits for the safety rescan,
// which stats every file under the root. A system that knows which path
// changed can ask for that one path.

// RecheckKind* are the values of RecheckResult.Kind (recheck_path.json).
const (
	RecheckKindFile      = "file"
	RecheckKindDirectory = "directory"
	RecheckKindMissing   = "missing"
)

// RecheckRoot is the normalized rel_path that names the corpus root itself.
const RecheckRoot = "."

// ErrWatchNotRunning is returned by Service.RecheckPath when no file watcher
// loop is running: before Watch started, or after it returned.
var ErrWatchNotRunning = errors.New("file watcher is not running")

// ErrRecheckPathRequired is returned for an empty rel_path.
var ErrRecheckPathRequired = errors.New("rel_path is required")

// RecheckResult is the §15.13 response: the normalized path, what one stat
// found at call time, whether this call added a request, and the number of
// requests pending after the call.
type RecheckResult struct {
	RelPath string
	Kind    string
	Queued  bool
	Pending int
}

// PathRechecker enqueues a reconcile of one path under the corpus root. The
// MCP server holds one when the daemon runs the file watcher and is not
// read-only; dir2mcp_recheck_path is served only then.
type PathRechecker interface {
	RecheckPath(ctx context.Context, relPath string, recursive bool) (RecheckResult, error)
}

// RecheckPath validates, classifies and enqueues a reconcile of relPath on the
// running watch loop (SPEC §15.13). It returns at once; the watcher's worker
// performs the reconcile. ErrWatchNotRunning says no loop is running.
func (s *Service) RecheckPath(ctx context.Context, relPath string, recursive bool) (RecheckResult, error) {
	s.recheckMu.RLock()
	loop := s.recheckLoop
	s.recheckMu.RUnlock()
	if loop == nil {
		return RecheckResult{}, ErrWatchNotRunning
	}
	return loop.recheckPath(ctx, relPath, recursive)
}

// setRecheckLoop publishes (or, with nil, withdraws) the live watch loop that
// RecheckPath forwards to. run() installs it before the worker starts and clears
// it on exit, so a request can never reach a loop whose worker is gone.
func (s *Service) setRecheckLoop(loop *fsWatchLoop) {
	s.recheckMu.Lock()
	defer s.recheckMu.Unlock()
	s.recheckLoop = loop
}

// recheckRequest is one pending reconcile request. The queue holds at most one
// per normalized rel_path.
type recheckRequest struct {
	rel       string
	recursive bool
}

// recheckQueue is the §15.13 request queue: a FIFO with at most one entry per
// normalized path. A repeat for a pending path adds nothing; a recursive repeat
// upgrades a pending non-recursive request. A request leaves the queue when the
// worker starts its reconcile.
type recheckQueue struct {
	mu      sync.Mutex
	pending map[string]*recheckRequest
	order   []string
	// signal wakes the worker. Capacity one: a burst of requests collapses to
	// one wake-up, and the worker re-signals itself while entries remain.
	signal chan struct{}
}

func newRecheckQueue() *recheckQueue {
	return &recheckQueue{
		pending: make(map[string]*recheckRequest),
		signal:  make(chan struct{}, 1),
	}
}

// add enqueues rel and reports whether this call added an entry, plus the
// number of entries pending after the call.
func (q *recheckQueue) add(rel string, recursive bool) (queued bool, pending int) {
	q.mu.Lock()
	if existing, ok := q.pending[rel]; ok {
		if recursive && !existing.recursive {
			existing.recursive = true
		}
		pending = len(q.pending)
		q.mu.Unlock()
		return false, pending
	}
	q.pending[rel] = &recheckRequest{rel: rel, recursive: recursive}
	q.order = append(q.order, rel)
	pending = len(q.pending)
	q.mu.Unlock()
	q.wake()
	return true, pending
}

// pop removes and returns the oldest request. ok=false means the queue is empty.
func (q *recheckQueue) pop() (recheckRequest, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) == 0 {
		return recheckRequest{}, false
	}
	rel := q.order[0]
	q.order = q.order[1:]
	req := q.pending[rel]
	delete(q.pending, rel)
	if req == nil {
		return recheckRequest{}, false
	}
	return *req, true
}

// size returns the number of pending requests.
func (q *recheckQueue) size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// wake signals the worker without blocking.
func (q *recheckQueue) wake() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// recheckPath is the call-time half of §15.13: normalize, resolve under the
// root with the §17 rules, stat once for kind, enqueue, return. It walks no
// subtree and reads no document.
func (w *fsWatchLoop) recheckPath(_ context.Context, relPath string, recursive bool) (RecheckResult, error) {
	rel, err := normalizeRecheckPath(relPath)
	if err != nil {
		return RecheckResult{}, err
	}
	kind, err := w.classifyRecheckPath(rel)
	if err != nil {
		return RecheckResult{}, err
	}
	queued, pending := w.rechecks.add(rel, recursive)
	w.svc.getLogger().Printf("watch: recheck %q (%s, recursive=%v): queued=%v, %d request(s) pending",
		rel, kind, recursive, queued, pending)
	return RecheckResult{RelPath: rel, Kind: kind, Queued: queued, Pending: pending}, nil
}

// normalizeRecheckPath applies the rel_path grammar of dir2mcp_open_file
// (SPEC §15.4): `/` as the separator, no leading `/`, no `..` segment. Unlike
// open_file, `.` is accepted and names the root (§15.13).
func normalizeRecheckPath(relPath string) (string, error) {
	trimmed := strings.TrimSpace(relPath)
	if trimmed == "" {
		return "", ErrRecheckPathRequired
	}
	normalized := filepath.ToSlash(filepath.Clean(trimmed))
	if filepath.IsAbs(trimmed) || strings.HasPrefix(normalized, "/") || normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", model.ErrPathOutsideRoot
	}
	return normalized, nil
}

// recheckAbsPath maps a normalized rel_path to the path under the watched root.
func (w *fsWatchLoop) recheckAbsPath(rel string) string {
	if rel == RecheckRoot {
		return w.absRoot
	}
	return filepath.Join(w.absRoot, filepath.FromSlash(rel))
}

// classifyRecheckPath enforces §17 root isolation on the resolved path and
// stats it once. A path that exists must resolve inside the root through every
// symlink on the way; a path that does not exist is classified `missing` once
// its nearest existing ancestor resolves inside the root.
func (w *fsWatchLoop) classifyRecheckPath(rel string) (string, error) {
	abs := w.recheckAbsPath(rel)
	rootResolved := corpusfs.ResolveRoot(w.absRoot)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := w.ancestorWithinRoot(rootResolved, abs); err != nil {
			return "", err
		}
		return RecheckKindMissing, nil
	}
	if !pathWithinRoot(rootResolved, filepath.Clean(resolved)) {
		return "", model.ErrPathOutsideRoot
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RecheckKindMissing, nil
		}
		return "", err
	}
	if info.IsDir() {
		return RecheckKindDirectory, nil
	}
	return RecheckKindFile, nil
}

// ancestorWithinRoot walks up from a path that does not exist to its nearest
// existing ancestor and checks that the ancestor resolves inside the root, so a
// symlinked directory that points outside the corpus cannot carry a request
// for a not-yet-existing child past the root.
func (w *fsWatchLoop) ancestorWithinRoot(rootResolved, abs string) error {
	dir := abs
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return model.ErrPathOutsideRoot
		}
		dir = parent
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if !pathWithinRoot(rootResolved, filepath.Clean(resolved)) {
			return model.ErrPathOutsideRoot
		}
		return nil
	}
}

// pathWithinRoot reports whether resolved is rootResolved or lies below it.
func pathWithinRoot(rootResolved, resolved string) bool {
	rel, err := filepath.Rel(rootResolved, resolved)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// processRecheck is the worker-side half of §15.13. It classifies the path
// again, because the path can change between the call and the reconcile, and
// applies the rule for what it finds now:
//
//   - a file: the single-path reconcile the watcher runs for an event
//   - a directory: every regular file under it (direct children only when the
//     request is not recursive), then a tombstone for each document indexed
//     under it whose file is gone
//   - a missing path: the tombstone pass the watcher runs for a vanished path
//
// The exclusion engine is applied by process, exactly as for an event, so an
// excluded path is reconciled as ineligible.
func (w *fsWatchLoop) processRecheck(ctx context.Context, req recheckRequest) {
	abs := w.recheckAbsPath(req.rel)
	// Lstat, not Stat: a symlink is judged by the symlink policy below, so a
	// link repointed outside the corpus between the call and the reconcile is
	// never walked. The root itself may be reached through a link, and the
	// scan resolves it the same way, so the root is the one path Stat'ed.
	stat := os.Lstat
	if req.rel == RecheckRoot {
		stat = os.Stat
	}
	info, err := stat(abs)
	switch {
	case err == nil && (info.IsDir() || w.linkedDirectoryWithinRoot(abs, info)):
		w.recheckDirectory(ctx, req.rel, abs, req.recursive)
	case req.rel == RecheckRoot:
		// The root itself is gone or unreadable. A full tombstone of the corpus
		// on one request is not a reconcile; the operator has to act. Say so.
		w.svc.getLogger().Printf("watch: recheck of the root: %s is not a readable directory (%v); nothing reconciled", abs, err)
	default:
		// A file, another non-directory entry, or a path that is gone: the
		// per-path reconcile handles each of them as it handles an event.
		w.process(ctx, watchJob{absPath: abs})
	}
}

// linkedDirectoryWithinRoot reports whether abs is a symlink to a directory
// that the symlink policy allows the watcher to enter: following is on and the
// target resolves inside the corpus root (the shared §17 rule). With following
// off, or for a target outside the root, the link is handed to process, which
// records the refusal exactly as it does for a symlink event.
func (w *fsWatchLoop) linkedDirectoryWithinRoot(abs string, info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink == 0 || !w.opts.FollowSymlinks {
		return false
	}
	resolved, ok := corpusfs.ResolveSymlinkWithinRoot(corpusfs.ResolveRoot(w.absRoot), abs)
	if !ok {
		return false
	}
	target, err := os.Stat(resolved)
	return err == nil && target.IsDir()
}

// recheckDirectory reconciles the regular files under absDir and tombstones
// the documents indexed under rel whose files are gone.
func (w *fsWatchLoop) recheckDirectory(ctx context.Context, rel, absDir string, recursive bool) {
	w.walkRecheckFiles(absDir, recursive, func(absPath string) {
		if ctx.Err() != nil {
			return
		}
		w.process(ctx, watchJob{absPath: absPath})
	})
	if ctx.Err() != nil {
		return
	}
	w.tombstoneGoneUnder(ctx, rel, recursive)
}

// walkRecheckFiles calls onFile for every regular file under absDir. A
// recursive walk is the walk registerNewTree performs (it honours the directory
// ignore list and the symlink policy); a non-recursive walk reads the direct
// children only.
func (w *fsWatchLoop) walkRecheckFiles(absDir string, recursive bool, onFile func(string)) {
	if recursive {
		w.walkTree(absDir, func(string) {}, onFile)
		return
	}
	entries, err := os.ReadDir(absDir)
	if err != nil {
		w.svc.getLogger().Printf("watch: recheck read %s: %v", absDir, err)
		return
	}
	for _, entry := range entries {
		child := filepath.Join(absDir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			// process applies the symlink policy; hand it the link.
			if target, statErr := os.Stat(child); statErr == nil && target.Mode().IsRegular() {
				onFile(child)
			}
			continue
		}
		if entry.Type().IsRegular() {
			onFile(child)
		}
	}
}

// tombstoneGoneUnder retires every active document stored under relDir whose
// file no longer exists. With recursive=false only the direct children are
// candidates. Unlike processDeleteSubtree, the directory itself still exists,
// so an archive member ("book.zip/chapter.md", a path that never exists on
// disk) is kept when its archive is still there.
func (w *fsWatchLoop) tombstoneGoneUnder(ctx context.Context, relDir string, recursive bool) {
	deleter, ok := w.svc.store.(documentDeleteMarker)
	if !ok {
		return
	}
	paths, err := w.svc.activeDocumentsUnder(ctx, relDir)
	if err != nil {
		if ctx.Err() == nil {
			w.svc.getLogger().Printf("watch: list documents under %s: %v", relDir, err)
		}
		return
	}
	deleted := make([]string, 0)
	for _, relPath := range paths {
		if ctx.Err() != nil {
			break
		}
		if !recursive && !isDirectChild(relDir, relPath) {
			continue
		}
		if entryBackedOnDisk(w.absRoot, relPath) {
			continue
		}
		if err := deleter.MarkDocumentDeleted(ctx, relPath); err != nil {
			w.svc.addErrors(1)
			continue
		}
		w.svc.addDeleted(1)
		deleted = append(deleted, relPath)
	}
	w.svc.notifyDocumentsDeleted(deleted)
}

// isDirectChild reports whether relPath sits directly in relDir.
func isDirectChild(relDir, relPath string) bool {
	return path.Dir(relPath) == relDir
}

// entryBackedOnDisk reports whether relPath, or the nearest ancestor of it that
// exists, is a non-directory entry on disk. A document is backed when its file
// exists, and an archive member is backed when its archive exists. A path whose
// nearest existing ancestor is a directory is gone.
func entryBackedOnDisk(absRoot, relPath string) bool {
	for p := relPath; p != "" && p != RecheckRoot; p = path.Dir(p) {
		info, err := os.Lstat(filepath.Join(absRoot, filepath.FromSlash(p)))
		if err != nil {
			continue
		}
		return !info.IsDir()
	}
	return false
}
