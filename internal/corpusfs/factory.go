package corpusfs

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Config is the resolved corpus-source description the factory consumes. It is a
// flat, dependency-free view (no import of internal/config) so corpusfs stays a
// leaf package; callers project their config.SourceConfig onto it.
//
// RootDir is the local corpus root used for local/nfs kinds. StateDir is always
// local and is where the S3 backend materializes its download cache — the state
// directory never moves to the object store.
type Config struct {
	// Kind is one of "local", "nfs", or "s3" (case-insensitive). Empty and any
	// non-s3 value are treated as a local filesystem backend.
	Kind string

	// RootDir is the local corpus root for local/nfs kinds.
	RootDir string
	// StateDir is the always-local state directory; the S3 cache lives under it.
	StateDir string

	// S3* describe the object-store backend (used only when Kind=="s3").
	S3Bucket          string
	S3Prefix          string
	S3Region          string
	S3Endpoint        string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3SessionToken    string
}

// S3API is the exported alias of the S3 client surface S3FS depends on, provided
// so external tests can declare a stub client and inject it through
// SetS3ClientBuilderForTest without importing the unexported interface.
type S3API = s3API

// s3CacheSubdir is the StateDir-relative directory under which the S3 backend
// materializes Localize() downloads. Kept local so non-local corpora still cache
// to the local state directory.
const s3CacheSubdir = "corpus-cache"

// newS3Client builds a real *s3.Client from the resolved config. It is a package
// var so tests can substitute a stub builder that returns a fake s3API without
// touching the network or requiring real credentials.
var newS3Client = func(ctx context.Context, cfg Config) (s3API, error) {
	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if strings.TrimSpace(cfg.S3Region) != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(strings.TrimSpace(cfg.S3Region)))
	}
	// Trim all three credential parts consistently: AWS keys/tokens never carry
	// meaningful leading/trailing whitespace, and env-sourced values commonly
	// pick some up. Checking the trimmed value but passing the raw one would let
	// a whitespace-padded secret pass the guard and then fail auth at AWS.
	accessKeyID := strings.TrimSpace(cfg.S3AccessKeyID)
	secretAccessKey := strings.TrimSpace(cfg.S3SecretAccessKey)
	if accessKeyID != "" && secretAccessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				accessKeyID,
				secretAccessKey,
				strings.TrimSpace(cfg.S3SessionToken),
			),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("corpusfs: load aws config: %w", err)
	}

	s3Opts := []func(*s3.Options){}
	if endpoint := strings.TrimSpace(cfg.S3Endpoint); endpoint != "" {
		// A custom endpoint (S3-compatible store such as MinIO/R2) typically
		// needs path-style addressing.
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = &endpoint
			o.UsePathStyle = true
		})
	}
	return s3.NewFromConfig(awsCfg, s3Opts...), nil
}

// SetS3ClientBuilderForTest swaps the S3 client builder New uses for the s3 kind
// and returns a restore func. It exists so external tests can exercise the
// factory's s3 path with a network-free stub client; production code never calls
// it. The returned restore func reinstates the previous builder; passing nil
// leaves the current builder in place (and restore is still a no-op-safe revert).
func SetS3ClientBuilderForTest(builder func(ctx context.Context, cfg Config) (S3API, error)) (restore func()) {
	prev := newS3Client
	if builder != nil {
		newS3Client = builder
	}
	return func() { newS3Client = prev }
}

// NewS3FSWithPresignForTest builds an S3FS over a stub client with an explicit
// presigner so external tests can exercise the MediaURL capability and the
// worker's URL-based segment path without a concrete *s3.Client or the network.
// presign maps (bucket, key) to a URL; a nil presign yields an S3FS whose
// MediaURL reports ok=false. Production code uses New / NewS3FS instead.
func NewS3FSWithPresignForTest(client S3API, cfg S3Config, presign func(ctx context.Context, bucket, key string) (string, error)) (*S3FS, error) {
	return newS3FSWithPresign(client, cfg, presignFunc(presign))
}

// New builds the CorpusFS selected by cfg.Kind. Local and nfs both yield a
// LocalFS rooted at cfg.RootDir (an NFS mount is just a local path). s3 builds an
// aws-sdk-go-v2 S3 client and an S3FS whose Localize cache lives under the
// always-local StateDir. The default kind (local) is byte-for-byte the historical
// behavior.
func New(ctx context.Context, cfg Config) (CorpusFS, error) {
	kind := strings.ToLower(strings.TrimSpace(cfg.Kind))
	switch kind {
	case "", "local", "nfs":
		return NewLocalFS(cfg.RootDir), nil
	case "s3":
		return newS3FromConfig(ctx, cfg)
	default:
		return nil, fmt.Errorf("corpusfs: unknown source kind %q", cfg.Kind)
	}
}

// newS3FromConfig constructs the S3-backed CorpusFS, building the client (real or
// stubbed) and pointing the Localize cache at StateDir/corpus-cache. When the
// built client is a concrete *s3.Client it also wires a presigner so the MediaURL
// capability can hand ffmpeg a range-seekable URL (issue #243); a stub client (no
// concrete *s3.Client) yields no presigner and MediaURL falls back to Localize.
func newS3FromConfig(ctx context.Context, cfg Config) (CorpusFS, error) {
	if err := validateS3SourceConfig(cfg); err != nil {
		return nil, err
	}
	client, err := newS3Client(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cacheDir := ""
	if strings.TrimSpace(cfg.StateDir) != "" {
		cacheDir = filepath.Join(cfg.StateDir, s3CacheSubdir)
	}
	return newS3FSWithPresign(client, S3Config{
		Bucket:   cfg.S3Bucket,
		Prefix:   cfg.S3Prefix,
		CacheDir: cacheDir,
	}, presignerForClient(client))
}

// validateS3SourceConfig enforces the S3 corpus-source invariants before a
// client is built so misconfiguration fails fast with a clear, actionable error
// at startup rather than as an opaque endpoint-resolution failure on the first
// ListObjects (issue #487). It requires a bucket; requires a region when no
// custom endpoint is configured (AWS endpoint resolution needs one, and the SDK
// otherwise fails deep in the request path); and, when a custom endpoint is set,
// requires it to be a syntactically valid http(s) URL with a host.
func validateS3SourceConfig(cfg Config) error {
	if strings.TrimSpace(cfg.S3Bucket) == "" {
		return errors.New("corpusfs: source kind s3 requires a bucket")
	}
	region := strings.TrimSpace(cfg.S3Region)
	endpoint := strings.TrimSpace(cfg.S3Endpoint)
	if endpoint == "" {
		if region == "" {
			return errors.New("corpusfs: source kind s3 requires a region " +
				"(set source.s3.region, DIR2MCP_SOURCE_S3_REGION, or AWS_REGION) " +
				"when no custom endpoint is configured")
		}
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		// Never echo the raw endpoint: it may embed userinfo
		// (http://user:pass@host), and a parse-failure error would leak those
		// credentials into logs/startup output.
		return errors.New("corpusfs: source.s3.endpoint is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("corpusfs: source.s3.endpoint must use http or https, got %q", redactedEndpoint(u))
	}
	if u.Host == "" {
		return errors.New("corpusfs: source.s3.endpoint must include a host")
	}
	return nil
}

// redactedEndpoint renders a parsed endpoint URL as scheme://host, dropping any
// userinfo, path, or query so credentials embedded in the endpoint
// (http://user:pass@host) never reach logs or error messages.
func redactedEndpoint(u *url.URL) string {
	if u.Host == "" {
		return u.Scheme
	}
	return u.Scheme + "://" + u.Host
}

// presignerForClient returns a presignFunc backed by the SDK's PresignClient when
// client is a concrete *s3.Client, or nil otherwise. The SDK's NewPresignClient
// requires the concrete type, so a stub s3API (tests) cannot be presigned —
// MediaURL then reports ok=false and the worker falls back to Localize.
func presignerForClient(client s3API) presignFunc {
	concrete, ok := client.(*s3.Client)
	if !ok {
		return nil
	}
	pc := s3.NewPresignClient(concrete)
	return func(ctx context.Context, bucket, key string) (string, error) {
		req, err := pc.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}, s3.WithPresignExpires(s3PresignExpiry))
		if err != nil {
			return "", err
		}
		return req.URL, nil
	}
}
