package corpusfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

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
}

// Read fulfills io.Reader, opening a ranged GET from the current offset on
// demand.
//
// Two bounds apply, and they answer different lies (#682). The cap check refuses
// to deliver a byte past maxBytes, which is what stops an object HeadObject
// under-reported and discovery therefore admitted. The per-call slice trim holds
// the reader to the size HeadObject DID report, so a body that returns more bytes
// than the range asked for cannot push the total past that size either.
func (r *s3RangeReader) Read(p []byte) (int, error) {
	if r.offset >= r.maxBytes {
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
	// Never hand back more than the object's reported size, or more than the cap,
	// whichever ends first.
	limit := min(r.size, r.maxBytes) - r.offset
	if int64(len(p)) > limit {
		p = p[:limit]
	}
	n, err := r.body.Read(p)
	r.offset += int64(n)
	return n, err
}

// openFrom starts a ranged GET beginning at off through the end of the object.
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
	r.body = out.Body
	return nil
}

// Seek fulfills io.Seeker. Any pending GET body is closed so the next Read
// reopens from the new offset.
func (r *s3RangeReader) Seek(offset int64, whence int) (int64, error) {
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

// Close releases any open GET body.
func (r *s3RangeReader) Close() error {
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
func (r *s3StreamReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, fs.ErrClosed
	}
	if r.offset >= r.maxBytes {
		return 0, fmt.Errorf("corpusfs: read s3://%s/%s: %w (cap %d bytes)", r.bucket, r.key, ErrObjectTooLarge, r.maxBytes)
	}
	if r.body == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if limit := r.maxBytes - r.offset; int64(len(p)) > limit {
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
