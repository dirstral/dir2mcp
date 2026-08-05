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

	"github.com/dirstral/dir2mcp/internal/relpath"
	"github.com/dirstral/dir2mcp/internal/statefs"
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
	// Before the capability check, not after: a backend without a presigner
	// would otherwise answer "no media URL" for a path that should have been
	// refused outright, which is a different answer from Open and Localize.
	if err := guardRel(relPath); err != nil {
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
// guardRel refuses a rel_path that is not inside the corpus.
//
// Discovery validates every key it emits, but a rel_path reaching Open,
// Localize or MediaURL has usually come back through the store, an MCP
// argument or a config file, and SPEC §7.8 requires the check on open and stat
// as well as on list. keyForRel would otherwise strip a leading slash and
// address a different object than the caller named.
func guardRel(relPath string) error {
	if _, err := relpath.Normalize(relPath); err != nil {
		return fmt.Errorf("corpusfs: %q: %w", relPath, err)
	}
	return nil
}

func (s *S3FS) keyForRel(relPath string) string {
	// No TrimSpace: Walk emits RelPath verbatim, so keys with leading/trailing
	// spaces must round-trip exactly through keyForRel for Open/Localize.
	return s.prefix + strings.TrimPrefix(filepath.ToSlash(relPath), "/")
}

// errKeyNotUnderPrefix marks a key outside the configured prefix. That is an
// ordinary, expected outcome of listing (the prefix is a filter, not a
// guarantee), and is distinct from a key that IS under the prefix but whose
// relative name cannot be trusted.
var errKeyNotUnderPrefix = errors.New("key is not under the configured prefix")

// relForKey maps a full S3 object key to its corpus-relative path.
//
// Stripping the prefix as a raw string is not enough. A bucket is untrusted
// input, and a key like `corpus/../outside.mp4` strips to `../outside.mp4`,
// which then flows into code that joins it against the LOCAL root: video
// recognition builds `RootDir + rel_path`, and so does TTML/SMIL export
// probing. A leading-slash rel path does not even round-trip, because
// keyForRel strips the slash and addresses a different object. SPEC §7.8
// requires root/prefix isolation on every backend, so the relative name is
// validated here, at the one place both callers pass through (#735).
func (s *S3FS) relForKey(key string) (string, error) {
	if !strings.HasPrefix(key, s.prefix) {
		return "", errKeyNotUnderPrefix
	}
	rel := strings.TrimPrefix(key, s.prefix)
	// The prefix itself. In scope, simply not a file.
	if rel == "" {
		return rel, nil
	}
	// A directory placeholder is in scope and not a file, but its NAME still
	// has to be inside the corpus: `corpus/../` is a placeholder shape and a
	// traversal, and skipping it on shape alone would hide it from
	// OnUnsafeKey. Validate what it points at, then let the caller skip it.
	if strings.HasSuffix(rel, "/") {
		if _, err := relpath.Normalize(strings.TrimSuffix(rel, "/")); err != nil {
			return "", err
		}
		return rel, nil
	}
	// Normalize returns the path unchanged or refuses it, so a discovered
	// rel_path is always the key's own bytes and maps back to the object it
	// came from.
	return relpath.Normalize(rel)
}

// Walk lists objects under the prefix and converts them to DiscoveredFile,
// honoring MaxSizeBytes and the default excluded directories (matched against
// the relative key segments). When opts.UseGitIgnore is set, the rules in any
// discovered .gitignore objects are fetched and applied to the listed keys so
// gitignore is honored on S3 exactly as it is on the local filesystem (issue
// #487). The root argument is ignored: the configured prefix is the corpus root.
// Results are sorted by RelPath.
func (s *S3FS) Walk(ctx context.Context, _ string, opts Options) ([]DiscoveredFile, error) {
	if opts.MaxSizeBytes <= 0 {
		opts.MaxSizeBytes = defaultMaxFileSizeBytes
	}

	files, gitignoreRels, err := s.listObjects(ctx, opts)
	if err != nil {
		return nil, err
	}

	if opts.UseGitIgnore && len(gitignoreRels) > 0 {
		if files, err = s.filterGitIgnored(ctx, files, gitignoreRels); err != nil {
			return nil, err
		}
	}

	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, nil
}

// listObjects paginates the bucket listing, converting in-scope objects to
// DiscoveredFile and (when gitignore is enabled) collecting the corpus-relative
// paths of any .gitignore objects so their rules can be loaded afterward.
func (s *S3FS) listObjects(ctx context.Context, opts Options) ([]DiscoveredFile, []string, error) {
	files := make([]DiscoveredFile, 0, 256)
	var gitignoreRels []string
	var token *string
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(s.prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("corpusfs: list s3://%s/%s: %w", s.bucket, s.prefix, err)
		}
		for _, obj := range out.Contents {
			if opts.UseGitIgnore {
				if rel, err := s.relForKey(aws.ToString(obj.Key)); err == nil && path.Base(rel) == ".gitignore" {
					gitignoreRels = append(gitignoreRels, rel)
				}
			}
			if f, ok := s.discoveredFromObject(obj, opts); ok {
				files = append(files, f)
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
		if token == nil {
			break
		}
	}
	return files, gitignoreRels, nil
}

// filterGitIgnored loads the rules from the discovered .gitignore objects and
// drops the discovered files they exclude, in place.
func (s *S3FS) filterGitIgnored(ctx context.Context, files []DiscoveredFile, gitignoreRels []string) ([]DiscoveredFile, error) {
	rules, err := s.loadGitIgnoreRules(ctx, gitignoreRels)
	if err != nil {
		return nil, err
	}
	kept := files[:0]
	for _, f := range files {
		if keyIgnoredByGitignore(f.RelPath, rules) {
			continue
		}
		kept = append(kept, f)
	}
	return kept, nil
}

// maxGitIgnoreObjectBytes bounds how many bytes are read from a .gitignore
// object so a pathologically large key cannot exhaust memory during discovery.
const maxGitIgnoreObjectBytes = 1 << 20 // 1 MiB

// loadGitIgnoreRules fetches and parses the given .gitignore objects into a
// single ordered rule set. The rels are sorted root-first (by directory depth,
// then lexically) so shallower rules precede deeper ones — matching the local
// walker's parent-first accumulation and gitignore's last-match-wins precedence.
func (s *S3FS) loadGitIgnoreRules(ctx context.Context, rels []string) ([]gitIgnoreRule, error) {
	ordered := append([]string(nil), rels...)
	sort.Slice(ordered, func(i, j int) bool {
		di, dj := strings.Count(ordered[i], "/"), strings.Count(ordered[j], "/")
		if di != dj {
			return di < dj
		}
		return ordered[i] < ordered[j]
	})

	var rules []gitIgnoreRule
	for _, rel := range ordered {
		content, err := s.getGitIgnoreObject(ctx, rel)
		if err != nil {
			return nil, err
		}
		relDir := path.Dir(rel)
		if relDir == "." {
			relDir = ""
		}
		rules = append(rules, parseGitIgnoreContent(content, relDir)...)
	}
	return rules, nil
}

// getGitIgnoreObject reads a single .gitignore object's bytes (bounded by
// maxGitIgnoreObjectBytes) so its rules can be parsed.
func (s *S3FS) getGitIgnoreObject(ctx context.Context, rel string) ([]byte, error) {
	key := s.keyForRel(rel)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("corpusfs: get gitignore s3://%s/%s: %w", s.bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()
	// Read one byte past the cap so a .gitignore larger than
	// maxGitIgnoreObjectBytes can be detected. Applying a silently truncated
	// rule set would drop trailing ignore rules and wrongly include files, so
	// fail loudly rather than filter with partial rules.
	content, err := io.ReadAll(io.LimitReader(out.Body, maxGitIgnoreObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("corpusfs: read gitignore s3://%s/%s: %w", s.bucket, key, err)
	}
	if int64(len(content)) > maxGitIgnoreObjectBytes {
		return nil, fmt.Errorf("corpusfs: gitignore s3://%s/%s exceeds %d-byte limit; "+
			"refusing to apply truncated ignore rules", s.bucket, key, maxGitIgnoreObjectBytes)
	}
	return content, nil
}

// discoveredFromObject converts a listed object into a DiscoveredFile, applying
// the directory-key skip, excluded-dir, and size-cap policies. ok=false means
// the object is skipped.
func (s *S3FS) discoveredFromObject(obj s3types.Object, opts Options) (DiscoveredFile, bool) {
	key := aws.ToString(obj.Key)
	rel, err := s.relForKey(key)
	if err != nil {
		// Not under the prefix is the listing's own noise. A key that IS under
		// it but is not a usable rel_path is a finding: report it rather than
		// dropping it silently, so an operator can see the bucket carries keys
		// dir2mcp refuses (#735).
		if !errors.Is(err, errKeyNotUnderPrefix) && opts.OnUnsafeKey != nil {
			opts.OnUnsafeKey(key, err)
		}
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
		if opts.OnOversize != nil {
			opts.OnOversize(rel, size)
		}
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
	if err := guardRel(relPath); err != nil {
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
	// A HEAD that omits Content-Length (seen on some MinIO/R2/gateway setups)
	// leaves the total size unknown. The range reader treats an unknown size as 0
	// and returns io.EOF on the first Read, so io.ReadAll would yield empty
	// content with no error — a silently truncated document. Fall back to a
	// whole-object streaming GET so the bytes are actually read (issue #487).
	if head.ContentLength == nil {
		return &s3StreamReader{
			ctx:    ctx,
			client: s.client,
			bucket: s.bucket,
			key:    key,
		}, nil
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
	if err := guardRel(relPath); err != nil {
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
	if err := statefs.MkdirAll(dir); err != nil {
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
