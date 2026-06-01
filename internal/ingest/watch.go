package ingest

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
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
//   - Per-file changes reuse processDocument (which hash-diffs, so a redundant
//     fire is a cheap no-op) and a single-path delete handler.
//   - Editor write bursts are coalesced by a per-path debounce timer.
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
	debounce time.Duration

	mu      sync.Mutex
	pending map[string]*time.Timer
	fire    chan watchJob
}

func (w *fsWatchLoop) run(ctx context.Context) error {
	rescan := time.NewTicker(safetyRescanInterval)
	defer rescan.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-rescan.C:
			// Backstop: reconcile anything the watcher missed.
			if err := w.svc.runScan(ctx); err != nil && ctx.Err() == nil {
				w.svc.getLogger().Printf("watch: safety rescan: %v", err)
			}
		case job := <-w.fire:
			w.process(ctx, job)
		case ev, ok := <-w.watcher.Events:
			if !ok {
				return nil
			}
			w.handleEvent(ev)
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return nil
			}
			if err != nil {
				w.svc.getLogger().Printf("watch: %v", err)
			}
		}
	}
}

// handleEvent classifies a raw fsnotify event and (re)arms the per-path
// debounce timer. Removes/renames cancel any pending write for the path and
// enqueue a delete; creates of directories register a new watch immediately.
func (w *fsWatchLoop) handleEvent(ev fsnotify.Event) {
	abs := ev.Name
	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		w.arm(abs, true)
		return
	}
	if ev.Op&(fsnotify.Create|fsnotify.Write) != 0 {
		// A newly created directory must be watched so its children are seen.
		if info, err := os.Stat(abs); err == nil && info.IsDir() {
			if !shouldSkipDirectory(filepath.Base(abs)) {
				_ = w.watcher.Add(abs)
			}
			return
		}
		w.arm(abs, false)
	}
}

// arm resets the debounce timer for absPath; when it fires the job is sent to
// the run loop for serialized processing.
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
	if statErr != nil || info.IsDir() {
		return
	}

	f := DiscoveredFile{
		AbsPath:   job.absPath,
		RelPath:   rel,
		SizeBytes: info.Size(),
		MTimeUnix: info.ModTime().Unix(),
		Mode:      info.Mode(),
	}
	if err := w.svc.processDocument(ctx, f, w.secrets, false, nil); err != nil && ctx.Err() == nil {
		w.svc.addErrors(1)
		w.svc.getLogger().Printf("watch: index %s: %v", rel, err)
	}
}

// addWatchDirs registers a watch on absRoot and every non-skipped subdirectory.
// fsnotify is not recursive, so each directory must be added individually.
func (s *Service) addWatchDirs(watcher *fsnotify.Watcher, absRoot string) error {
	return filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; safety rescan still covers them
		}
		if !d.IsDir() {
			return nil
		}
		if path != absRoot && shouldSkipDirectory(d.Name()) {
			return filepath.SkipDir
		}
		return watcher.Add(path)
	})
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
