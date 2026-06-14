package corpusfs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

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
// stubbed) and pointing the Localize cache at StateDir/corpus-cache.
func newS3FromConfig(ctx context.Context, cfg Config) (CorpusFS, error) {
	if strings.TrimSpace(cfg.S3Bucket) == "" {
		return nil, errors.New("corpusfs: source kind s3 requires a bucket")
	}
	client, err := newS3Client(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cacheDir := ""
	if strings.TrimSpace(cfg.StateDir) != "" {
		cacheDir = filepath.Join(cfg.StateDir, s3CacheSubdir)
	}
	return NewS3FS(client, S3Config{
		Bucket:   cfg.S3Bucket,
		Prefix:   cfg.S3Prefix,
		CacheDir: cacheDir,
	})
}
