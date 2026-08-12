// Package scancache provides an optional, sqlite-backed directory-discovery
// cache for the corpusfs LocalFS walker (issue #267 item 5). It persists a
// per-directory signature — the directory's own mtime plus the sorted
// name/size/mtime/mode of its direct children — so a subsequent scan of a large
// local archive can skip re-reading and re-sorting directories whose contents
// are unchanged, while still detecting any added/removed/modified file.
//
// Every timestamp is stored in NANOSECONDS (#667). Seconds were too coarse: a
// change inside the recorded second read as no change at all.
//
// The cache is purely a performance optimization: the walker only trusts a
// signature after confirming the directory's live mtime is unchanged AND
// re-stat'ing every cached child, so a stale or corrupt cache can never cause a
// changed file to be missed — at worst it triggers a full re-walk. It therefore
// stores no authoritative state and is safe to delete at any time.
//
// It reuses the same pure-Go modernc.org/sqlite driver as internal/store, so it
// introduces no new heavyweight dependency, and follows the same single-writer
// connection-pool + WAL pragmas to avoid lock contention.
package scancache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/dirstral/dir2mcp/internal/corpusfs"

	_ "modernc.org/sqlite" // pure-Go sqlite driver (shared with internal/store).
)

// SQLiteCache is a sqlite-backed corpusfs.ScanCache. It is opened lazily on the
// first Lookup/Store and is safe for the walker's sequential use within a single
// Walk. Construct it with Open and release it with Close.
type SQLiteCache struct {
	path string

	mu sync.Mutex
	db *sql.DB

	// hits/misses count fast-path outcomes for observability and tests:
	// LookupDir increments hits when it returns a stored signature and misses
	// otherwise. They are advisory metrics, not part of the cache contract.
	hits   int64
	misses int64
}

// compile-time assertion that SQLiteCache satisfies the consumer interface.
var _ corpusfs.ScanCache = (*SQLiteCache)(nil)

// Open returns a SQLiteCache backed by the sqlite file at path, creating its
// parent directory. The database file and schema are created lazily on first
// use, so Open itself performs no I/O beyond recording the path.
func Open(path string) *SQLiteCache {
	return &SQLiteCache{path: path}
}

// DefaultPath returns the conventional scan-cache location under a state dir,
// alongside the other content-addressed caches (state/cache/*).
func DefaultPath(stateDir string) string {
	return filepath.Join(stateDir, "cache", "scan.sqlite")
}

func (c *SQLiteCache) ensureDB(ctx context.Context) (*sql.DB, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db != nil {
		return c.db, nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return nil, fmt.Errorf("scancache: create cache directory: %w", err)
	}
	db, err := sql.Open("sqlite", c.path)
	if err != nil {
		return nil, fmt.Errorf("scancache: open db: %w", err)
	}
	// Serialize through a single connection and enable WAL, mirroring
	// internal/store so concurrent processes do not trip SQLITE_BUSY. Order
	// matters: busy_timeout must precede the WAL switch.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		`PRAGMA busy_timeout=5000;`,
		`PRAGMA journal_mode=WAL;`,
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("scancache: pragma: %w", err)
		}
	}
	// The table is versioned in its NAME and the previous version is dropped
	// (#667). Timestamps moved from seconds to nanoseconds, so a row written by an
	// older build states a number this build reads as a different instant. Such a
	// row is harmless (the walker compares it, sees a mismatch, and re-reads the
	// directory), but keeping it wastes a lookup and a row per directory forever.
	// The cache holds no authoritative state and is safe to delete at any time, so
	// dropping is the whole migration.
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS dir_signatures;`,
		`CREATE TABLE IF NOT EXISTS dir_signatures_v2 (
  rel_dir             TEXT PRIMARY KEY,
  dir_mtime_unix_nano INTEGER NOT NULL,
  entries_json        TEXT NOT NULL
);`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("scancache: init schema: %w", err)
		}
	}
	c.db = db
	return db, nil
}

// LookupDir returns the cached signature for relDir. A missing row, a database
// error, or unparseable JSON all surface as a miss (ok=false, err=nil) so the
// walker simply falls back to a full directory read.
func (c *SQLiteCache) LookupDir(relDir string) (corpusfs.CachedDirSignature, bool, error) {
	ctx := context.Background()
	db, err := c.ensureDB(ctx)
	if err != nil {
		c.recordMiss()
		return corpusfs.CachedDirSignature{}, false, nil //nolint:nilerr // a cache error is a miss
	}

	var mtimeNano int64
	var entriesJSON string
	row := db.QueryRowContext(ctx,
		`SELECT dir_mtime_unix_nano, entries_json FROM dir_signatures_v2 WHERE rel_dir = ?`, relDir)
	if err := row.Scan(&mtimeNano, &entriesJSON); err != nil {
		c.recordMiss()
		return corpusfs.CachedDirSignature{}, false, nil //nolint:nilerr // no row / scan error -> miss
	}

	var entries []corpusfs.CachedDirEntry
	if entriesJSON != "" {
		if err := json.Unmarshal([]byte(entriesJSON), &entries); err != nil {
			c.recordMiss()
			return corpusfs.CachedDirSignature{}, false, nil //nolint:nilerr // corrupt row -> miss
		}
	}
	c.recordHit()
	return corpusfs.CachedDirSignature{DirMTimeUnixNano: mtimeNano, Entries: entries}, true, nil
}

// StoreDir upserts the signature for relDir.
func (c *SQLiteCache) StoreDir(relDir string, sig corpusfs.CachedDirSignature) error {
	ctx := context.Background()
	db, err := c.ensureDB(ctx)
	if err != nil {
		return err
	}
	entriesJSON, err := json.Marshal(sig.Entries)
	if err != nil {
		return fmt.Errorf("scancache: marshal entries: %w", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO dir_signatures_v2 (rel_dir, dir_mtime_unix_nano, entries_json)
		 VALUES (?, ?, ?)
		 ON CONFLICT(rel_dir) DO UPDATE SET
		   dir_mtime_unix_nano = excluded.dir_mtime_unix_nano,
		   entries_json        = excluded.entries_json`,
		relDir, sig.DirMTimeUnixNano, string(entriesJSON))
	if err != nil {
		return fmt.Errorf("scancache: store signature: %w", err)
	}
	return nil
}

// Close releases the underlying database handle. It is safe to call more than
// once and on a cache that was never used.
func (c *SQLiteCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}

// Stats returns the number of cache hits and misses observed by LookupDir since
// the cache was opened. Intended for observability and tests.
func (c *SQLiteCache) Stats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

func (c *SQLiteCache) recordHit() {
	c.mu.Lock()
	c.hits++
	c.mu.Unlock()
}

func (c *SQLiteCache) recordMiss() {
	c.mu.Lock()
	c.misses++
	c.mu.Unlock()
}
