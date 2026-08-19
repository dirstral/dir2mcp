package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/model"
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

// watchJobQueueCapacity is the depth of the internal job queue between the
// event loop and the worker. It buys the event loop room to keep draining
// fsnotify while the worker extracts and embeds one document.
//
// The queue is lossy on purpose: a send that would block is dropped instead,
// because blocking the event loop is what makes the kernel drop events. A drop
// is therefore normal under a large burst, and it is handled by
// handleQueueFull, not ignored.
const watchJobQueueCapacity = 256

// queueFullReportInterval rate limits the queue-saturation log line. One burst
// can drop thousands of jobs, and one line per drop would bury the corpus log.
// The first drop reports at once; while drops continue, one more line follows
// per interval, and each line carries the running total.
const queueFullReportInterval = 30 * time.Second

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
//   - The job queue between the two is bounded and lossy. A drop asks for an
//     immediate coalesced rescan and is logged, so a saturated queue costs one
//     reconcile, not up to ten minutes of stale content (issue #679).
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
		fire:     make(chan watchJob, watchJobQueueCapacity),
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

	// requestRescan queues a coalesced safety rescan. run() installs it before
	// the worker starts, so every caller (the event loop and the worker alike)
	// shares one request slot.
	requestRescan func()

	mu      sync.Mutex
	pending map[string]*time.Timer
	fire    chan watchJob

	// dropMu guards the queue-saturation report state below. It is a separate
	// lock from mu on purpose: mu serializes the debounce timer map, and a drop
	// is recorded from inside a fired timer, after that map entry is gone.
	dropMu sync.Mutex
	// droppedJobs counts every debounced job the queue refused over the life of
	// the loop. It feeds the log line, so an operator reads a running total
	// rather than a single incident.
	droppedJobs int64
	// unreconciledDrops counts the drops the next reconcile has still to cover.
	// The reconcile takes the count and names it, so an operator learns how big
	// the burst was, not only that one drop happened.
	unreconciledDrops int64
	// lastDropReport is when the first-drop line was last written. It rate limits
	// that line; see queueFullReportInterval.
	lastDropReport time.Time
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
	w.requestRescan = requestRescan
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
//
// A queued rescan competes with the job queue on equal terms: select picks at
// random between two ready cases, so a rescan requested because the queue
// saturated (issue #679) starts after a job or two, not after the whole backlog.
func (w *fsWatchLoop) worker(ctx context.Context, rescanReq <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-w.fire:
			w.process(ctx, job)
		case <-rescanReq:
			dropped := w.reportDropsUnderReconcile()
			if err := w.svc.runScan(ctx); err != nil && ctx.Err() == nil {
				w.svc.getLogger().Printf("watch: safety rescan: %v", err)
				// The scan did not reconcile them after all, so give the drops
				// back. The next scan then reports the true outstanding number
				// instead of losing this burst from the ledger.
				w.returnUnreconciledDrops(dropped)
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
		job := watchJob{absPath: absPath, deleted: deleted}
		select {
		case w.fire <- job:
		default:
			w.handleQueueFull(job)
		}
	})
}

// handleQueueFull reacts to a debounced job the worker queue could not accept
// (issue #679).
//
// The send stays non-blocking. To block here would hold a debounce timer
// goroutine, and, once the timers pile up, the event loop is the next thing to
// stall, which is exactly what makes the kernel drop events. So the job is
// still dropped. What changes is the consequence of the drop:
//
//   - It asks for an immediate coalesced rescan. The rescan is a full hash-diff
//     scan, so it reconciles every dropped job at once, whatever the job was: a
//     new or edited file is indexed, and a file that is gone is tombstoned. The
//     request is coalesced, so a burst of ten thousand drops costs one
//     reconcile.
//   - It reports the loss. Before this, a drop was invisible: the corpus served
//     stale or missing content until the next safety rescan tick, up to ten
//     minutes later, with nothing in the log to say why. An operator now reads
//     one line when the queue first refuses a change, and one line when the
//     reconcile starts, which names how many changes it covers.
//
// The work here is deliberately tiny (one counter, one non-blocking send, and a
// rate-limited log line) and it runs only on the drop path, so it adds nothing
// to the cost of handling a normal event.
//
// SPEC note: this does NOT touch the watch_overflows counter. SPEC §15.6 and
// stats.json define that field as the count of fsnotify KERNEL event-buffer
// overflows. This queue is dir2mcp's own, and the kernel buffer did not
// overflow, so counting it there would serve a number the field does not
// describe. To surface a served counter for an internal drop needs a
// dirstral-spec change first.
func (w *fsWatchLoop) handleQueueFull(job watchJob) {
	// The drop is counted before the reconcile is asked for, so the count a
	// starting reconcile reads already includes every drop that asked for it.
	// The log line comes last, so it can only claim a reconcile already queued.
	dropped, report := w.recordDroppedJob(time.Now())
	rescanQueued := w.askForRescan()
	if !report {
		return
	}
	// The path is quoted with %q, not printed raw. A file name can carry a
	// newline or another control character, and this line goes to an untrusted
	// sink (a log file, a pipe). %q escapes them, so a crafted name cannot forge
	// a second log line. The path itself stays: an operator needs to know which
	// change was lost.
	w.svc.getLogger().Printf(
		"watch: internal job queue full (%d jobs): dropped the pending change for %q; %d change(s) dropped since the watcher started; %s",
		cap(w.fire), job.absPath, dropped, rescanNote(rescanQueued),
	)
}

// recordDroppedJob counts one dropped job and reports whether this drop must be
// logged now. It returns the running total so the line names it.
func (w *fsWatchLoop) recordDroppedJob(now time.Time) (int64, bool) {
	w.dropMu.Lock()
	defer w.dropMu.Unlock()
	w.droppedJobs++
	w.unreconciledDrops++
	if !w.lastDropReport.IsZero() && now.Sub(w.lastDropReport) < queueFullReportInterval {
		return w.droppedJobs, false
	}
	w.lastDropReport = now
	return w.droppedJobs, true
}

// reportDropsUnderReconcile logs the size of the burst a starting reconcile has
// to repair and returns that count (issue #679). It runs before every rescan the
// worker performs and says nothing when no job was dropped, so a periodic or
// rule-driven rescan logs as it did before.
//
// The line closes the report the drop path opened: the drop path says a change
// was lost, this one says how many the reconcile covers. Taking the count also
// clears the rate limit, so the first drop of the NEXT burst reports at once
// instead of waiting out the interval.
func (w *fsWatchLoop) reportDropsUnderReconcile() int64 {
	w.dropMu.Lock()
	pending := w.unreconciledDrops
	if pending > 0 {
		w.unreconciledDrops = 0
		w.lastDropReport = time.Time{}
	}
	w.dropMu.Unlock()
	if pending == 0 {
		return 0
	}
	w.svc.getLogger().Printf(
		"watch: rescan starting to reconcile %d change(s) that the full internal job queue (%d jobs) dropped",
		pending, cap(w.fire),
	)
	return pending
}

// returnUnreconciledDrops puts drops back on the outstanding count after the
// rescan that claimed them failed. The count then stays true: the next rescan
// reports this burst again, together with anything dropped since.
func (w *fsWatchLoop) returnUnreconciledDrops(count int64) {
	if count <= 0 {
		return
	}
	w.dropMu.Lock()
	defer w.dropMu.Unlock()
	w.unreconciledDrops += count
}

// askForRescan queues a coalesced reconcile and reports whether it could. run()
// installs the request slot before the first event is handled, so false means
// the loop was built without one.
func (w *fsWatchLoop) askForRescan() bool {
	if w.requestRescan == nil {
		return false
	}
	w.requestRescan()
	return true
}

// rescanNote states what closes the gap the drop opened, so the log line never
// promises a reconcile that was not asked for.
func rescanNote(queued bool) string {
	if queued {
		return "an immediate rescan is queued to reconcile them"
	}
	return "the periodic safety rescan reconciles them"
}

// gitignoreFileName is the discovery rule file whose content decides which
// paths are eligible.
const gitignoreFileName = ".gitignore"

// documentStatusSkipped is the documents.status value for a discovered path
// that ingest refused. It carries a model.SkipReason* in skip_reason.
const documentStatusSkipped = "skipped"

// watchSkip says how the store must represent a path the watcher refuses.
//
// The two forms mirror what the initial scan does with the same path, so a
// watch decision and a rescan decision agree:
//
//   - reason set: discovery keeps a durable skipped row for such a path, so the
//     watcher writes that row too. The value is a model.SkipReason* constant.
//   - reason empty: discovery drops the path entirely, so the watcher
//     tombstones the document.
type watchSkip struct {
	reason    string
	sizeBytes int64
}

// process applies a debounced job: index the file, reconcile it, or mark it
// deleted.
func (w *fsWatchLoop) process(ctx context.Context, job watchJob) {
	rel, ok := w.relPath(job.absPath)
	if !ok {
		return
	}
	w.rescanOnIgnoreRuleChange(job.absPath)

	info, statErr := os.Lstat(job.absPath)
	if statErr != nil {
		w.processStatError(ctx, job, rel, statErr)
		return
	}

	f, skip, ok := w.indexableFile(job.absPath, rel, info)
	if !ok {
		// The path exists but is no longer indexable: it became a directory, it
		// grew past the size cap, it is newly gitignored, or it is an unfollowed
		// symlink. A document that was indexed under the old rules must stop
		// being searchable now, not at the next safety rescan (issue #680).
		w.svc.reconcileIneligibleDocument(ctx, rel, skip)
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

// relPath converts an event path to the corpus-relative, slash-separated path
// the store keys on. ok=false means the watcher must ignore the event.
func (w *fsWatchLoop) relPath(absPath string) (string, bool) {
	rel, err := filepath.Rel(w.absRoot, absPath)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || matchesAnyPathExclude(rel, w.svc.cfg.PathExcludes) {
		return "", false
	}
	return rel, true
}

// rescanOnIgnoreRuleChange asks for a coalesced rescan when the changed path is
// a .gitignore file (issue #680).
//
// A rule file decides the eligibility of every path below it, and one write can
// flip many of them at once. No per-path event describes that, so the watcher
// asks the scan to reconcile the tree. The request is coalesced, so a burst of
// edits collapses to one reconcile.
func (w *fsWatchLoop) rescanOnIgnoreRuleChange(absPath string) {
	if !w.opts.UseGitIgnore || w.requestRescan == nil {
		return
	}
	if filepath.Base(absPath) != gitignoreFileName {
		return
	}
	w.requestRescan()
}

// processStatError handles a job whose path could not be read.
func (w *fsWatchLoop) processStatError(ctx context.Context, job watchJob, rel string, statErr error) {
	if os.IsNotExist(statErr) {
		// The path is genuinely gone: tombstone it. Covers both a delete job and
		// a write job whose file vanished before we processed it.
		w.svc.processDelete(ctx, rel)
		// A directory that is removed or renamed out of the tree is reported as
		// ONE event for the directory path. The store holds no document at that
		// path, only documents below it, so the descendants must be reconciled
		// here too (issue #678).
		w.svc.processDeleteSubtree(ctx, w.absRoot, rel)
		return
	}
	// Transient stat error (e.g. permissions). Honor a delete job as before;
	// for a write job, skip and let the safety rescan reconcile.
	if job.deleted {
		w.svc.processDelete(ctx, rel)
	}
}

// indexableFile applies the same discovery filters as the initial scan
// (directory/symlink policy, size cap, .gitignore) and returns the
// DiscoveredFile to index, or ok=false plus the way the store must record the
// refusal.
func (w *fsWatchLoop) indexableFile(absPath, rel string, info os.FileInfo) (DiscoveredFile, watchSkip, bool) {
	if info.IsDir() {
		// A directory is not a document. Discovery walks it instead of recording
		// it, so a document that was replaced by a directory leaves the corpus.
		return DiscoveredFile{}, watchSkip{}, false
	}
	// Symlink policy: mirror the discovery walker. When symlinks are not
	// followed, skip them; otherwise resolve the link and enforce root
	// containment before looking at the target at all.
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, ok := w.followedSymlinkTarget(absPath, rel)
		if !ok {
			return DiscoveredFile{}, w.symlinkSkip(), false
		}
		target, err := os.Stat(resolved)
		if err != nil {
			return DiscoveredFile{}, watchSkip{}, false
		}
		// Hand downstream the RESOLVED path, exactly as the discovery walker
		// records it in DiscoveredFile.AbsPath, so any consumer that reads the
		// absolute path reads the target we just checked rather than re-traversing
		// a link that may since have been repointed.
		absPath = resolved
		info = target
	}
	if !info.Mode().IsRegular() {
		return DiscoveredFile{}, watchSkip{}, false
	}
	if info.Size() > w.opts.MaxSizeBytes {
		// Discovery records an over-size file as a durable skipped row (#497), so
		// the watcher records the same row when a file grows past the cap.
		//
		// One divergence, under `media.variants.group: true`: the scan groups the
		// renditions first and records no row for a rendition grouping discards
		// (#879), while the watcher judges the single changed path and knows
		// nothing about its group. So a touched over-cap rendition still gets a
		// row here, and the next full scan tombstones it. The watcher does not
		// group renditions at all (it would also index a non-canonical rendition
		// as its own document), so teaching it §8.6.5 is one change, not this one.
		return DiscoveredFile{}, watchSkip{reason: model.SkipReasonSizeCap, sizeBytes: info.Size()}, false
	}
	if w.opts.UseGitIgnore {
		ignored, err := matchesGitignoreForFile(w.absRoot, rel)
		if err != nil {
			w.svc.getLogger().Printf("watch: gitignore check %s: %v", rel, err)
		} else if ignored {
			// Discovery never yields a gitignored path, so a rescan tombstones it.
			return DiscoveredFile{}, watchSkip{}, false
		}
	}
	return DiscoveredFile{
		AbsPath:   absPath,
		RelPath:   rel,
		SizeBytes: info.Size(),
		MTimeUnix: info.ModTime().Unix(),
		Mode:      info.Mode(),
	}, watchSkip{}, true
}

// symlinkSkip says how the store records a symlink the watcher refuses.
//
// With following off, discovery reports the link and persists a
// symlink_ignored row (#781). With following on, the only refusal left is a
// target outside the corpus root, which discovery drops without a row, so the
// document is tombstoned instead.
func (w *fsWatchLoop) symlinkSkip() watchSkip {
	if w.opts.FollowSymlinks {
		return watchSkip{}
	}
	return watchSkip{reason: model.SkipReasonSymlinkIgnored}
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
	s.notifyDocumentsDeleted([]string{relPath})
}

// reconcileIneligibleDocument retires a document whose path still exists but no
// longer passes the discovery filters (issue #680).
//
// Before this, an unindexable write was dropped, so a file that grew past the
// size cap or that became gitignored kept its old chunks. Search and ask
// returned content the current ingest policy excludes, until the safety rescan
// up to ten minutes later. For a newly ignored path that is a policy surprise:
// the operator excluded it and it stayed retrievable.
//
// The two outcomes mirror what a full scan does with the same path, so watch
// and rescan agree:
//
//   - skip.reason empty: the path leaves the corpus. Tombstone it.
//   - skip.reason set: discovery keeps a durable skipped row for such a path.
//     The document is tombstoned first, which is what evicts the
//     representations and the chunks, and the document row is then written back
//     as a skipped row. The result is a visible document with a skip reason and
//     no searchable content, which is exactly how the initial scan represents a
//     file it refused.
//
// SPEC §6.6 is preserved. The eviction goes through MarkDocumentDeleted, so the
// representations and the chunks carry deleted=1. Nothing is hard-deleted.
func (s *Service) reconcileIneligibleDocument(ctx context.Context, relPath string, skip watchSkip) {
	doc, err := s.store.GetDocumentByPath(ctx, relPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && ctx.Err() == nil {
			s.getLogger().Printf("watch: read document %s: %v", relPath, err)
		}
		return
	}
	if doc.Deleted {
		return // already retired; nothing of it is searchable
	}
	if skip.reason == "" {
		s.processDelete(ctx, relPath)
		return
	}
	if doc.Status == documentStatusSkipped && doc.SkipReason == skip.reason {
		// The transition was already reconciled. The pass that set this state
		// evicted the chunks, so a repeated write (an append to an over-size
		// file) must not write the row again.
		return
	}
	s.processDelete(ctx, relPath)
	skipped := model.Document{
		RelPath:    relPath,
		DocType:    ClassifyDocType(relPath),
		SizeBytes:  skip.sizeBytes,
		Status:     documentStatusSkipped,
		SkipReason: skip.reason,
	}
	if err := s.store.UpsertDocument(ctx, skipped); err != nil {
		if ctx.Err() == nil {
			s.getLogger().Printf("watch: persist %s skip row for %s: %v", skip.reason, relPath, err)
		}
		return
	}
	s.addSkipped(1)
	s.notifyDocumentSkip(skipped)
}

// subtreeListPageSize pages the descendant listing processDeleteSubtree reads.
// It matches the page size listActiveDocuments uses for the full corpus.
const subtreeListPageSize = 500

// processDeleteSubtree tombstones every active document stored below relDir and
// evicts it from retrieval (issue #678).
//
// The watcher needs it because a filesystem does not report a subtree removal
// per child. A `rm -rf docs` or a `mv docs ../elsewhere` can arrive as a single
// Remove/Rename event for `docs`, while the store holds one document per
// descendant file. Without this pass those documents stay searchable until the
// safety rescan, which is up to ten minutes later.
//
// The caller MUST have proven that relDir itself is gone from disk. That proof
// is what makes the deletion safe: a document at `relDir/child` can only be
// reachable through a directory at `relDir`, so if `relDir` does not exist then
// no descendant of it exists either. The per-path Lstat below re-checks that
// for each candidate, so a path that somehow still exists keeps its chunks.
//
// SPEC §6.6 is preserved: each document is retired through MarkDocumentDeleted,
// which sets deleted=1 on the document, its representations and its chunks.
// Nothing is hard-deleted.
func (s *Service) processDeleteSubtree(ctx context.Context, absRoot, relDir string) {
	deleter, ok := s.store.(documentDeleteMarker)
	if !ok {
		return
	}
	paths, err := s.activeDocumentsUnder(ctx, relDir)
	if err != nil {
		if ctx.Err() == nil {
			s.getLogger().Printf("watch: list documents under %s: %v", relDir, err)
		}
		return
	}
	deleted := make([]string, 0, len(paths))
	for _, relPath := range paths {
		if ctx.Err() != nil {
			break
		}
		if pathStillPresent(absRoot, relPath) {
			continue
		}
		if err := deleter.MarkDocumentDeleted(ctx, relPath); err != nil {
			s.addErrors(1)
			continue
		}
		s.addDeleted(1)
		deleted = append(deleted, relPath)
	}
	s.notifyDocumentsDeleted(deleted)
}

// activeDocumentsUnder returns the sorted rel_paths of every active document
// stored below relDir.
//
// The store prefix filter is only a coarse pre-filter. ListFiles normalizes the
// prefix, which drops a trailing slash, and SQLite LIKE compares ASCII letters
// case-insensitively. The prefix "docs" therefore also matches "docsets/a.md"
// and "Docs/a.md". The byte-exact HasPrefix test below is what makes the
// returned set safe to tombstone.
func (s *Service) activeDocumentsUnder(ctx context.Context, relDir string) ([]string, error) {
	prefix := relDir + "/"
	var out []string
	offset := 0
	for {
		docs, total, err := s.store.ListFiles(ctx, relDir, "", subtreeListPageSize, offset)
		if err != nil {
			if errors.Is(err, model.ErrNotImplemented) {
				return nil, nil
			}
			return nil, err
		}
		for _, doc := range docs {
			if doc.Deleted || !strings.HasPrefix(doc.RelPath, prefix) {
				continue
			}
			out = append(out, doc.RelPath)
		}
		offset += len(docs)
		if len(docs) == 0 || int64(offset) >= total {
			break
		}
	}
	sort.Strings(out)
	return out, nil
}

// pathStillPresent reports whether relPath names an entry that is still on disk
// under absRoot.
//
// It guards the destructive subtree pass. An archive member carries a synthetic
// rel_path ("book.zip/chapter.md") that never exists on disk, so a member of a
// removed archive is retired as it should be, while a real file that survived
// the event keeps its chunks.
func pathStillPresent(absRoot, relPath string) bool {
	_, err := os.Lstat(filepath.Join(absRoot, filepath.FromSlash(relPath)))
	return err == nil
}
