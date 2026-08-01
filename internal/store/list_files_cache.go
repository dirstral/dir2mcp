package store

import (
	"context"
	"database/sql"
	"sync"
)

// listFilesCache memoizes the expensive part of ListFiles across the pages of a
// single listing walk (#429 F10): the `SELECT COUNT(*)` that the glob-free path
// used to run on every page, and the full rescan-and-reglob that the glob path
// used to run on every page. Walking a corpus of N documents cost O(N) count
// scans (and O(N) glob rescans) before this; it now costs one per walk.
//
// Exactness contract (the reason this is a version-guarded memo and not a TTL
// cache): an entry is reused ONLY while SQLite's `PRAGMA data_version` is
// unchanged on a pinned read connection. data_version changes whenever any OTHER
// connection or process commits to the database, so an unchanged value means no
// commit has landed since the entry was computed and the memoized total is still
// literally exact, not merely recent. The first commit from ingest (or from any
// other process) drops the whole cache and the next call recomputes from SQL.
//
// The probe MUST run on a connection that never writes: data_version is
// explicitly documented NOT to change for commits made on the same connection,
// and its values are only comparable within one connection. Hence the *sql.Conn
// pinned from SQLiteStore.vdb, the dedicated single-connection query_only handle
// that exists for nothing else. When that handle is unavailable, or the probe
// errors, the cache is bypassed entirely and every call recomputes, which is
// exactly the pre-#429-F10 behaviour.
//
// Locking: probeMu owns the pinned connection and is the only lock ever held
// across database I/O; mu guards the in-memory epoch and entries and is only
// ever held for a few field assignments. The order is always probeMu then mu,
// never the reverse. Keeping them separate means a cache lookup never waits
// behind someone else's probe.
type listFilesCache struct {
	probeMu sync.Mutex
	// conn is pinned for the store's lifetime because data_version readings are
	// only comparable when they come from the same connection. It belongs to the
	// dedicated single-connection probe handle (SQLiteStore.vdb), so pinning it
	// costs the read pool nothing.
	conn *sql.Conn

	mu      sync.Mutex
	version int64
	primed  bool
	entries map[string]listFilesCacheEntry
}

// listFilesCacheEntry is one memoized listing: the exact number of matching
// documents plus, for the glob path, a sparse index of where each block of
// stride matches starts, so a later page can resume mid-corpus instead of
// re-globbing from the first row.
type listFilesCacheEntry struct {
	total  int64
	stride int
	bounds []string
}

const (
	// listFilesCacheMaxEntries bounds the number of distinct (prefix, glob)
	// listings memoized within one data_version epoch. Overflow clears the map
	// rather than evicting an LRU victim: entries are cheap to recompute, epochs
	// are short-lived during ingest, and a clear cannot return a wrong total.
	listFilesCacheMaxEntries = 32

	// globBoundsMinStride keeps the sparse glob index small on tiny page sizes:
	// a caller paging one row at a time would otherwise record one boundary per
	// matching document.
	globBoundsMinStride = 256

	// globBoundsMaxCount caps the sparse glob index. On overflow the stride
	// doubles and every second boundary is dropped, so memory stays bounded no
	// matter how large the corpus is; the only cost is a slightly longer forward
	// scan between boundaries.
	globBoundsMaxCount = 4096
)

// begin probes the database change counter and returns the epoch that lookup
// and store must be called with. ok is false when the cache is unavailable; the
// caller then computes everything from SQL and memoizes nothing.
func (c *listFilesCache) begin(ctx context.Context, vdb *sql.DB) (epoch int64, ok bool) {
	if vdb == nil {
		return 0, false
	}
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	return c.probeLocked(ctx, vdb)
}

// probeLocked reads data_version on the pinned connection and rolls the epoch
// forward, dropping every memo when a commit has landed. c.probeMu must be held,
// which is also what keeps concurrent probes from applying their epochs out of
// order (data_version values are only defined for equality comparison, so a
// later reading cannot be recognized as newer).
func (c *listFilesCache) probeLocked(ctx context.Context, vdb *sql.DB) (int64, bool) {
	if c.conn == nil {
		conn, err := vdb.Conn(ctx)
		if err != nil {
			return 0, false
		}
		c.conn = conn
	}
	var version int64
	if err := c.conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&version); err != nil {
		// A broken probe connection must not disable the cache forever: drop it so
		// the next call re-pins, and bypass the cache for this call.
		_ = c.conn.Close()
		c.conn = nil
		c.invalidate()
		return 0, false
	}
	c.roll(version)
	return version, true
}

// roll makes version the current epoch, dropping every memo when it differs
// from the epoch they were computed in.
func (c *listFilesCache) roll(version int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.primed || version != c.version {
		c.entries = nil
		c.version = version
		c.primed = true
	}
}

// invalidate drops every memo and un-primes the epoch, so nothing is reused
// until a probe succeeds again.
func (c *listFilesCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.primed = false
}

// lookup returns the memoized entry for key, if it was computed in the epoch
// that is still current.
func (c *listFilesCache) lookup(epoch int64, key string) (listFilesCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.primed || epoch != c.version {
		return listFilesCacheEntry{}, false
	}
	entry, ok := c.entries[key]
	return entry, ok
}

// store memoizes entry under key, but only if no commit has landed since the
// begin that produced epoch. It re-probes to establish that rather than trusting
// the last epoch seen, because the commit that matters here is one that raced
// the caller's own computation: such an entry describes a newer database state
// than epoch, and memoizing it under epoch could hand a concurrent caller a
// total that does not match the rows it sees. Dropping it costs one recount.
func (c *listFilesCache) store(ctx context.Context, vdb *sql.DB, epoch int64, key string, entry listFilesCacheEntry) {
	if vdb == nil {
		return
	}
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	current, ok := c.probeLocked(ctx, vdb)
	if !ok || current != epoch {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.primed || c.version != epoch {
		return
	}
	if len(c.entries) >= listFilesCacheMaxEntries {
		c.entries = nil
	}
	if c.entries == nil {
		c.entries = make(map[string]listFilesCacheEntry, 4)
	}
	c.entries[key] = entry
}

// reset drops every memo and releases the pinned probe connection. It MUST run
// before the handle that connection came from is closed, and it leaves the cache
// usable again if the store is later reopened.
func (c *listFilesCache) reset() {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.version = 0
	c.primed = false
}

// startFor maps a page offset onto the nearest recorded boundary at or before
// it: the returned rel_path is where the SQL scan may resume, and skip is how
// many matches to discard after it before the page begins. An entry with no
// boundaries (or a non-glob entry) degrades to scanning from the first row.
func (e listFilesCacheEntry) startFor(offset int) (start string, skip int) {
	if e.stride <= 0 || len(e.bounds) == 0 || offset <= 0 {
		return "", offset
	}
	i := offset / e.stride
	if i >= len(e.bounds) {
		i = len(e.bounds) - 1
	}
	return e.bounds[i], offset - i*e.stride
}

// globBounds accumulates the sparse resume index described on
// listFilesCacheEntry while a full glob scan runs.
type globBounds struct {
	stride   int
	maxCount int
	bounds   []string
}

// newGlobBounds sizes the stride to the caller's page size so that a sequential
// walk lands exactly on recorded boundaries and skips nothing.
func newGlobBounds(pageLimit int) *globBounds {
	return &globBounds{stride: max(pageLimit, globBoundsMinStride), maxCount: globBoundsMaxCount}
}

// observe records relPath when it is the first match of a stride-sized block.
// matchIndex is the 0-based position of the match within the filtered listing.
func (b *globBounds) observe(matchIndex int64, relPath string) {
	if matchIndex%int64(b.stride) != 0 {
		return
	}
	b.bounds = append(b.bounds, relPath)
	if len(b.bounds) >= b.maxCount {
		b.compact()
	}
}

// compact halves the index in place and doubles the stride, preserving the
// invariant that bounds[i] is the match at index i*stride.
func (b *globBounds) compact() {
	kept := b.bounds[:0]
	for i := 0; i < len(b.bounds); i += 2 {
		kept = append(kept, b.bounds[i])
	}
	b.bounds = kept
	b.stride *= 2
}

// entry freezes the accumulated index into a cache entry with the given total.
func (b *globBounds) entry(total int64) listFilesCacheEntry {
	return listFilesCacheEntry{total: total, stride: b.stride, bounds: b.bounds}
}

// GlobBoundsIndexForTest builds the sparse glob resume index over matches, with
// the boundary cap lowered so the compaction path is reachable. It returns the
// final stride, the boundaries, and the (start, skip) pair the index would hand
// a page starting at probeOffset.
//
// Exported solely so the #429 F10 test can live under tests/ as AGENTS.md
// requires: the invariant worth pinning is bounds[i] == matches[i*stride] after
// any number of compactions, which production only reaches on corpora of
// millions of matching documents. Production code never calls this.
func GlobBoundsIndexForTest(pageLimit, maxCount, probeOffset int, matches []string) (stride int, bounds []string, start string, skip int) {
	b := newGlobBounds(pageLimit)
	b.maxCount = maxCount
	for i, m := range matches {
		b.observe(int64(i), m)
	}
	entry := b.entry(int64(len(matches)))
	start, skip = entry.startFor(probeOffset)
	return entry.stride, entry.bounds, start, skip
}
