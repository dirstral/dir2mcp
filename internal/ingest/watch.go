package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// defaultWatchDebounce is used when the configured debounce is non-positive.
const defaultWatchDebounce = 500 * time.Millisecond

// safetyRescanInterval bounds how stale the index can get if the watcher
// misses events (kernel event coalescing, inotify/kqueue limits on large
// trees). A low-frequency full rescan reuses the existing hash-diffing scan,
// so it is cheap when nothing changed and reconciles any drift — including
// deletes the watcher's per-path handler missed.
const safetyRescanInterval = 10 * time.Minute

// Watch runs a filesystem watcher that incrementally indexes added, changed,
// and deleted files for the life of ctx. It is intended to run after the
// initial Run() scan completes, turning a one-shot index into a continuously
// maintained one.
//
// Design notes:
//   - Event draining (reading fsnotify events) is decoupled from per-document
//     processing: a worker goroutine performs the slow work (hashing,
//     extraction, embedding) while the main loop only debounces events, so a
//     slow document can never stall ingestion enough for fsnotify to drop
//     events from its fixed-size buffer.
//   - Per-file changes reuse processDocument (which hash-diffs, so a redundant
//     fire is a cheap no-op) and a single-path delete handler.
//   - Changed files pass through the same discovery filters as the initial
//     scan (gitignore, size cap, symlink policy, path excludes) before being
//     indexed.
//   - A periodic safety rescan backstops missed events; the watcher alone is
//     not a correctness guarantee.
//   - The watcher deliberately does NOT toggle indexingState.Running: the
//     initial scan owns that signal, and a steady-state server picking up the
//     occasional new file should not flip IndexingComplete back to false.
func (s *Service) Watch(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	root := s.cfg.RootDir
	if root == "" {
		return nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()

	if err := s.addWatchDirs(watcher, absRoot); err != nil {
		// A failure to register some dirs is non-fatal — the safety rescan
		// still catches changes there. Log and continue.
		s.getLogger().Printf("watch: registering directories: %v", err)
	}

	secrets, err := compileSecretPatterns(s.cfg.SecretPatterns)
	if err != nil {
		return err
	}

	debounce := s.cfg.IngestWatchDebounce
	if debounce <= 0 {
		debounce = defaultWatchDebounce
	}

	w := &fsWatchLoop{
		svc:      s,
		watcher:  watcher,
		absRoot:  absRoot,
		secrets:  secrets,
		opts:     DiscoverOptionsFromConfig(s.cfg),
		debounce: debounce,
		pending:  make(map[string]*time.Timer),
		fire:     make(chan watchJob, 256),
	}
	return w.run(ctx)
}

type watchJob struct {
	absPath string
	deleted bool
}

type fsWatchLoop struct {
	svc      *Service
	watcher  *fsnotify.Watcher
	absRoot  string
	secrets  []*regexp.Regexp
	opts     DiscoverOptions
	debounce time.Duration

	mu      sync.Mutex
	pending map[string]*time.Timer
	fire    chan watchJob
}

func (w *fsWatchLoop) run(ctx context.Context) error {
	rescanReq := make(chan struct{}, 1)
	workerDone := make(chan struct{})
	go w.worker(ctx, rescanReq, workerDone)

	rescan := time.NewTicker(safetyRescanInterval)
	defer rescan.Stop()

	for {
		select {
		case <-ctx.Done():
			<-workerDone
			return nil
		case <-rescan.C:
			// Backstop: reconcile anything the watcher missed. Coalesce — if a
			// rescan is already queued or running, drop this tick.
			select {
			case rescanReq <- struct{}{}:
			default:
			}
		case ev, ok := <-w.watcher.Events:
			if !ok {
				<-workerDone
				return nil
			}
			w.handleEvent(ev)
		case err, ok := <-w.watcher.Errors:
			if !ok {
				<-workerDone
				return nil
			}
			if err != nil {
				w.svc.getLogger().Printf("watch: %v", err)
			}
		}
	}
}

// worker performs the slow per-document work and safety rescans off the
// event-draining path. Keeping fsnotify event reads in run()'s select loop
// (which only debounces) prevents the kernel's fixed event buffer from
// overflowing while a large file is being extracted/embedded.
func (w *fsWatchLoop) worker(ctx context.Context, rescanReq <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-w.fire:
			w.process(ctx, job)
		case <-rescanReq:
			if err := w.svc.runScan(ctx); err != nil && ctx.Err() == nil {
				w.svc.getLogger().Printf("watch: safety rescan: %v", err)
			}
		}
	}
}

// handleEvent classifies a raw fsnotify event and (re)arms the per-path
// debounce timer. Removes/renames cancel any pending write for the path and
// enqueue a delete; creates of directories register the whole new subtree.
func (w *fsWatchLoop) handleEvent(ev fsnotify.Event) {
	abs := ev.Name
	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		w.arm(abs, true)
		return
	}
	if ev.Op&(fsnotify.Create|fsnotify.Write) != 0 {
		// A newly created (or moved-in) directory must be watched recursively,
		// since fsnotify is not recursive and the Create events for nested
		// children were emitted before we were watching them.
		if info, err := os.Stat(abs); err == nil && info.IsDir() {
			if !shouldSkipDirectory(filepath.Base(abs)) {
				w.registerNewTree(abs)
			}
			return
		}
		w.arm(abs, false)
	}
}

// registerNewTree registers watches for every non-skipped directory under
// absDir and arms index jobs for any regular files already present, so a
// directory tree created or moved into the watched root is picked up
// immediately rather than at the next safety rescan. It only walks the
// directory tree (no document I/O), so it stays cheap on the event loop.
func (w *fsWatchLoop) registerNewTree(absDir string) {
	_ = filepath.WalkDir(absDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != absDir && shouldSkipDirectory(d.Name()) {
				return filepath.SkipDir
			}
			if err := w.watcher.Add(path); err != nil {
				w.svc.getLogger().Printf("watch: add %s: %v", path, err)
			}
			return nil
		}
		// Regular file already present in the new tree — arm it for indexing.
		// Discovery filters are applied later in process().
		w.arm(path, false)
		return nil
	})
}

// arm resets the debounce timer for absPath; when it fires the job is sent to
// the worker for processing.
func (w *fsWatchLoop) arm(absPath string, deleted bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.pending[absPath]; ok {
		t.Stop()
	}
	w.pending[absPath] = time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		delete(w.pending, absPath)
		w.mu.Unlock()
		select {
		case w.fire <- watchJob{absPath: absPath, deleted: deleted}:
		default:
			// Channel full: the safety rescan will reconcile.
		}
	})
}

// process applies a debounced job: index the file or mark it deleted.
func (w *fsWatchLoop) process(ctx context.Context, job watchJob) {
	rel, err := filepath.Rel(w.absRoot, job.absPath)
	if err != nil {
		return
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || matchesAnyPathExclude(rel, w.svc.cfg.PathExcludes) {
		return
	}

	info, statErr := os.Lstat(job.absPath)
	if job.deleted || os.IsNotExist(statErr) {
		w.svc.processDelete(ctx, rel)
		return
	}
	if statErr != nil {
		return
	}

	f, ok := w.indexableFile(job.absPath, rel, info)
	if !ok {
		return
	}
	if err := w.svc.processDocument(ctx, f, w.secrets, false, nil); err != nil && ctx.Err() == nil {
		w.svc.addErrors(1)
		w.svc.getLogger().Printf("watch: index %s: %v", rel, err)
	}
}

// indexableFile applies the same discovery filters as the initial scan
// (directory/symlink policy, size cap, .gitignore) and returns the
// DiscoveredFile to index, or ok=false if the path should be skipped.
func (w *fsWatchLoop) indexableFile(absPath, rel string, info os.FileInfo) (DiscoveredFile, bool) {
	if info.IsDir() {
		return DiscoveredFile{}, false
	}
	// Symlink policy: mirror the discovery walker. When symlinks are not
	// followed, skip them; otherwise resolve to the target for size/type.
	if info.Mode()&os.ModeSymlink != 0 {
		if !w.opts.FollowSymlinks {
			return DiscoveredFile{}, false
		}
		target, err := os.Stat(absPath)
		if err != nil {
			return DiscoveredFile{}, false
		}
		info = target
	}
	if !info.Mode().IsRegular() || info.Size() > w.opts.MaxSizeBytes {
		return DiscoveredFile{}, false
	}
	if w.opts.UseGitIgnore {
		ignored, err := matchesGitignoreForFile(w.absRoot, rel)
		if err != nil {
			w.svc.getLogger().Printf("watch: gitignore check %s: %v", rel, err)
		} else if ignored {
			return DiscoveredFile{}, false
		}
	}
	return DiscoveredFile{
		AbsPath:   absPath,
		RelPath:   rel,
		SizeBytes: info.Size(),
		MTimeUnix: info.ModTime().Unix(),
		Mode:      info.Mode(),
	}, true
}

// matchesGitignoreForFile reports whether the file at slash-separated relPath
// (relative to absRoot) is excluded by .gitignore rules, mirroring the
// hierarchical rule accumulation the discovery walker performs. A directory
// excluded by an ancestor rule excludes everything beneath it.
func matchesGitignoreForFile(absRoot, relPath string) (bool, error) {
	rel := strings.TrimPrefix(filepath.ToSlash(relPath), "./")
	if rel == "" || rel == "." {
		return false, nil
	}
	segs := strings.Split(rel, "/")

	rules, err := parseGitIgnoreRules(absRoot, "")
	if err != nil {
		return false, err
	}
	relDir := ""
	absDir := absRoot
	for i := 0; i < len(segs)-1; i++ {
		dirRel := segs[i]
		if relDir != "" {
			dirRel = relDir + "/" + segs[i]
		}
		if matchesGitIgnoreRules(rules, dirRel, true) {
			return true, nil
		}
		absDir = filepath.Join(absDir, segs[i])
		relDir = dirRel
		local, err := parseGitIgnoreRules(absDir, relDir)
		if err != nil {
			return false, err
		}
		rules = append(rules, local...)
	}
	return matchesGitIgnoreRules(rules, rel, false), nil
}

// addWatchDirs registers a watch on absRoot and every non-skipped subdirectory.
// fsnotify is not recursive, so each directory must be added individually. A
// single unwatchable directory does not abort registration of its siblings;
// per-directory failures are aggregated and returned.
func (s *Service) addWatchDirs(watcher *fsnotify.Watcher, absRoot string) error {
	var errs []error
	_ = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; safety rescan still covers them
		}
		if !d.IsDir() {
			return nil
		}
		if path != absRoot && shouldSkipDirectory(d.Name()) {
			return filepath.SkipDir
		}
		if addErr := watcher.Add(path); addErr != nil {
			errs = append(errs, fmt.Errorf("watch %s: %w", path, addErr))
		}
		return nil
	})
	return errors.Join(errs...)
}

// processDelete tombstones a single removed path and evicts it from retrieval,
// mirroring the per-document portion of markMissingAsDeleted. Unknown paths are
// a harmless no-op.
func (s *Service) processDelete(ctx context.Context, relPath string) {
	deleter, ok := s.store.(documentDeleteMarker)
	if !ok {
		return
	}
	if err := deleter.MarkDocumentDeleted(ctx, relPath); err != nil {
		s.addErrors(1)
		return
	}
	s.addDeleted(1)
	s.onDocumentsDeletedMu.RLock()
	onDeleted := s.onDocumentsDeleted
	s.onDocumentsDeletedMu.RUnlock()
	if onDeleted != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.addErrors(1)
					s.getLogger().Printf("onDocumentsDeleted panic for %s (%s)", relPath, safePanicValue(r))
				}
			}()
			onDeleted([]string{relPath})
		}()
	}
}
