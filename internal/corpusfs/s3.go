package corpusfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3API is the subset of the S3 client S3FS depends on. Interfacing it lets
// tests stub ListObjectsV2/GetObject/HeadObject without hitting the network.
type s3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

// presignFunc mints a presigned http(s) GetObject URL for a full object key. It
// is a function rather than a concrete *s3.PresignClient so the s3API client can
// stay an interface (the SDK's NewPresignClient needs a concrete *s3.Client) and
// so tests can inject a deterministic, network-free presigner. A nil presignFunc
// means the backend cannot presign and MediaURL reports ok=false.
type presignFunc func(ctx context.Context, bucket, key string) (string, error)

// s3PresignExpiry bounds the lifetime of presigned media URLs. It only needs to
// outlast a single ffmpeg range-read of one segment, so it is kept short to limit
// the blast radius if a URL leaks (e.g. into a log line of an ffmpeg invocation).
const s3PresignExpiry = 15 * time.Minute

// S3FS is a CorpusFS backed by an S3 bucket+prefix. AbsPath on discovered files
// is empty (there is no local path); Localize downloads to a temp file under the
// configured cache dir so ffmpeg/archive extraction can read a real path.
type S3FS struct {
	client   s3API
	bucket   string
	prefix   string // normalized: no leading slash, trailing slash if non-empty
	cacheDir string // temp download root for Localize (e.g. StateDir/cache)
	// presign mints presigned GetObject URLs for the MediaURL capability. Nil
	// when the backend was built without presign support (e.g. a stub client in a
	// test that does not exercise the URL path); MediaURL then reports ok=false so
	// callers fall back to Localize.
	presign presignFunc
}

// S3Config configures an S3FS.
type S3Config struct {
	Bucket string
	Prefix string
	// CacheDir is the directory under which Localize materializes temp files.
	// When empty, the OS temp dir is used.
	CacheDir string
}

// NewS3FS constructs an S3FS over the provided client and config. The client is
// injected so production code can pass a real *s3.Client and tests can pass a
// stub. The resulting S3FS has no presigner (MediaURL reports ok=false); the
// factory installs one via newS3FSWithPresign when building from real config.
func NewS3FS(client s3API, cfg S3Config) (*S3FS, error) {
	return newS3FSWithPresign(client, cfg, nil)
}

// newS3FSWithPresign is NewS3FS plus an explicit presigner so the factory can
// wire S3 presigned-URL support and tests can inject a network-free presigner.
func newS3FSWithPresign(client s3API, cfg S3Config, presign presignFunc) (*S3FS, error) {
	if client == nil {
		return nil, errors.New("corpusfs: nil s3 client")
	}
	bucket := strings.TrimSpace(cfg.Bucket)
	if bucket == "" {
		return nil, errors.New("corpusfs: empty s3 bucket")
	}
	return &S3FS{
		client:   client,
		bucket:   bucket,
		prefix:   normalizeS3Prefix(cfg.Prefix),
		cacheDir: cfg.CacheDir,
		presign:  presign,
	}, nil
}

// MediaURL implements MediaURLProvider: it presigns a short-lived GetObject URL
// for relPath so a range-seeking consumer (avutil.ExtractSegmentURL → ffmpeg)
// reads only the bytes it needs over HTTP instead of forcing a whole-object
// Localize download. ok=false (nil error) means no presigner is configured and
// the caller should fall back to Localize; a non-nil error means presigning
// failed.
func (s *S3FS) MediaURL(ctx context.Context, relPath string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if s.presign == nil {
		return "", false, nil
	}
	key := s.keyForRel(relPath)
	url, err := s.presign(ctx, s.bucket, key)
	if err != nil {
		return "", false, fmt.Errorf("corpusfs: presign s3://%s/%s: %w", s.bucket, key, err)
	}
	return url, true, nil
}

// normalizeS3Prefix strips a leading slash and ensures a non-empty prefix ends
// with a single slash so key↔relPath mapping is unambiguous.
func normalizeS3Prefix(prefix string) string {
	p := strings.TrimSpace(prefix)
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return ""
	}
	return strings.TrimSuffix(p, "/") + "/"
}

// keyForRel maps a corpus-relative path to a full S3 object key.
func (s *S3FS) keyForRel(relPath string) string {
	// No TrimSpace: Walk emits RelPath verbatim, so keys with leading/trailing
	// spaces must round-trip exactly through keyForRel for Open/Localize.
	return s.prefix + strings.TrimPrefix(filepath.ToSlash(relPath), "/")
}

// relForKey maps a full S3 object key to its corpus-relative path, or returns
// ok=false when the key does not live under the configured prefix.
func (s *S3FS) relForKey(key string) (string, bool) {
	if !strings.HasPrefix(key, s.prefix) {
		return "", false
	}
	return strings.TrimPrefix(key, s.prefix), true
}

// Walk lists objects under the prefix and converts them to DiscoveredFile,
// honoring MaxSizeBytes and the default excluded directories (matched against
// the relative key segments). The root argument is ignored: the configured
// prefix is the corpus root. Results are sorted by RelPath.
func (s *S3FS) Walk(ctx context.Context, _ string, opts Options) ([]DiscoveredFile, error) {
	if opts.MaxSizeBytes <= 0 {
		opts.MaxSizeBytes = defaultMaxFileSizeBytes
	}

	files := make([]DiscoveredFile, 0, 256)
	var token *string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(s.prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("corpusfs: list s3://%s/%s: %w", s.bucket, s.prefix, err)
		}
		for _, obj := range out.Contents {
			f, ok := s.discoveredFromObject(obj, opts)
			if !ok {
				continue
			}
			files = append(files, f)
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
		if token == nil {
			break
		}
	}

	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, nil
}

// discoveredFromObject converts a listed object into a DiscoveredFile, applying
// the directory-key skip, excluded-dir, and size-cap policies. ok=false means
// the object is skipped.
func (s *S3FS) discoveredFromObject(obj s3types.Object, opts Options) (DiscoveredFile, bool) {
	key := aws.ToString(obj.Key)
	rel, ok := s.relForKey(key)
	if !ok {
		return DiscoveredFile{}, false
	}
	// Skip "directory" placeholder keys (those ending in "/") and the prefix
	// itself.
	if rel == "" || strings.HasSuffix(rel, "/") {
		return DiscoveredFile{}, false
	}
	if keyHasExcludedDir(rel) {
		return DiscoveredFile{}, false
	}
	size := aws.ToInt64(obj.Size)
	if size > opts.MaxSizeBytes {
		return DiscoveredFile{}, false
	}
	var mtime int64
	if obj.LastModified != nil {
		mtime = obj.LastModified.Unix()
	}
	return DiscoveredFile{
		RelPath:   rel,
		AbsPath:   "",
		SizeBytes: size,
		MTimeUnix: mtime,
		ETag:      strings.Trim(aws.ToString(obj.ETag), "\""),
	}, true
}

// keyHasExcludedDir reports whether the relative key lives under a
// default-excluded directory (e.g. .git, node_modules). Only ancestor path
// segments are tested — the final segment is the object's own basename, and
// LocalFS excludes directories, not regular files, so a file whose name happens
// to equal an excluded-dir name (e.g. a file literally named "vendor") must
// still be discovered for the two backends to stay in parity.
func keyHasExcludedDir(rel string) bool {
	segs := strings.Split(rel, "/")
	for _, seg := range segs[:len(segs)-1] {
		if _, ok := defaultExcludedDirs[seg]; ok {
			return true
		}
	}
	return false
}

// Open returns a range-GET-backed seekable reader. HeadObject supplies the total
// size so seeks (including io.SeekEnd) resolve, and reads issue a ranged GET for
// the requested window — so a caller reading a slice does not download the whole
// object.
func (s *S3FS) Open(ctx context.Context, relPath string) (io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := s.keyForRel(relPath)
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("corpusfs: head s3://%s/%s: %w", s.bucket, key, err)
	}
	return &s3RangeReader{
		ctx:    ctx,
		client: s.client,
		bucket: s.bucket,
		key:    key,
		size:   aws.ToInt64(head.ContentLength),
	}, nil
}

// Localize downloads the object to a temp file under the cache dir (preserving
// the object extension so ffmpeg can infer the muxer) and returns its path with
// a cleanup that removes the file.
func (s *S3FS) Localize(ctx context.Context, relPath string) (string, func(), error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	key := s.keyForRel(relPath)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", nil, fmt.Errorf("corpusfs: get s3://%s/%s: %w", s.bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()

	dir := s.cacheDir
	if strings.TrimSpace(dir) == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("corpusfs: create cache dir: %w", err)
	}
	ext := path.Ext(relPath)
	tmp, err := os.CreateTemp(dir, "corpusfs-*"+ext)
	if err != nil {
		return "", nil, fmt.Errorf("corpusfs: create temp file: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if _, err := io.Copy(tmp, out.Body); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", nil, fmt.Errorf("corpusfs: download s3://%s/%s: %w", s.bucket, key, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("corpusfs: close temp file: %w", err)
	}
	return tmp.Name(), cleanup, nil
}
