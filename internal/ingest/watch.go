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

	"github.com/dirstral/dir2mcp/internal/corpusfs"
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

// remoteRescanInterval is how often a remote corpus (source.kind=s3)
// reconciles while ingest.watch is on. A remote corpus has no filesystem event
// source, so the reconcile is the only continuous-sync mechanism. It is a
// separate constant from safetyRescanInterval on purpose: the two loops have
// different semantics. safetyRescanInterval backstops a live fsnotify watcher;
// this interval IS the sync. Debounce and overflow do not apply to it.
const remoteRescanInterval = 10 * time.Minute

// SourceSupportsFileWatch reports whether source.kind names a corpus that the
// local filesystem watcher can observe.
//
// Only a real filesystem qualifies. "local" and "nfs" are both ordinary
// directory trees under cfg.RootDir (SPEC §7.8), so an fsnotify event maps to
// the true corpus path. An NFS mount sees fewer events than a local disk,
// because inotify does not report a write made by another client. That is a
// completeness limit, not a correctness one, and the safety rescan covers it.
//
// "s3" does not qualify. An S3 corpus is a set of objects under a bucket and
// prefix. cfg.RootDir still defaults to ".", so the watcher would root itself
// at an unrelated local directory and read local events as corpus events
// (issue #695). Kind matching mirrors corpusfs.New: the comparison is
// case-insensitive and an empty kind normalizes to local.
func SourceSupportsFileWatch(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "local", "nfs":
		return true
	default:
		return false
	}
}

// Watch keeps the index in sync with the corpus for the life of ctx. It is
// intended to run after the initial Run() scan completes, turning a one-shot
// index into a continuously maintained one.
//
// The mechanism depends on source.kind (SPEC §7.8). A local or NFS corpus gets
// the fsnotify watcher below. A remote corpus gets a periodic reconcile
// through the configured CorpusFS instead. See watchRemote.
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
	// The source-kind gate runs before the RootDir check. An S3 corpus does not
	// need a local root at all (issue #738), so a non-empty RootDir must never
	// decide which watch mechanism runs.
	if !SourceSupportsFileWatch(s.cfg.Source.Kind) {
		return s.watchRemote(ctx)
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

	discoverOpts := DiscoverOptionsFromConfig(s.cfg)
	if err := s.addWatchDirs(watcher, absRoot, discoverOpts); err != nil {
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
		opts:     discoverOpts,
		excluded: discoverOpts.ExcludedDirs(),
		debounce: debounce,
		pending:  make(map[string]*time.Timer),
		fire:     make(chan watchJob, 256),
	}
	return w.run(ctx)
}

// watchRemote keeps a remote corpus in sync with a periodic reconcile. It runs
// instead of the fsnotify watcher when source.kind names a corpus that no local
// filesystem event can describe (issue #695).
//
// The reconcile is a normal incremental scan. runScan enumerates through the
// configured CorpusFS, so it reads the actual remote objects: a new or changed
// object is ingested, and an object that is gone is tombstoned. Every mutation
// therefore comes from the remote source itself. No local file event can reach
// the store.
//
// This loop deliberately does NOT call MarkWatchActive. There is no fsnotify
// watcher, so there is no kernel event buffer and no overflow to count. SPEC
// §15.6 requires the server to omit watch_overflows when it does not run the
// watcher, and a consumer must read that absence as "not applicable".
//
// Debounce does not apply either. ingest.watch_debounce coalesces editor write
// bursts on a local disk; a scheduled full reconcile has no per-path bursts to
// coalesce.
func (s *Service) watchRemote(ctx context.Context) error {
	ticker := time.NewTicker(remoteRescanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.runScan(ctx); err != nil && ctx.Err() == nil {
				s.getLogger().Printf("watch: remote corpus rescan: %v", err)
			}
		}
	}
}

type watchJob struct {
	absPath string
	deleted bool
}

type fsWatchLoop struct {
	svc     *Service
	watcher *fsnotify.Watcher
	absRoot string
	secrets []*regexp.Regexp
	opts    DiscoverOptions
	// excluded is the directory ignore set resolved from opts once, so every
	// watch decision reads the list the initial scan read (#773).
	excluded corpusfs.ExcludedDirSet
	debounce time.Duration

	mu      sync.Mutex
	pending map[string]*time.Timer
	fire    chan watchJob
}

func (w *fsWatchLoop) run(ctx context.Context) error {
	rescanReq := make(chan struct{}, 1)
	// requestRescan queues a coalesced safety rescan: if one is already queued or
	// running the request is dropped, so a burst of triggers collapses to a
	// single reconcile.
	requestRescan := func() {
		select {
		case rescanReq <- struct{}{}:
		default:
		}
	}
	// A watcher is now running: let the stats surface report watch_overflows
	// (even 0) rather than omitting it as "not applicable" (#591).
	w.svc.indexingState.MarkWatchActive()

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
			// Backstop: reconcile anything the watcher missed.
			requestRescan()
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
			if err == nil {
				continue
			}
			// fsnotify reports ErrEventOverflow when the kernel dropped events
			// from its fixed-size buffer during a large burst (mass checkout,
			// rsync, branch switch). Previously this only logged and relied on
			// the up-to-10-minute safety rescan, so dropped updates lagged
			// silently (issue #409 item 5). Log a distinct line and request an
			// immediate coalesced rescan so the missed changes reconcile promptly
			// rather than waiting for the next tick.
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				w.svc.indexingState.AddWatchOverflow(1)
				w.svc.getLogger().Printf("watch: event overflow (kernel event buffer exceeded); some events dropped, requesting immediate rescan")
				requestRescan()
				continue
			}
			w.svc.getLogger().Printf("watch: %v", err)
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
			if !shouldSkipDirectory(w.excluded, filepath.Base(abs)) {
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
	addDir := func(path string) {
		if err := w.watcher.Add(path); err != nil {
			w.svc.getLogger().Printf("watch: add %s: %v", path, err)
		}
	}
	// Regular files already present in the new tree are armed for indexing;
	// discovery filters are applied later in process().
	armFile := func(path string) { w.arm(path, false) }

	// With symlink following on, descend symlinked subdirectories too so the
	// watch coverage matches the discovery walker (internal/corpusfs/local.go),
	// which follows them when IngestFollowSymlinks is set. The default (off) path
	// keeps the historical WalkDir behavior byte-for-byte.
	if w.opts.FollowSymlinks {
		rootResolved := corpusfs.ResolveRoot(w.absRoot)
		// A newly created (or moved-in) directory can itself be a symlink pointing
		// outside the corpus. walkWatchTree refuses it, but say so: an operator who
		// linked a directory into the corpus and sees nothing appear needs to know
		// it was refused on purpose, not missed.
		if _, ok := corpusfs.ResolveSymlinkWithinRoot(rootResolved, absDir); !ok {
			w.svc.getLogger().Printf(
				"watch: refusing directory %s: it does not resolve inside the corpus root; not watched or indexed",
				absDir,
			)
			return
		}
		walkWatchTree(absDir, rootResolved, w.excluded, map[string]struct{}{}, addDir, armFile)
		return
	}
	_ = filepath.WalkDir(absDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != absDir && shouldSkipDirectory(w.excluded, d.Name()) {
				return filepath.SkipDir
			}
			addDir(path)
			return nil
		}
		armFile(path)
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
	if os.IsNotExist(statErr) {
		// The path is genuinely gone: tombstone it. Covers both a delete job and
		// a write job whose file vanished before we processed it.
		w.svc.processDelete(ctx, rel)
		return
	}
	if statErr != nil {
		// Transient stat error (e.g. permissions). Honor a delete job as before;
		// for a write job, skip and let the safety rescan reconcile.
		if job.deleted {
			w.svc.processDelete(ctx, rel)
		}
		return
	}

	f, ok := w.indexableFile(job.absPath, rel, info)
	if !ok {
		// The path exists but is no longer indexable (became a directory, was
		// filtered out, exceeds the size cap, is gitignored, or is an unfollowed
		// symlink). A delete job is honored so a document replaced by a
		// non-indexable entry is removed; a write job is simply dropped, matching
		// the prior behavior for an unindexable modify.
		if job.deleted {
			w.svc.processDelete(ctx, rel)
		}
		return
	}
	// The path exists and is indexable. Whether the job was armed as a delete
	// (a delete→recreate flap from an atomic save: write-temp + rename-over) or a
	// write, reconcile by (re)indexing rather than tombstoning. processDocument
	// hash-diffs, so a redundant fire is a cheap no-op, and the document is never
	// momentarily absent from the index (issue #409 item 4).
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
	// followed, skip them; otherwise resolve the link and enforce root
	// containment before looking at the target at all.
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, ok := w.followedSymlinkTarget(absPath, rel)
		if !ok {
			return DiscoveredFile{}, false
		}
		target, err := os.Stat(resolved)
		if err != nil {
			return DiscoveredFile{}, false
		}
		// Hand downstream the RESOLVED path, exactly as the discovery walker
		// records it in DiscoveredFile.AbsPath, so any consumer that reads the
		// absolute path reads the target we just checked rather than re-traversing
		// a link that may since have been repointed.
		absPath = resolved
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

// followedSymlinkTarget applies the symlink policy to an in-root symlink the
// watcher is about to index and returns its resolved target.
//
// It is the watcher's half of SPEC §1/§7.1 root isolation (issue #717): the
// initial discovery walk refuses a symlink whose target resolves outside the
// corpus, and a link that appears (or is repointed) while the watcher is running
// must be refused on exactly the same terms, so a corpus cannot be extended past
// its root just by creating a link after startup. The rule is not restated here:
// corpusfs.ResolveSymlinkWithinRoot is the one implementation both walks call.
//
// Resolution happens on every call, i.e. when the event is handled, so the
// decision is made against the link's target at that moment, not at creation.
// ok=false is logged rather than dropped silently: an unexplained absence is
// indistinguishable from "the watcher missed my file".
func (w *fsWatchLoop) followedSymlinkTarget(absPath, rel string) (string, bool) {
	if !w.opts.FollowSymlinks {
		return "", false
	}
	resolved, ok := corpusfs.ResolveSymlinkWithinRoot(corpusfs.ResolveRoot(w.absRoot), absPath)
	if !ok {
		w.svc.getLogger().Printf(
			"watch: refusing %s: symlink target does not resolve inside the corpus root; not indexed",
			rel,
		)
		return "", false
	}
	return resolved, true
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
func (s *Service) addWatchDirs(watcher *fsnotify.Watcher, absRoot string, opts DiscoverOptions) error {
	var errs []error
	followSymlinks := opts.FollowSymlinks
	excluded := opts.ExcludedDirs()
	add := func(path string) {
		if addErr := watcher.Add(path); addErr != nil {
			errs = append(errs, fmt.Errorf("watch %s: %w", path, addErr))
		}
	}
	// When symlink following is enabled, register watches on symlinked
	// subdirectories too, matching the discovery walker (internal/corpusfs/local.go)
	// so an fsnotify-driven edit under a symlinked tree is observed rather than
	// waiting for the 10-minute safety rescan (issue #409). Cycles terminate via
	// the resolved-path visited set. The default (off) path is unchanged.
	if followSymlinks {
		rootResolved := corpusfs.ResolveRoot(absRoot)
		// walkWatchTree refuses any directory it cannot resolve, the root included.
		// That can only happen if the root itself stopped resolving (deleted or
		// unreadable), in which case there is nothing to watch; report it instead of
		// registering zero watches silently.
		if _, ok := corpusfs.ResolveSymlinkWithinRoot(rootResolved, absRoot); !ok {
			return fmt.Errorf("watch %s: corpus root did not resolve; no directories watched", absRoot)
		}
		walkWatchTree(absRoot, rootResolved, excluded, map[string]struct{}{}, add, nil)
		return errors.Join(errs...)
	}
	_ = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; safety rescan still covers them
		}
		if !d.IsDir() {
			return nil
		}
		if path != absRoot && shouldSkipDirectory(excluded, d.Name()) {
			return filepath.SkipDir
		}
		add(path)
		return nil
	})
	return errors.Join(errs...)
}

// walkWatchTree recursively visits absDir, invoking onDir for every non-skipped
// directory and onFile (when non-nil) for every regular file, descending into
// symlinked directories too. It is used only on the symlink-following watch
// path and mirrors the discovery walker (internal/corpusfs/local.go): the
// visited set holds resolved (EvalSymlinks) directory paths already entered so a
// symlink cycle terminates AND a target reachable via both its real and a
// symlinked name is walked once (under whichever name is seen first); a
// symlinked directory is followed only when its target stays within rootResolved
// (the shared corpusfs check, so a directory link cannot extend the watch past
// the root any more than a file link can — #717).
// onDir/onFile receive the lexical path (through the symlink) so emitted fsnotify
// events map back to a path under the watched root.
func walkWatchTree(absDir, rootResolved string, excluded corpusfs.ExcludedDirSet, visited map[string]struct{}, onDir func(string), onFile func(string)) {
	resolved, ok := corpusfs.ResolveSymlinkWithinRoot(rootResolved, absDir)
	if !ok {
		return
	}
	if _, seen := visited[resolved]; seen {
		return
	}
	visited[resolved] = struct{}{}

	onDir(absDir)

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return // unreadable dir; safety rescan still covers it
	}
	for _, entry := range entries {
		name := entry.Name()
		child := filepath.Join(absDir, name)
		switch {
		case entry.IsDir():
			if shouldSkipDirectory(excluded, name) {
				continue
			}
			walkWatchTree(child, rootResolved, excluded, visited, onDir, onFile)
		case entry.Type()&os.ModeSymlink != 0:
			// Resolve the link to decide dir-vs-file; descend symlinked dirs
			// (containment is enforced on entry) and arm symlinked regular files.
			target, statErr := os.Stat(child)
			if statErr != nil {
				continue
			}
			if target.IsDir() {
				if shouldSkipDirectory(excluded, name) {
					continue
				}
				walkWatchTree(child, rootResolved, excluded, visited, onDir, onFile)
			} else if onFile != nil && target.Mode().IsRegular() {
				onFile(child)
			}
		default:
			if onFile != nil {
				onFile(child)
			}
		}
	}
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
