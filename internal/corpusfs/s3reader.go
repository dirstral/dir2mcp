package corpusfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ErrRangeNotHonored is returned when an object store answers a ranged GET with
// bytes that do not start at the requested offset (#673).
//
// The condition is not exotic. An S3-compatible proxy or gateway that drops the
// Range header answers with the WHOLE object, so a reader that seeked to offset N
// receives the bytes at offset 0 and cannot tell. Those bytes are then hashed,
// chunked and cited as if they came from N, so the corpus indexes content the
// source document does not have at that position.
//
// It is a distinct sentinel because it is a property of the SERVER, not of the
// object: the same object read without a seek is fine, so a caller may report the
// backend as unusable for range reads instead of the document as broken.
var ErrRangeNotHonored = errors.New("corpusfs: object store did not honor the requested byte range")

// ErrObjectTruncated is returned when an object body ends before the size the
// store itself reported (#673).
//
// The bytes that did arrive are a PREFIX of the document. Returned as io.EOF they
// are indistinguishable from a complete read, so the pipeline stores a short
// document, a content hash over the wrong bytes, and citations that do not match
// the source. That silent short read is the #487 truncation failure, and #682
// established the rule this follows: a read that did not deliver the object fails
// with its own error and never with io.EOF.
var ErrObjectTruncated = errors.New("corpusfs: object body ended before the reported size")

// s3RangeReader is an io.ReadSeekCloser over an S3 object that satisfies reads
// via ranged GETs. It tracks a logical offset and lazily opens a GET body on the
// next Read after a seek, so a caller reading only a slice (e.g. a header probe)
// does not download the whole object. Note: ffmpeg/archive paths use Localize
// (a whole-object download) instead — range reads only help non-ffmpeg byte
// consumers such as image/PDF whole-file reads via io.ReadAll.
type s3RangeReader struct {
	ctx    context.Context
	client s3API
	bucket string
	key    string
	size   int64
	// maxBytes caps the bytes this reader will deliver in total (#682). It is
	// always positive (see resolveMaxBytes), so the reader is never unbounded.
	maxBytes int64

	offset int64
	body   io.ReadCloser
	closed bool
}

// Read fulfills io.Reader, opening a ranged GET from the current offset on
// demand.
//
// Two bounds apply, and they answer different problems (#682). The cap stops an
// object DISCOVERY under-reported: ListObjectsV2 said the object fitted, HeadObject
// says otherwise, and the cap refuses the difference. The per-call slice trim holds
// the reader to the size HeadObject DID report, so a body that returns more bytes
// than the range asked for cannot push the total past that size either.
//
// The cap is enforced as a limit+1, the same idiom every other bounded read in this
// repository uses. The reader delivers one byte more than the cap and then fails,
// because that extra byte is what separates an object of exactly the cap (which is
// inside the policy and must read cleanly to io.EOF) from an object past it.
//
// A read after Close returns fs.ErrClosed. Close leaves the logical offset where
// it was, so a reopen would issue a fresh ranged GET on a reader the caller has
// already released: the context may be cancelled, and if the store ignores the
// Range the reopened body serves offset 0 while the caller believes it is at
// r.offset (#673).
func (r *s3RangeReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, fs.ErrClosed
	}
	if r.offset > r.maxBytes {
		return 0, fmt.Errorf("corpusfs: read s3://%s/%s: %w (cap %d bytes)", r.bucket, r.key, ErrObjectTooLarge, r.maxBytes)
	}
	if r.offset >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		if err := r.openFrom(r.offset); err != nil {
			return 0, err
		}
	}
	// Never hand back more than the object's reported size, or more than cap+1,
	// whichever ends first.
	limit := min(r.size, r.maxBytes+1) - r.offset
	if int64(len(p)) > limit {
		p = p[:limit]
	}
	n, err := r.body.Read(p)
	r.offset += int64(n)
	if errors.Is(err, io.EOF) {
		return n, r.endOfBody()
	}
	return n, err
}

// endOfBody decides what the end of a GET body means at the current offset.
//
// It is io.EOF only when the reader delivered every byte the store said the
// object has. A body that stops earlier delivered a PREFIX, and reporting that
// prefix as io.EOF is the silent truncation #487 fixed once already, so it fails
// with ErrObjectTruncated instead. The cap is checked first so an object past the
// configured limit keeps the single answer #682 gave it.
func (r *s3RangeReader) endOfBody() error {
	if r.offset > r.maxBytes {
		return fmt.Errorf("corpusfs: read s3://%s/%s: %w (cap %d bytes)", r.bucket, r.key, ErrObjectTooLarge, r.maxBytes)
	}
	if r.offset >= r.size {
		return io.EOF
	}
	return fmt.Errorf("corpusfs: read s3://%s/%s: %w (got %d of %d bytes)",
		r.bucket, r.key, ErrObjectTruncated, r.offset, r.size)
}

// openFrom starts a ranged GET beginning at off through the end of the object,
// and refuses a response that does not start there (#673).
func (r *s3RangeReader) openFrom(off int64) error {
	if off >= r.size {
		return io.EOF
	}
	rangeHeader := fmt.Sprintf("bytes=%d-", off)
	out, err := r.client.GetObject(r.ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(r.key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		return fmt.Errorf("corpusfs: range get s3://%s/%s: %w", r.bucket, r.key, err)
	}
	if err := r.checkRangeResponse(off, out); err != nil {
		_ = out.Body.Close()
		return err
	}
	r.body = out.Body
	return nil
}

// checkRangeResponse verifies that the body of a ranged GET really begins at off.
//
// The only evidence accepted is a well-formed Content-Range that states the
// requested first byte position. A store that honors `bytes=N-` answers 206 with
// `Content-Range: bytes N-<end>/<total>`, which RFC 9110 §14.4 requires it to
// send. A store that DROPS the header answers 200 with the whole object and
// states no range at all, so an absent Content-Range at a non-zero offset is
// refused: the alternative is to read offset-0 bytes as if they came from N.
//
// A response length is NOT accepted as a substitute. Content-Length states how
// many bytes arrive, never where they start, so an object replaced between the
// HEAD and the GET can produce a full body whose length happens to equal the
// requested tail. The one response without a Content-Range that is still
// accepted is off == 0, where a whole-object body IS the requested range.
//
// A stated complete length must also match the size HEAD reported. A different
// total describes a different object, and its bytes are not the ones the caller
// asked for. The `*` form states no total and is left to the start check.
func (r *s3RangeReader) checkRangeResponse(off int64, out *s3.GetObjectOutput) error {
	rng, stated, err := parseContentRange(aws.ToString(out.ContentRange))
	if err != nil {
		return r.refuseRange(err.Error())
	}
	if !stated {
		if off == 0 {
			return nil
		}
		return r.refuseRange(fmt.Sprintf("asked for byte %d, response states no range", off))
	}
	if rng.start != off {
		return r.refuseRange(fmt.Sprintf("asked for byte %d, response starts at byte %d", off, rng.start))
	}
	if rng.total >= 0 && rng.total != r.size {
		return r.refuseRange(fmt.Sprintf("object is %d bytes, response states %d", r.size, rng.total))
	}
	return nil
}

// refuseRange builds the ErrRangeNotHonored refusal. detail names offsets and
// sizes only, never a byte of the object, so the error is safe to log.
func (r *s3RangeReader) refuseRange(detail string) error {
	return fmt.Errorf("corpusfs: range get s3://%s/%s: %w (%s)", r.bucket, r.key, ErrRangeNotHonored, detail)
}

// contentRange is a parsed `Content-Range: bytes <start>-<end>/<total>` value.
type contentRange struct {
	start int64
	end   int64
	// total is the complete length of the object, or -1 for the `*` form RFC 9110
	// allows when the store does not know it.
	total int64
}

// parseContentRange parses a Content-Range response header. stated=false means
// the response carried no Content-Range at all.
//
// The whole value is validated, not only the part that is read. A store that
// sends a malformed header has not stated where its bytes start, so accepting the
// leading digits of `bytes 32-x/64` would trust exactly the response that proves
// the store is unreliable. Every malformed form is an error, and an error is
// never treated as an absent header (which would fall through to the off == 0
// acceptance).
func parseContentRange(header string) (contentRange, bool, error) {
	h := strings.TrimSpace(header)
	if h == "" {
		return contentRange{}, false, nil
	}
	const unit = "bytes "
	if !strings.HasPrefix(h, unit) {
		return contentRange{}, true, fmt.Errorf("unsupported Content-Range unit in %q", header)
	}
	spec, totalText, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(h, unit)), "/")
	if !ok {
		return contentRange{}, true, fmt.Errorf("no complete length in Content-Range %q", header)
	}
	rng, err := parseByteRangeSpec(spec)
	if err != nil {
		return contentRange{}, true, fmt.Errorf("%s in Content-Range %q", err, header)
	}
	total, err := parseCompleteLength(totalText)
	if err != nil {
		return contentRange{}, true, fmt.Errorf("%s in Content-Range %q", err, header)
	}
	rng.total = total
	return rng, true, nil
}

// parseByteRangeSpec parses the `<start>-<end>` half of a Content-Range. Both
// positions are required: `bytes 32-/64` states a start it does not back with an
// end, which is not a range a store served.
func parseByteRangeSpec(spec string) (contentRange, error) {
	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return contentRange{}, errors.New("no last byte position")
	}
	start, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil || start < 0 {
		return contentRange{}, errors.New("invalid first byte position")
	}
	end, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)
	if err != nil || end < start {
		return contentRange{}, errors.New("invalid last byte position")
	}
	return contentRange{start: start, end: end}, nil
}

// parseCompleteLength parses the `/<total>` half of a Content-Range. It returns
// -1 for the `*` form, which is legal and states nothing.
func parseCompleteLength(text string) (int64, error) {
	t := strings.TrimSpace(text)
	if t == "*" {
		return -1, nil
	}
	total, err := strconv.ParseInt(t, 10, 64)
	if err != nil || total <= 0 {
		return 0, errors.New("invalid complete length")
	}
	return total, nil
}

// Seek fulfills io.Seeker. Any pending GET body is closed so the next Read
// reopens from the new offset. A seek after Close returns fs.ErrClosed, matching
// Read: a closed reader is finished, and moving its offset only sets up a read
// that must fail anyway (#673).
func (r *s3RangeReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, fs.ErrClosed
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.offset + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, errors.New("corpusfs: invalid seek whence")
	}
	if abs < 0 {
		return 0, errors.New("corpusfs: negative seek position")
	}
	if abs != r.offset && r.body != nil {
		_ = r.body.Close()
		r.body = nil
	}
	r.offset = abs
	return abs, nil
}

// Close releases any open GET body and marks the reader closed, so a later Read
// cannot issue a new ranged GET on a reader the caller already released (#673).
// Double-Close is a no-op, as it is for s3StreamReader.
func (r *s3RangeReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.body != nil {
		err := r.body.Close()
		r.body = nil
		return err
	}
	return nil
}

// s3StreamReader is an io.ReadSeekCloser over an S3 object whose total size is
// unknown (a HEAD that omitted Content-Length). It streams a single whole-object
// GET rather than issuing ranged reads, so sequential consumers (io.ReadAll of a
// whole file) get the complete bytes instead of the silent-empty result a
// size-0 range reader would produce. Seeks are limited to what a forward-only
// stream can honor: querying the current position, a no-op seek to it, and
// forward skips (by discarding bytes); backward seeks and SeekEnd fail because
// the object cannot be rewound and its length is unknown.
type s3StreamReader struct {
	ctx    context.Context
	client s3API
	bucket string
	key    string
	// maxBytes caps the bytes this reader will deliver in total (#682). This
	// reader is the one that most needs the cap: it exists precisely because the
	// object reported no length at all, so nothing else bounds it.
	maxBytes int64

	offset int64
	body   io.ReadCloser
	closed bool
}

// Read fulfills io.Reader, opening the whole-object GET body on demand.
//
// It stops at the configured cap with ErrObjectTooLarge (#682). An unknown-length
// object is the case where a size check cannot help at all: there is no reported
// size to check, so only a bound on the bytes as they arrive can hold.
//
// The cap is enforced as a limit+1, exactly as in s3RangeReader. Here the extra
// byte is the ONLY way to tell an object of exactly the cap from one past it: with
// no reported length, the difference between "the body ended" and "the body
// continues" is one byte of evidence.
func (r *s3StreamReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, fs.ErrClosed
	}
	if r.offset > r.maxBytes {
		return 0, fmt.Errorf("corpusfs: read s3://%s/%s: %w (cap %d bytes)", r.bucket, r.key, ErrObjectTooLarge, r.maxBytes)
	}
	if r.body == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if limit := r.maxBytes + 1 - r.offset; int64(len(p)) > limit {
		p = p[:limit]
	}
	n, err := r.body.Read(p)
	r.offset += int64(n)
	return n, err
}

// open starts the whole-object (unranged) GET.
func (r *s3StreamReader) open() error {
	out, err := r.client.GetObject(r.ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(r.key),
	})
	if err != nil {
		return fmt.Errorf("corpusfs: stream get s3://%s/%s: %w", r.bucket, r.key, err)
	}
	r.body = out.Body
	return nil
}

// Seek fulfills io.Seeker within the limits of a forward-only stream: it resolves
// the target absolute offset, treats a seek to the current position as a no-op
// (so callers can query the position with Seek(0, io.SeekCurrent)), and services
// a forward seek by discarding the intervening bytes. Backward seeks and seeks
// relative to the (unknown) end are rejected.
func (r *s3StreamReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, fs.ErrClosed
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.offset + offset
	case io.SeekEnd:
		return 0, errors.New("corpusfs: cannot seek relative to end of unknown-length s3 object")
	default:
		return 0, errors.New("corpusfs: invalid seek whence")
	}
	if abs < 0 {
		return 0, errors.New("corpusfs: negative seek position")
	}
	if abs == r.offset {
		return abs, nil
	}
	if abs < r.offset {
		return 0, errors.New("corpusfs: cannot seek backward in a streaming s3 object")
	}
	// A forward seek is serviced by DISCARDING bytes, so an unbounded target would
	// pull an unbounded body through the socket to reach it (#682). The cap applies
	// to bytes read, not to bytes returned, so it applies here too.
	if abs > r.maxBytes {
		return 0, fmt.Errorf("corpusfs: forward seek s3://%s/%s: %w (cap %d bytes)", r.bucket, r.key, ErrObjectTooLarge, r.maxBytes)
	}
	if r.body == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if _, err := io.CopyN(io.Discard, r.body, abs-r.offset); err != nil {
		return 0, fmt.Errorf("corpusfs: forward seek s3://%s/%s: %w", r.bucket, r.key, err)
	}
	r.offset = abs
	return abs, nil
}

// Close releases the open GET body, if any, and marks the reader closed so a
// later Read/Seek cannot silently reopen the stream from offset 0 while r.offset
// is non-zero (which would return corrupted data). Double-Close is a no-op.
func (r *s3StreamReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.body != nil {
		err := r.body.Close()
		r.body = nil
		return err
	}
	return nil
}
