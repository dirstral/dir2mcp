package corpusfs

import (
	"context"
	"errors"
	"fmt"
	"io"

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

	offset int64
	body   io.ReadCloser
}

// Read fulfills io.Reader, opening a ranged GET from the current offset on
// demand.
func (r *s3RangeReader) Read(p []byte) (int, error) {
	if r.offset >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		if err := r.openFrom(r.offset); err != nil {
			return 0, err
		}
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
