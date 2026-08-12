// Package diskindex implements the Tier-B on-disk vector backend (issue #246).
//
// It satisfies the merged pluggable model.Index contract (plus the optional
// Persistable and FilteringIndex capabilities) exactly like the in-memory HNSW
// reference (internal/index.HNSWIndex), but removes the all-vectors-in-RAM
// ceiling: the navigational metadata (one fixed-size locator per chunk) stays
// in a Go map, while the dense vector payloads live in a single on-disk segment
// file that is memory-mapped and read on demand at search time.
//
// The whole point of Tier B is "no C extension": the implementation is pure Go.
// It uses golang.org/x/exp/mmap (a pure-Go ReaderAt over a mmap'd file on
// supported platforms, falling back to read syscalls otherwise) — there is no
// cgo. The package therefore builds and runs with CGO_ENABLED=0.
//
// On-disk format (segment file, see writeRecord / scanSegment):
//
//	[8]  magic   "D2MDISK1"
//	[4]  version uint32 (=1)
//	... then a stream of records, each:
//	[8]  chunkID    uint64 (little-endian)
//	[1]  tombstone  byte   (0 = live, 1 = deleted)
//	[4]  vecLen     uint32 (number of float32 elements)
//	[4]  payloadLen uint32 (gob-encoded model.IndexPayload byte length)
//	[vecLen*4] vector  float32 little-endian
//	[payloadLen] payload gob bytes
//
// Mutations append a new record (Upsert) or a tombstone record (Delete); the
// in-RAM locator map always points at the latest record for a chunk so the
// effective state is the last-writer-wins fold over the log. Save() compacts the
// live set into a fresh segment via atomic temp-file rename, reclaiming the
// space held by superseded/tombstoned records. The identity (SPEC 8.1.4) is
// stored in a sibling JSON file so it survives reopen without parsing vectors.
package diskindex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/exp/mmap"

	"github.com/dirstral/dir2mcp/internal/index/topk"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/statefs"
)

const (
	segmentMagic   = "D2MDISK1"
	segmentVersion = uint32(1)
	headerLen      = len(segmentMagic) + 4 // magic + version
	recordHdrLen   = 8 + 1 + 4 + 4         // chunkID + tombstone + vecLen + payloadLen
)

// SegmentFileName is the on-disk segment basename for an index "kind"
// (text/code). It mirrors index.TextIndexFileName / CodeIndexFileName but for
// the disk backend, and is deliberately distinct so the two backends never
// alias each other's files.
func SegmentFileName(kind string) string {
	return "vectors_" + kind + ".diskv1.idx"
}

// IdentitySidecarSuffix is appended to a segment path to locate the JSON
// sidecar that stores the recorded embed identity. Exported so reindex cleanup
// (internal/index.StaleIndexFiles) can name the sidecar without hardcoding it.
const IdentitySidecarSuffix = ".identity.json"

// identitySidecarPath returns the path of the JSON sidecar holding the recorded
// embed identity for the given segment path.
func identitySidecarPath(segmentPath string) string {
	return segmentPath + IdentitySidecarSuffix
}

// locator records where a chunk's latest record lives in the segment file plus
// its fixed-size shape, so the vector/payload can be read on demand without
// holding them in RAM. This is the only per-vector state kept resident.
type locator struct {
	offset     int64
	vecLen     uint32
	payloadLen uint32
}

// DiskIndex is the on-disk backend. Only the locator map and identity are
// resident; vectors/payloads are read from the mmap'd segment on demand.
type DiskIndex struct {
	path string

	mu        sync.RWMutex
	locators  map[uint64]locator
	identity  string
	appendEnd int64          // current end-of-file offset for the append log
	reader    *mmap.ReaderAt // mmap view of the segment, nil until first read/Load

	// version is a monotonically increasing mutation counter bumped under the
	// write lock on every appended record (Upsert/Delete) and Reset. savedVersion
	// records the version captured by the last successful Save; when they are equal
	// the on-disk segment already reflects the in-memory state, so the periodic
	// autosave skips the full compaction rewrite (issue #429 F7). All the fields
	// below are guarded by mu.
	version      uint64
	savedVersion uint64

	// autosaveThreshold / autosaveMaxInterval / lastSaveTime throttle the periodic
	// autosave exactly as the memory backend does (issue #429 C-a): AutosaveTick
	// compacts only once threshold mutations have accumulated or maxInterval has
	// elapsed; the force path (Save) always compacts when dirty.
	autosaveThreshold   uint64
	autosaveMaxInterval time.Duration
	lastSaveTime        time.Time
}

// compile-time assertions: DiskIndex satisfies the core contract and the
// optional capabilities (issue #246/#247/#429 F8).
var (
	_ model.Index          = (*DiskIndex)(nil)
	_ model.Persistable    = (*DiskIndex)(nil)
	_ model.FilteringIndex = (*DiskIndex)(nil)
	_ model.BatchUpserter  = (*DiskIndex)(nil)
)

// SetSyncHookForTest replaces the fsync used as the durability barrier and
// returns a function restoring the previous one.
//
// It is exported solely so the batch tests can count fsyncs from tests/ rather
// than in-package: AGENTS.md requires new test files to live under tests/, and
// counting durability barriers is the only way to pin "one fsync per batch
// instead of one per record" (#429 F8) as an observable property. Production
// code never calls this; the zero state is a real f.Sync().
func SetSyncHookForTest(fn func(*os.File) error) (restore func()) {
	prev := syncFile
	syncFile = fn
	return func() { syncFile = prev }
}

// syncFile fsyncs f. It is a package-level var solely so SetSyncHookForTest can
// swap it; every production path leaves it at the real f.Sync() below.
var syncFile = func(f *os.File) error { return f.Sync() }

// New creates an empty disk index. path is the segment file used by Save/Load
// and the append log; it may be empty for a purely in-process instance, in
// which case Save/Load require an explicit path argument.
func New(path string) *DiskIndex {
	return &DiskIndex{
		path:                path,
		locators:            make(map[uint64]locator),
		autosaveThreshold:   defaultAutosaveThreshold,
		autosaveMaxInterval: defaultAutosaveMaxInterval,
		lastSaveTime:        time.Now(),
	}
}

// defaultAutosaveThreshold / defaultAutosaveMaxInterval mirror
// index.DefaultAutosaveThreshold / DefaultAutosaveMaxInterval; they are redefined
// here (rather than imported) because internal/index imports this package, so the
// dependency cannot run the other way (issue #429 C-a).
const (
	defaultAutosaveThreshold   = 50000
	defaultAutosaveMaxInterval = 5 * time.Minute
)

// SetAutosavePolicy overrides the periodic-autosave throttle (issue #429 C-a),
// mirroring HNSWIndex.SetAutosavePolicy: a tick compacts only once threshold
// mutations have accumulated or maxInterval has elapsed since the last save.
// threshold==0 disables delta-gating; maxInterval<=0 disables the time fallback.
// The force path (Save/StopAndSave) always compacts when dirty.
func (d *DiskIndex) SetAutosavePolicy(threshold uint64, maxInterval time.Duration) {
	d.mu.Lock()
	d.autosaveThreshold = threshold
	d.autosaveMaxInterval = maxInterval
	d.mu.Unlock()
}

// Upsert appends the vector+payload as the newest record and points the locator
// at it. An empty vector or zero chunk_id is an error (matching HNSWIndex).
//
// It shares the append path with BatchUpsert, so it shares the crash contract: a
// write or fsync failure truncates the segment back to its pre-append length and
// leaves the locators untouched (#674).
func (d *DiskIndex) Upsert(ctx context.Context, vector []float32, payload model.IndexPayload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	recs, err := stageUpserts([]model.IndexUpsert{{Vector: vector, Payload: payload}})
	if err != nil {
		return err
	}

	// d.path is read under the lock, like the batch path: Load assigns it under
	// d.mu, so an unlocked read here races a concurrent reopen.
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.path == "" {
		return errors.New("disk index requires a path")
	}
	return d.appendRecords(recs)
}

// BatchUpsert appends every item to the segment through ONE file open, ONE
// buffered write pass and ONE fsync (issue #429 F8), instead of the open +
// write + fsync + close that Upsert pays per record. Ingest upserts one vector
// per chunk, so the per-record barrier capped disk-backend indexing at the
// fsync rate; batching moves that cap to once per embed batch.
//
// Durability is honestly per BATCH, not per item: when this returns nil every
// item is on stable storage, but a crash part-way through loses the whole batch
// (the caller's chunks simply stay pending and are re-embedded; see
// EmbeddingWorker.indexChunks, which only marks chunks embedded after this
// returns). Nothing is ever silently half-applied: a write or fsync failure
// truncates the segment back to its pre-batch length so the log can never be
// scanned as a mix of applied and torn records, and the in-memory locator map
// is only advanced after the barrier succeeds.
func (d *DiskIndex) BatchUpsert(ctx context.Context, items []model.IndexUpsert) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	// Item validation and payload encoding stay ahead of the lock so a malformed
	// batch is rejected without touching shared state. Reading d.path must stay
	// behind the lock: Load assigns it under d.mu, so an unlocked read here
	// races a concurrent reopen.
	recs, err := stageUpserts(items)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.path == "" {
		return errors.New("disk index requires a path")
	}
	return d.appendRecords(recs)
}

// pendingRecord is one record staged for the append log: an upsert (tombstone
// false, carrying the vector and the encoded payload) or a delete (tombstone
// true). Every mutation is staged in full before the segment file is opened, so
// a validation or encoding failure can never leave bytes behind.
type pendingRecord struct {
	chunkID      uint64
	tombstone    bool
	vector       []float32
	payloadBytes []byte
}

// stageUpserts validates and encodes every item. It rejects the same two cases
// as Upsert, and it does so for the whole batch before any record is written.
func stageUpserts(items []model.IndexUpsert) ([]pendingRecord, error) {
	recs := make([]pendingRecord, len(items))
	for i := range items {
		if len(items[i].Vector) == 0 {
			return nil, errors.New("vector cannot be empty")
		}
		if items[i].Payload.ChunkID == 0 {
			return nil, errors.New("payload chunk_id cannot be zero")
		}
		payloadBytes, err := encodePayload(items[i].Payload)
		if err != nil {
			return nil, err
		}
		recs[i] = pendingRecord{
			chunkID:      items[i].Payload.ChunkID,
			vector:       items[i].Vector,
			payloadBytes: payloadBytes,
		}
	}
	return recs, nil
}

// appendRecords writes every staged record through ONE file open, ONE buffered
// write pass and ONE fsync, and then publishes the result in memory.
//
// Every mutation (Upsert, BatchUpsert, Delete) goes through here, so every
// mutation has the same crash contract: on a write or fsync failure the segment
// is truncated back to its pre-append length, and neither the locators nor the
// append pointer move. The single-record path used to return on failure without
// the truncation, which left abandoned bytes past d.appendEnd for a later append
// to overwrite in part (#674). Caller holds the write lock.
func (d *DiskIndex) appendRecords(recs []pendingRecord) error {
	f, err := statefs.OpenFile(d.path, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	if err := d.ensureAppendEnd(f); err != nil {
		return err
	}
	if _, err := f.Seek(d.appendEnd, io.SeekStart); err != nil {
		return err
	}

	start := d.appendEnd
	offsets, end, werr := writeRecords(f, start, recs)
	if werr == nil {
		// The single durability barrier for the whole mutation.
		werr = syncFile(f)
	}
	if werr != nil {
		return d.rollbackAppend(f, start, werr)
	}

	// Published only now that the bytes are durable. Applying in slice order
	// makes a repeated chunk ID resolve last-writer-wins, exactly as a sequence
	// of single mutations would.
	d.applyRecords(recs, offsets)
	d.appendEnd = end
	// One mutation per record keeps the autosave delta accounting identical to
	// the old per-record path.
	d.version += uint64(len(recs))
	// The mmap view is now stale (file grew); drop it so the next read re-maps.
	return d.invalidateReaderLocked()
}

// applyRecords points the locator map at the records just made durable: an
// upsert publishes its locator, a tombstone drops the entry. offsets is parallel
// to recs. Caller holds the write lock.
func (d *DiskIndex) applyRecords(recs []pendingRecord, offsets []int64) {
	for i := range recs {
		if recs[i].tombstone {
			delete(d.locators, recs[i].chunkID)
			continue
		}
		d.locators[recs[i].chunkID] = locator{
			offset:     offsets[i],
			vecLen:     uint32(len(recs[i].vector)),
			payloadLen: uint32(len(recs[i].payloadBytes)),
		}
	}
}

// writeRecords writes every staged record into w through a single buffered
// writer, with the first record landing at offset start. It returns one offset
// per record (parallel to recs) and the new end offset. It does not fsync:
// appendRecords owns the mutation's single durability barrier.
func writeRecords(w io.Writer, start int64, recs []pendingRecord) ([]int64, int64, error) {
	bw := bufio.NewWriter(w)
	offsets := make([]int64, len(recs))
	offset := start
	for i := range recs {
		n, err := writeRecord(bw, recs[i].chunkID, recs[i].tombstone, recs[i].vector, recs[i].payloadBytes)
		if err != nil {
			return nil, 0, err
		}
		offsets[i] = offset
		offset += int64(n)
	}
	if err := bw.Flush(); err != nil {
		return nil, 0, err
	}
	return offsets, offset, nil
}

// rollbackAppend truncates the segment back to its pre-append length after a
// failed mutation and returns cause (joined with any rollback failure).
// d.locators and d.appendEnd are never advanced before the fsync succeeds, so
// after the truncation the in-memory state already matches the file again; only
// the mmap view has to be dropped. Caller holds the write lock.
func (d *DiskIndex) rollbackAppend(f *os.File, start int64, cause error) error {
	if terr := f.Truncate(start); terr != nil {
		return errors.Join(cause, fmt.Errorf("diskindex: rollback truncate to %d: %w", start, terr))
	}
	if serr := syncFile(f); serr != nil {
		return errors.Join(cause, fmt.Errorf("diskindex: rollback sync: %w", serr))
	}
	if ierr := d.invalidateReaderLocked(); ierr != nil {
		return errors.Join(cause, ierr)
	}
	return cause
}

// Delete writes a tombstone record for each id and drops the locator. Unknown
// ids are ignored. Tombstones keep the on-disk log append-only and consistent;
// space is reclaimed on the next Save (compaction).
//
// A multi-id delete is ALL or NOTHING. Every tombstone goes into one write pass
// behind one durability barrier, so a failure removes no id at all. The old
// per-id loop made each id durable on its own, so an error on the third id left
// the first two deleted and reported failure for the whole call (#674).
func (d *DiskIndex) Delete(ctx context.Context, chunkIDs []uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunkIDs) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	recs, err := d.stageTombstones(chunkIDs)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return nil
	}
	return d.appendRecords(recs)
}

// stageTombstones builds one tombstone record per id that the index actually
// holds. Unknown ids are skipped, and a repeated id is staged once: the old loop
// wrote a single tombstone for a repeat too, because the second pass found the
// locator already dropped. A tombstone carries the encoded zero payload, exactly
// as the per-record path always wrote it, so the segment bytes are unchanged.
// Caller holds the write lock.
func (d *DiskIndex) stageTombstones(chunkIDs []uint64) ([]pendingRecord, error) {
	// Every tombstone carries the same encoded zero payload, so encode it once.
	payloadBytes, err := encodePayload(model.IndexPayload{})
	if err != nil {
		return nil, err
	}
	recs := make([]pendingRecord, 0, len(chunkIDs))
	staged := make(map[uint64]struct{}, len(chunkIDs))
	for _, id := range chunkIDs {
		if _, known := d.locators[id]; !known {
			continue
		}
		if _, dup := staged[id]; dup {
			continue
		}
		staged[id] = struct{}{}
		recs = append(recs, pendingRecord{chunkID: id, tombstone: true, payloadBytes: payloadBytes})
	}
	return recs, nil
}

// ensureAppendEnd initialises d.appendEnd from the open file, writing the header
// on a fresh/empty file. Caller holds the write lock.
// syncDir fsyncs the directory holding path so a newly created file's directory
// entry is durable. Syncing the file alone leaves a window where the contents
// are on stable storage but the name is not, so a crash can lose a segment the
// caller was told was durable. Best-effort: platforms that cannot open a
// directory for read report no error rather than failing an otherwise good
// write.
func syncDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

func (d *DiskIndex) ensureAppendEnd(f *os.File) error {
	if d.appendEnd != 0 {
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		if err := writeHeader(f); err != nil {
			return err
		}
		// The segment file was just created; persist its directory entry too, so
		// the durability the batch barrier promises covers the file's existence
		// and not only its contents.
		if err := syncDir(d.path); err != nil {
			return err
		}
		d.appendEnd = int64(headerLen)
		return nil
	}
	d.appendEnd = info.Size()
	return nil
}

// Search reads each candidate vector on demand from the mmap'd segment, scores
// it by cosine similarity, and returns the k best (best-first). The filter is
// applied over the on-disk payload via model.Filter.Match, so CanFilter is true.
func (d *DiskIndex) Search(ctx context.Context, vector []float32, k int, filter model.Filter) ([]model.IndexHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(vector) == 0 {
		return nil, errors.New("query vector cannot be empty")
	}
	if k <= 0 {
		return []model.IndexHit{}, nil
	}

	// Lazily (re)open the mmap view under the write lock so the assignment to
	// d.reader cannot race with a concurrent Search; the subsequent scan holds
	// the read lock, during which no mutation can invalidate the view.
	if err := d.ensureReader(); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	reader := d.reader
	if reader == nil {
		// A concurrent mutation invalidated the view (or the segment is empty);
		// with nothing mapped there is nothing to score.
		return []model.IndexHit{}, nil
	}

	cands, err := d.scoreTopK(reader, vector, k, filter)
	if err != nil {
		return nil, err
	}
	sort.Slice(cands, func(a, b int) bool {
		return topk.Before(cands[a].Score, cands[b].Score, cands[a].ChunkID, cands[b].ChunkID)
	})
	hits := make([]model.IndexHit, len(cands))
	for i, c := range cands {
		hits[i] = model.IndexHit{ChunkID: c.ChunkID, Score: c.Score, Payload: c.Payload}
	}
	return hits, nil
}

// scoreTopK scans every dimension-matching locator, scoring each candidate into a
// bounded top-k heap, and returns the (unordered) retained best k. Caller holds
// the read lock and passes the live mmap reader. A single query-sized byte buffer
// + float32 slice is reused across candidates, so the whole scan's vector-read
// allocation is O(1) rather than one []byte+[]float32 per candidate (issue #429
// F2); the scratch is never retained past a score, so reuse is safe.
func (d *DiskIndex) scoreTopK(reader *mmap.ReaderAt, vector []float32, k int, filter model.Filter) ([]topk.Candidate, error) {
	applyFilter := !filter.IsZero()
	h := topk.New(k)
	vecBytes := make([]byte, len(vector)*4)
	vecScratch := make([]float32, len(vector))
	for id, loc := range d.locators {
		if int(loc.vecLen) != len(vector) {
			continue // dimension mismatch; skip like HNSW does
		}
		if err := scoreCandidate(reader, id, loc, vector, vecBytes, vecScratch, applyFilter, filter, h); err != nil {
			return nil, err
		}
	}
	return h.Items(), nil
}

// scoreCandidate reads, filters, scores, and conditionally admits one candidate
// to the heap. When filtering, the payload is decoded first so the predicate can
// reject the candidate before it is scored; otherwise the (expensive gob) payload
// decode is deferred and paid only for candidates that survive the heap guard —
// the disk analogue of the memory backend's in-place scoring (issue #429 F1/F2).
func scoreCandidate(reader *mmap.ReaderAt, id uint64, loc locator, vector []float32, vecBytes []byte, vecScratch []float32, applyFilter bool, filter model.Filter, h *topk.Heap) error {
	var payload model.IndexPayload
	havePayload := false
	if applyFilter {
		p, err := readPayloadAt(reader, loc)
		if err != nil {
			return err
		}
		if !filter.Match(p) {
			return nil
		}
		payload, havePayload = p, true
	}
	if _, err := reader.ReadAt(vecBytes, loc.offset+int64(recordHdrLen)); err != nil {
		return err
	}
	fillFloat32s(vecScratch, vecBytes)
	score := cosineSimilarity(vector, vecScratch)
	if h.Full() && !h.Better(score, id) {
		return nil
	}
	if !havePayload {
		p, err := readPayloadAt(reader, loc)
		if err != nil {
			return err
		}
		payload = p
	}
	h.Push(topk.Candidate{ChunkID: id, Score: score, Payload: payload})
	return nil
}

// CanFilter reports that the backend evaluates filters itself: Search applies
// model.Filter.Match over the on-disk payloads, so retrieval may push the
// filter down rather than overfetch-then-filter.
func (d *DiskIndex) CanFilter(filter model.Filter) bool {
	return true
}

// Identity returns the recorded corpus-lifetime embed identity, or "" when
// fresh.
func (d *DiskIndex) Identity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.identity, nil
}

// Reset discards all vectors/payloads (truncating the segment back to a fresh
// header) and records identity as the new corpus-lifetime embed identity.
func (d *DiskIndex) Reset(ctx context.Context, identity string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.invalidateReaderLocked(); err != nil {
		return err
	}
	d.locators = make(map[uint64]locator)
	d.identity = identity
	d.appendEnd = 0
	d.version++

	if d.path == "" {
		return nil
	}
	// Stage the new identity BEFORE destroying the segment, and publish it
	// after. Whichever step fails, what is on disk stays a matched pair: a
	// staging failure leaves the old segment with its old identity, and a
	// publish failure leaves an empty segment with the old identity, which the
	// next EnsureIdentity resets again onto an already-empty segment. The
	// previous order could truncate and then fail, leaving live-looking vectors
	// claimed by an identity that no longer described them (#728).
	sidecarPath := identitySidecarPath(d.path)
	staged, err := stageIdentitySidecar(sidecarPath, identity)
	if err != nil {
		return err
	}
	if err := truncateSegment(d.path); err != nil {
		_ = os.Remove(staged)
		return err
	}
	d.appendEnd = int64(headerLen)
	return publishIdentitySidecar(staged, sidecarPath)
}

// truncateSegment rewrites path with just a fresh header.
func truncateSegment(path string) error {
	f, err := statefs.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if err := writeHeader(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Save compacts the live records into a fresh segment file at path (atomic
// temp-file rename), reclaiming space held by superseded/tombstoned records,
// and writes the identity sidecar. Vectors are streamed from the current mmap'd
// segment to the new file without ever holding the whole corpus in RAM.
func (d *DiskIndex) Save(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if path == "" {
		path = d.path
	}
	if path == "" {
		return errors.New("path is required")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// When nothing has changed since the last durable save, the on-disk segment
	// already reflects our state: skip the whole-segment compaction rewrite (no
	// temp file, no fsync, no rename) so an idle corpus's autosave is a no-op
	// (issue #429 F7). Save holds the write lock throughout, so no mutation can
	// race between this check and advancing savedVersion below.
	if d.version == d.savedVersion {
		return nil
	}
	captured := d.version

	reader, err := d.readerLocked()
	if err != nil {
		return err
	}

	tmpPath := path + ".tmp"
	out, err := statefs.Create(tmpPath)
	if err != nil {
		return err
	}
	newLocators, newEnd, err := d.compactInto(out, reader)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := finalizeFile(out, tmpPath, path); err != nil {
		return err
	}

	// Point ourselves at the freshly written, compacted segment.
	d.path = path
	d.locators = newLocators
	d.appendEnd = newEnd
	if err := d.invalidateReaderLocked(); err != nil {
		return err
	}
	// The identity sidecar is part of this commit, not a postscript to it. A
	// segment without its identity is unreadable on the next startup: Load maps
	// a missing sidecar to "", EnsureIdentity reads that as a mismatch, and
	// Reset discards every vector. So savedVersion must not advance until the
	// sidecar is durable, or a failure here would mark the index clean and the
	// `version == savedVersion` gate would ensure no later Save ever retried it
	// (#728).
	if err := writeIdentitySidecar(identitySidecarPath(path), d.identity); err != nil {
		return err
	}
	d.savedVersion = captured
	d.lastSaveTime = time.Now()
	return nil
}

// AutosaveTick is the periodic-save entrypoint (issue #429 C-a): it compacts via
// Save only when the index is dirty AND either the accumulated mutation delta has
// reached autosaveThreshold or autosaveMaxInterval has elapsed since the last
// save. Otherwise it is a no-op, so a long ingest no longer recompacts the whole
// segment on every 15s tick. StopAndSave still calls the force path (Save), so
// the throttled tail is always persisted at shutdown.
func (d *DiskIndex) AutosaveTick(ctx context.Context, path string) error {
	d.mu.RLock()
	delta := d.version - d.savedVersion
	threshold := d.autosaveThreshold
	maxInterval := d.autosaveMaxInterval
	sinceSave := time.Since(d.lastSaveTime)
	d.mu.RUnlock()

	if !shouldAutosave(delta, threshold, maxInterval, sinceSave) {
		return nil
	}
	return d.Save(ctx, path)
}

// shouldAutosave decides whether a periodic autosave tick should compact now
// (issue #429 C-a). It mirrors index.shouldAutosave exactly (the two backends
// cannot share a helper without an import cycle): never save a clean index; save
// immediately once delta reaches the threshold; below the threshold save only
// after maxInterval has elapsed. A zero threshold saves on any dirty tick; a
// non-positive maxInterval disables the time-based fallback.
func shouldAutosave(delta, threshold uint64, maxInterval, sinceSave time.Duration) bool {
	if delta == 0 {
		return false
	}
	if threshold == 0 || delta >= threshold {
		return true
	}
	return maxInterval > 0 && sinceSave >= maxInterval
}

// compactInto writes the header and every live record to out (buffered),
// returning the rebuilt locator map and end offset. Caller holds the lock.
func (d *DiskIndex) compactInto(out *os.File, reader *mmap.ReaderAt) (map[uint64]locator, int64, error) {
	w := bufio.NewWriter(out)
	if err := writeHeader(w); err != nil {
		return nil, 0, err
	}
	offset := int64(headerLen)
	newLocators := make(map[uint64]locator, len(d.locators))

	// Deterministic order keeps the output byte-stable for a given live set.
	ids := make([]uint64, 0, len(d.locators))
	for id := range d.locators {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })

	for _, id := range ids {
		loc := d.locators[id]
		vec, payload, rerr := readRecordAt(reader, loc)
		if rerr != nil {
			return nil, 0, rerr
		}
		payloadBytes, err := encodePayload(payload)
		if err != nil {
			return nil, 0, err
		}
		n, err := writeRecord(w, id, false, vec, payloadBytes)
		if err != nil {
			return nil, 0, err
		}
		newLocators[id] = locator{offset: offset, vecLen: loc.vecLen, payloadLen: uint32(len(payloadBytes))}
		offset += int64(n)
	}
	if err := w.Flush(); err != nil {
		return nil, 0, err
	}
	return newLocators, offset, nil
}

// finalizeFile fsyncs, closes, and atomically renames tmpPath to path.
func finalizeFile(out *os.File, tmpPath, path string) error {
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	// The file's own contents are synced above; the rename that makes them
	// visible under `path` needs the directory synced too (#728).
	return syncDir(path)
}

// Load reopens the segment at path: it rebuilds the locator map by scanning
// record headers (without retaining vectors) and reads the identity sidecar. A
// missing segment is treated as a fresh index (no error), matching HNSWIndex.
func (d *DiskIndex) Load(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if path == "" {
		path = d.path
	}
	if path == "" {
		return errors.New("path is required")
	}

	locators, end, torn, scanErr := scanSegment(path)
	if scanErr != nil {
		if errors.Is(scanErr, os.ErrNotExist) {
			// No segment at all. A sidecar that is damaged here has nothing to
			// protect, so it is not worth failing startup over.
			identity, _ := readIdentitySidecar(identitySidecarPath(path))
			d.mu.Lock()
			d.path = path
			d.identity = identity
			d.mu.Unlock()
			return nil
		}
		return scanErr
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// Drop a torn tail left by an ungraceful crash mid-append/mid-batch before
	// anything appends after it, otherwise the garbage bytes would sit in the
	// middle of the log and make every later scan stop short of the new records.
	//
	// This runs INSIDE the critical section: truncating outside it lets a
	// concurrent durable append land past end between the scan and the truncate,
	// and the truncate would then silently destroy it.
	if torn {
		if err := os.Truncate(path, end); err != nil {
			return fmt.Errorf("diskindex: truncate torn segment tail at %d: %w", end, err)
		}
	}
	if err := d.invalidateReaderLocked(); err != nil {
		return err
	}
	identity, idErr := readIdentitySidecar(identitySidecarPath(path))
	if idErr != nil {
		// A sidecar that EXISTS and cannot be understood. This used to read as
		// "" exactly like a missing one, so EnsureIdentity saw a mismatch and
		// Reset discarded every vector the segment still held (#728).
		//
		// Only the damaged case fails here. A missing sidecar over a populated
		// segment is NOT damage: appends are durable immediately while the
		// sidecar is only written by Save and Reset, so every index between its
		// first append and its first save is legitimately in that state. I had
		// this wrong and the existing suite caught it.
		//
		// Reported rather than repaired: adopting the configured identity would
		// silently pair vectors with an embedding space that may not be theirs,
		// which is worse than saying so. `reindex` is the deliberate recovery.
		return fmt.Errorf(
			"diskindex: segment %s holds %d vectors but its recorded embed identity is damaged (%w); "+
				"run `dir2mcp reindex` to rebuild it deliberately rather than losing it silently",
			path, len(locators), idErr,
		)
	}
	d.path = path
	d.locators = locators
	d.appendEnd = end
	d.identity = identity
	return nil
}

// Close releases the mmap view.
func (d *DiskIndex) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.invalidateReaderLocked()
}

// ensureReader opens the mmap view if it is not already cached, taking the
// write lock so the assignment to d.reader is exclusive (Search holds only the
// read lock for its scan and must not write d.reader concurrently). A missing
// segment file (fresh index) is not an error; d.reader simply stays nil.
func (d *DiskIndex) ensureReader() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.readerLocked()
	return err
}

// readerLocked returns the mmap reader, opening it lazily. Returns (nil, nil)
// when no segment file exists yet (fresh index). The cached view is invalidated
// on every mutation (appendRecords/Reset/Save), so a held view always reflects
// the on-disk bytes the current locator offsets point at. Callers must hold the
// write lock (it may assign d.reader).
func (d *DiskIndex) readerLocked() (*mmap.ReaderAt, error) {
	if d.reader != nil {
		return d.reader, nil
	}
	if d.path == "" {
		return nil, nil
	}
	r, err := mmap.Open(d.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	d.reader = r
	return r, nil
}

// invalidateReaderLocked closes and clears any cached mmap view. Caller holds
// the write lock (or is otherwise exclusive).
func (d *DiskIndex) invalidateReaderLocked() error {
	if d.reader == nil {
		return nil
	}
	err := d.reader.Close()
	d.reader = nil
	return err
}

// --- record / segment encoding ---

// writeHeader writes the magic + version prefix.
func writeHeader(w io.Writer) error {
	if _, err := w.Write([]byte(segmentMagic)); err != nil {
		return err
	}
	var v [4]byte
	binary.LittleEndian.PutUint32(v[:], segmentVersion)
	_, err := w.Write(v[:])
	return err
}

// writeRecord encodes one record to w and returns the number of bytes written.
func writeRecord(w io.Writer, chunkID uint64, tombstone bool, vector []float32, payloadBytes []byte) (int, error) {
	hdr := make([]byte, recordHdrLen)
	binary.LittleEndian.PutUint64(hdr[0:8], chunkID)
	if tombstone {
		hdr[8] = 1
	}
	binary.LittleEndian.PutUint32(hdr[9:13], uint32(len(vector)))
	binary.LittleEndian.PutUint32(hdr[13:17], uint32(len(payloadBytes)))
	if _, err := w.Write(hdr); err != nil {
		return 0, err
	}
	vecBytes := float32sToBytes(vector)
	if _, err := w.Write(vecBytes); err != nil {
		return 0, err
	}
	if _, err := w.Write(payloadBytes); err != nil {
		return 0, err
	}
	return recordHdrLen + len(vecBytes) + len(payloadBytes), nil
}

// readPayloadAt reads and gob-decodes only the payload for a locator, without
// touching the vector bytes. Search uses it for lazy payload materialisation:
// the payload is decoded only for candidates that survive the top-k heap guard
// (or up front when a filter must inspect it), not for every scanned record.
func readPayloadAt(reader *mmap.ReaderAt, loc locator) (model.IndexPayload, error) {
	vecBytesLen := int(loc.vecLen) * 4
	buf := make([]byte, int(loc.payloadLen))
	// The payload follows the record header and the vector bytes.
	if _, err := reader.ReadAt(buf, loc.offset+int64(recordHdrLen)+int64(vecBytesLen)); err != nil {
		return model.IndexPayload{}, err
	}
	return decodePayload(buf)
}

// readRecordAt reads the vector and payload for a locator from the mmap reader.
func readRecordAt(reader *mmap.ReaderAt, loc locator) ([]float32, model.IndexPayload, error) {
	vecBytesLen := int(loc.vecLen) * 4
	buf := make([]byte, vecBytesLen+int(loc.payloadLen))
	// The vector/payload start right after the record header.
	if _, err := reader.ReadAt(buf, loc.offset+int64(recordHdrLen)); err != nil {
		return nil, model.IndexPayload{}, err
	}
	vec := bytesToFloat32s(buf[:vecBytesLen])
	payload, err := decodePayload(buf[vecBytesLen:])
	if err != nil {
		return nil, model.IndexPayload{}, err
	}
	return vec, payload, nil
}

// scanSegment reads the segment header + record headers to rebuild the locator
// map (last-writer-wins, tombstones drop entries) without retaining vectors. It
// returns the locator map, the offset just past the last COMPLETE record, and
// whether the segment ended in a torn (partially written) record.
//
// A torn tail is not an error: the segment is an append log whose durability
// barrier is per write (Upsert) or per batch (BatchUpsert, issue #429 F8), so an
// ungraceful crash can leave a prefix of an unsynced batch in the file. Those
// records belong to chunks the embed worker never marked embedded (it marks
// only after the barrier returns), so dropping them loses nothing the metadata
// store believes is indexed; the chunks are simply still pending and get
// re-embedded. Erroring instead would make one torn record poison the whole
// segment on load.
// maxRecordBodyLen bounds a single record's body when scanning. Real records are
// a vector plus a small payload (a 3072-dimension vector is about 12 KB), so 1
// GiB is far above anything legitimate and far below the ~21.5 GB a corrupt
// uint32 pair can encode.
const maxRecordBodyLen = 1 << 30

func scanSegment(path string) (map[uint64]locator, int64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = f.Close() }()

	empty, err := isEmptyFile(f)
	if err != nil {
		return nil, 0, false, err
	}
	if empty {
		// An empty segment holds no header and no records, so it is a fresh
		// index, not damage. The append path creates the file before it writes
		// the header, so a failure or a crash in that window leaves exactly this
		// file. Reporting an error here refused to start over a file that says
		// nothing (#674); returning end 0 makes the next append write the header.
		return make(map[uint64]locator), 0, false, nil
	}

	r := bufio.NewReader(f)
	if err := verifyHeader(r); err != nil {
		return nil, 0, false, err
	}

	locators := make(map[uint64]locator)
	offset := int64(headerLen)
	hdr := make([]byte, recordHdrLen)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) {
				return locators, offset, false, nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				// Torn record header: stop at the last complete record.
				return locators, offset, true, nil
			}
			return nil, 0, false, err
		}
		chunkID := binary.LittleEndian.Uint64(hdr[0:8])
		tombstone := hdr[8] == 1
		vecLen := binary.LittleEndian.Uint32(hdr[9:13])
		payloadLen := binary.LittleEndian.Uint32(hdr[13:17])
		bodyLen := int64(vecLen)*4 + int64(payloadLen)
		// vecLen and payloadLen are uint32, so a corrupt header can encode a body
		// near 21.5 GB. Discard takes an int, which is 32-bit on some platforms,
		// where that value wraps and can go negative; bufio panics on a negative
		// count, crashing Load instead of failing cleanly. Bound it first.
		//
		// An over-long body is treated as a torn tail rather than an error, which
		// matches what the scan already does for every other unreadable record:
		// stop at the last complete one. Startup then recovers instead of
		// bricking, and only records at or after the damage are dropped.
		if bodyLen < 0 || bodyLen > maxRecordBodyLen {
			return locators, offset, true, nil
		}
		if _, err := r.Discard(int(bodyLen)); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// Torn record body: stop at the last complete record.
				return locators, offset, true, nil
			}
			return nil, 0, false, fmt.Errorf("truncated record body for chunk %d: %w", chunkID, err)
		}
		if tombstone {
			delete(locators, chunkID)
		} else {
			locators[chunkID] = locator{offset: offset, vecLen: vecLen, payloadLen: payloadLen}
		}
		offset += int64(recordHdrLen) + bodyLen
	}
}

// isEmptyFile reports whether f holds no bytes at all.
func isEmptyFile(f *os.File) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	return info.Size() == 0, nil
}

// verifyHeader reads and validates the segment magic + version.
func verifyHeader(r io.Reader) error {
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return fmt.Errorf("read segment header: %w", err)
	}
	if string(hdr[:len(segmentMagic)]) != segmentMagic {
		return errors.New("diskindex: bad segment magic")
	}
	if v := binary.LittleEndian.Uint32(hdr[len(segmentMagic):]); v != segmentVersion {
		return fmt.Errorf("diskindex: unsupported segment version %d", v)
	}
	return nil
}

// --- payload + identity sidecar codecs ---

func encodePayload(p model.IndexPayload) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodePayload(b []byte) (model.IndexPayload, error) {
	var p model.IndexPayload
	if len(b) == 0 {
		return p, nil
	}
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&p); err != nil {
		return model.IndexPayload{}, err
	}
	return p, nil
}

func writeIdentitySidecar(path, identity string) error {
	staged, err := stageIdentitySidecar(path, identity)
	if err != nil {
		return err
	}
	return publishIdentitySidecar(staged, path)
}

// stageIdentitySidecar writes the identity to a temporary file beside path and
// returns that file's name, leaving it unpublished.
//
// Split from the rename so Reset can make the write durable BEFORE it destroys
// anything, and rename afterwards.
func stageIdentitySidecar(path, identity string) (string, error) {
	data, err := json.Marshal(struct {
		Identity string `json:"identity"`
	}{Identity: identity})
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	file, err := statefs.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	// fsync the file, not just close it: statefs.WriteFile returns once the
	// bytes are in the page cache, so a crash could publish a rename pointing
	// at an empty or partial sidecar. The segment already syncs this way in
	// finalizeFile; the sidecar did not, which is why the two were not one
	// durable transaction (#728).
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// publishIdentitySidecar renames the staged file into place and fsyncs the
// directory, without which the rename itself is not durable across a crash.
func publishIdentitySidecar(staged, path string) error {
	if err := os.Rename(staged, path); err != nil {
		_ = os.Remove(staged)
		return err
	}
	return syncDir(path)
}

// errSidecarUnreadable marks a sidecar that exists but cannot be understood, as
// distinct from one that is simply absent. Collapsing the two to "" is what let
// a damaged sidecar look exactly like a fresh index (#728).
var errSidecarUnreadable = errors.New("identity sidecar is present but unreadable")

// readIdentitySidecar returns the recorded identity.
//
// Three outcomes, deliberately not two:
//
//   - the sidecar is absent: ("", nil). For a segment that is also absent this
//     is a genuinely fresh index.
//   - the sidecar is readable: (identity, nil).
//   - the sidecar exists and is damaged: ("", errSidecarUnreadable). This used
//     to return "" like the first case, so EnsureIdentity saw a mismatch and
//     Reset threw away every vector the segment still held.
func readIdentitySidecar(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("%w: %s: %v", errSidecarUnreadable, path, err)
	}
	var s struct {
		Identity string `json:"identity"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return "", fmt.Errorf("%w: %s: %v", errSidecarUnreadable, path, err)
	}
	return s.Identity, nil
}

// --- float32 <-> bytes (little-endian) ---

func float32sToBytes(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

func bytesToFloat32s(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	fillFloat32s(out, b)
	return out
}

// fillFloat32s decodes little-endian float32s from b into the caller-provided
// dst (no allocation). dst must be sized for b (len(b)/4 elements); Search reuses
// a single query-sized dst across every candidate (issue #429 F2).
func fillFloat32s(dst []float32, b []byte) {
	for i := range dst {
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
}

func cosineSimilarity(a, b []float32) float32 {
	var dot, magA, magB float32
	for idx := range a {
		dot += a[idx] * b[idx]
		magA += a[idx] * a[idx]
		magB += b[idx] * b[idx]
	}
	if magA == 0 || magB == 0 {
		return 0
	}
	return dot / float32(math.Sqrt(float64(magA*magB)))
}
