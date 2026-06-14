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
	"sort"
	"sync"

	"golang.org/x/exp/mmap"

	"github.com/dirstral/dir2mcp/internal/model"
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
}

// compile-time assertions: DiskIndex satisfies the core contract and both
// optional capabilities (issue #246/#247).
var (
	_ model.Index          = (*DiskIndex)(nil)
	_ model.Persistable    = (*DiskIndex)(nil)
	_ model.FilteringIndex = (*DiskIndex)(nil)
)

// New creates an empty disk index. path is the segment file used by Save/Load
// and the append log; it may be empty for a purely in-process instance, in
// which case Save/Load require an explicit path argument.
func New(path string) *DiskIndex {
	return &DiskIndex{
		path:     path,
		locators: make(map[uint64]locator),
	}
}

// Upsert appends the vector+payload as the newest record and points the locator
// at it. An empty vector or zero chunk_id is an error (matching HNSWIndex).
func (d *DiskIndex) Upsert(ctx context.Context, vector []float32, payload model.IndexPayload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(vector) == 0 {
		return errors.New("vector cannot be empty")
	}
	if payload.ChunkID == 0 {
		return errors.New("payload chunk_id cannot be zero")
	}
	if d.path == "" {
		return errors.New("disk index requires a path")
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	return d.appendRecord(payload.ChunkID, false, vector, payload)
}

// Delete writes a tombstone record for each id and drops the locator. Unknown
// ids are ignored. Tombstones keep the on-disk log append-only and consistent;
// space is reclaimed on the next Save (compaction).
func (d *DiskIndex) Delete(ctx context.Context, chunkIDs []uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunkIDs) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range chunkIDs {
		if _, ok := d.locators[id]; !ok {
			continue
		}
		if err := d.appendRecord(id, true, nil, model.IndexPayload{}); err != nil {
			return err
		}
		delete(d.locators, id)
	}
	return nil
}

// appendRecord appends one record to the segment, creating/initialising the
// file (with header) on first write, and updates the locator map. The caller
// must hold the write lock.
func (d *DiskIndex) appendRecord(chunkID uint64, tombstone bool, vector []float32, payload model.IndexPayload) error {
	f, err := os.OpenFile(d.path, os.O_RDWR|os.O_CREATE, 0o644)
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
	payloadBytes, err := encodePayload(payload)
	if err != nil {
		return err
	}
	recOffset := d.appendEnd
	n, err := writeRecord(f, chunkID, tombstone, vector, payloadBytes)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	d.appendEnd += int64(n)

	if !tombstone {
		d.locators[chunkID] = locator{
			offset:     recOffset,
			vecLen:     uint32(len(vector)),
			payloadLen: uint32(len(payloadBytes)),
		}
	}
	// The mmap view is now stale (file grew); drop it so the next read re-maps.
	return d.invalidateReaderLocked()
}

// ensureAppendEnd initialises d.appendEnd from the open file, writing the header
// on a fresh/empty file. Caller holds the write lock.
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

	d.mu.RLock()
	defer d.mu.RUnlock()

	reader, err := d.readerLocked()
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return []model.IndexHit{}, nil
	}

	applyFilter := !filter.IsZero()
	scored := make([]model.IndexHit, 0, len(d.locators))
	for id, loc := range d.locators {
		if int(loc.vecLen) != len(vector) {
			continue // dimension mismatch; skip like HNSW does
		}
		vec, payload, rerr := readRecordAt(reader, loc)
		if rerr != nil {
			return nil, rerr
		}
		if applyFilter && !filter.Match(payload) {
			continue
		}
		scored = append(scored, model.IndexHit{
			ChunkID: id,
			Score:   cosineSimilarity(vector, vec),
			Payload: payload,
		})
	}

	sortHits(scored)
	if len(scored) > k {
		scored = scored[:k]
	}
	return scored, nil
}

// sortHits orders hits best-first with a deterministic chunkID tiebreak,
// matching HNSWIndex.Search.
func sortHits(hits []model.IndexHit) {
	const eps = 1e-6
	sort.Slice(hits, func(a, b int) bool {
		diff := math.Abs(float64(hits[a].Score) - float64(hits[b].Score))
		if diff <= eps {
			return hits[a].ChunkID < hits[b].ChunkID
		}
		return hits[a].Score > hits[b].Score
	})
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

	if d.path == "" {
		return nil
	}
	if err := truncateSegment(d.path); err != nil {
		return err
	}
	d.appendEnd = int64(headerLen)
	return writeIdentitySidecar(identitySidecarPath(d.path), identity)
}

// truncateSegment rewrites path with just a fresh header.
func truncateSegment(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
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

	reader, err := d.readerLocked()
	if err != nil {
		return err
	}

	tmpPath := path + ".tmp"
	out, err := os.Create(tmpPath)
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
	return writeIdentitySidecar(identitySidecarPath(path), d.identity)
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
	return nil
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

	locators, end, scanErr := scanSegment(path)
	if scanErr != nil {
		if errors.Is(scanErr, os.ErrNotExist) {
			d.mu.Lock()
			d.path = path
			d.identity = readIdentitySidecar(identitySidecarPath(path))
			d.mu.Unlock()
			return nil
		}
		return scanErr
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.invalidateReaderLocked(); err != nil {
		return err
	}
	d.path = path
	d.locators = locators
	d.appendEnd = end
	d.identity = readIdentitySidecar(identitySidecarPath(path))
	return nil
}

// Close releases the mmap view.
func (d *DiskIndex) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.invalidateReaderLocked()
}

// readerLocked returns the mmap reader, opening it lazily. Returns (nil, nil)
// when no segment file exists yet (fresh index). The cached view is invalidated
// on every mutation (appendRecord/Reset/Save), so a held view always reflects
// the on-disk bytes the current locator offsets point at.
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
// returns the locator map and the end-of-file offset.
func scanSegment(path string) (map[uint64]locator, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReader(f)
	if err := verifyHeader(r); err != nil {
		return nil, 0, err
	}

	locators := make(map[uint64]locator)
	offset := int64(headerLen)
	hdr := make([]byte, recordHdrLen)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, 0, err
		}
		chunkID := binary.LittleEndian.Uint64(hdr[0:8])
		tombstone := hdr[8] == 1
		vecLen := binary.LittleEndian.Uint32(hdr[9:13])
		payloadLen := binary.LittleEndian.Uint32(hdr[13:17])
		bodyLen := int64(vecLen)*4 + int64(payloadLen)
		if _, err := r.Discard(int(bodyLen)); err != nil {
			return nil, 0, fmt.Errorf("truncated record body for chunk %d: %w", chunkID, err)
		}
		if tombstone {
			delete(locators, chunkID)
		} else {
			locators[chunkID] = locator{offset: offset, vecLen: vecLen, payloadLen: payloadLen}
		}
		offset += int64(recordHdrLen) + bodyLen
	}
	return locators, offset, nil
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
	data, err := json.Marshal(struct {
		Identity string `json:"identity"`
	}{Identity: identity})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readIdentitySidecar returns the recorded identity, or "" when the sidecar is
// missing/unreadable (treated as a fresh index, like a missing snapshot).
func readIdentitySidecar(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var s struct {
		Identity string `json:"identity"`
	}
	if json.Unmarshal(data, &s) != nil {
		return ""
	}
	return s.Identity
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
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
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
