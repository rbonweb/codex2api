// Package imagestore provides pluggable storage backends for image assets.
//
// Design goals:
//   - The default LocalBackend behaves exactly as before (the absolute path is used as storage_path)
//   - An optional S3Backend covering S3-compatible services such as AWS S3, Cloudflare R2, MinIO, Alibaba Cloud OSS, Tencent Cloud COS and B2
//   - No database schema change: reads and deletes are routed by the storage_path prefix ("s3://" vs an absolute path), so existing local images need no migration
package imagestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

const (
	BackendLocal = "local"
	BackendS3    = "s3"

	s3RefScheme = "s3://"
)

// Backend abstracts storage reads and writes for image objects.
type Backend interface {
	// Name returns the backend identifier ("local" / "s3"), for logs and checks.
	Name() string
	// Save stores the image bytes and returns the ref after writing (LocalBackend returns the absolute path, S3Backend returns s3://bucket/key).
	Save(ctx context.Context, key string, data []byte, mime string) (ref string, err error)
	// Open reads as a stream; the caller is responsible for closing it. size may be -1 when unknown.
	Open(ctx context.Context, ref string) (rc io.ReadCloser, size int64, err error)
	// Read reads everything at once (for small data such as thumbnails).
	Read(ctx context.Context, ref string) ([]byte, error)
	// Delete deletes the corresponding ref.
	Delete(ctx context.Context, ref string) error
}

// Presigner is implemented by backends that can generate time-limited direct links for objects (currently only S3Backend).
type Presigner interface {
	PresignGetURL(ctx context.Context, ref string, ttl time.Duration) (string, error)
}

// ErrPresignUnsupported means the current backend cannot generate cloud direct links (such as LocalBackend).
var ErrPresignUnsupported = errors.New("imagestore: the current backend does not support generating cloud storage direct links")

// Config describes one runtime configuration in one place. It comes from system_settings.
type Config struct {
	Backend        string // local / s3
	Endpoint       string
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
	Prefix         string // for example "images/", automatically prepended to the object key
	ForcePathStyle bool   // must be enabled for MinIO and some self-hosted services
	LocalDir       string // directory used by LocalBackend; if empty, the caller provides the default
}

// Normalize fixes common whitespace and defaults, so values align with what is persisted in the DB.
func (c Config) Normalize() Config {
	c.Backend = strings.ToLower(strings.TrimSpace(c.Backend))
	if c.Backend == "" {
		c.Backend = BackendLocal
	}
	c.Endpoint = strings.TrimSpace(c.Endpoint)
	c.Region = strings.TrimSpace(c.Region)
	c.Bucket = strings.TrimSpace(c.Bucket)
	c.AccessKey = strings.TrimSpace(c.AccessKey)
	c.SecretKey = strings.TrimSpace(c.SecretKey)
	c.Prefix = strings.TrimSpace(c.Prefix)
	if c.Prefix != "" {
		c.Prefix = strings.Trim(c.Prefix, "/") + "/"
	}
	c.LocalDir = strings.TrimSpace(c.LocalDir)
	return c
}

// Validate checks the required fields before switching to S3.
func (c Config) Validate() error {
	c = c.Normalize()
	if c.Backend == BackendLocal {
		return nil
	}
	if c.Backend != BackendS3 {
		return fmt.Errorf("unknown image storage backend: %q", c.Backend)
	}
	missing := make([]string, 0, 4)
	if c.Bucket == "" {
		missing = append(missing, "bucket")
	}
	if c.AccessKey == "" {
		missing = append(missing, "access_key")
	}
	if c.SecretKey == "" {
		missing = append(missing, "secret_key")
	}
	if len(missing) > 0 {
		return fmt.Errorf("S3 config is missing fields: %s", strings.Join(missing, ", "))
	}
	return nil
}

// IsS3Ref reports whether storage_path points to the S3 backend.
func IsS3Ref(ref string) bool {
	return strings.HasPrefix(strings.TrimSpace(ref), s3RefScheme)
}

// state holds the backends currently in effect.
//
// It keeps both the local and (optional) s3 instances, so that existing local images and new images coexist:
//   - writes always go through currentWriter (the one the user selected in system settings)
//   - reads and deletes are dispatched to the matching backend by ref prefix
type state struct {
	cfg     Config
	local   Backend
	s3      Backend // nil means S3 is not currently enabled
	primary Backend // used for writes
}

var current atomic.Pointer[state]

// ErrNotConfigured means Configure has not been called yet.
var ErrNotConfigured = errors.New("imagestore: not configured")

// Configure applies a new configuration and atomically switches the global backend.
//
// It always constructs a LocalBackend as a fallback, so existing local images stay readable.
func Configure(cfg Config) error {
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return err
	}

	local, err := NewLocalBackend(cfg.LocalDir)
	if err != nil {
		return fmt.Errorf("imagestore: failed to initialize the local backend: %w", err)
	}

	st := &state{cfg: cfg, local: local, primary: local}

	if cfg.Backend == BackendS3 {
		s3, err := NewS3Backend(cfg)
		if err != nil {
			return fmt.Errorf("imagestore: failed to initialize the S3 backend: %w", err)
		}
		st.s3 = s3
		st.primary = s3
	}

	current.Store(st)
	return nil
}

// CurrentConfig returns a copy of the configuration currently in effect.
func CurrentConfig() Config {
	if st := current.Load(); st != nil {
		return st.cfg
	}
	return Config{Backend: BackendLocal}
}

// Primary returns the backend currently used for writes (the one the user selected in settings).
func Primary() (Backend, error) {
	st := current.Load()
	if st == nil {
		return nil, ErrNotConfigured
	}
	return st.primary, nil
}

// Resolve routes to the correct backend by ref.
//   - "s3://..." goes to S3 (an error is returned if S3 is not configured)
//   - everything else goes to Local
func Resolve(ref string) (Backend, error) {
	st := current.Load()
	if st == nil {
		return nil, ErrNotConfigured
	}
	if IsS3Ref(ref) {
		if st.s3 == nil {
			return nil, fmt.Errorf("imagestore: S3 is not enabled, but an S3 ref exists: %s", ref)
		}
		return st.s3, nil
	}
	return st.local, nil
}

// LocalDir returns the directory used by the current LocalBackend (for path allowlist validation).
func LocalDir() string {
	if st := current.Load(); st != nil {
		if lb, ok := st.local.(*LocalBackend); ok {
			return lb.Dir()
		}
	}
	return ""
}

// PresignURL generates a time-limited direct link for ref. It routes to the matching backend by ref prefix,
// and returns ErrPresignUnsupported if that backend does not support presigning (such as the local backend).
func PresignURL(ctx context.Context, ref string, ttl time.Duration) (string, error) {
	backend, err := Resolve(ref)
	if err != nil {
		return "", err
	}
	presigner, ok := backend.(Presigner)
	if !ok {
		return "", ErrPresignUnsupported
	}
	return presigner.PresignGetURL(ctx, ref, ttl)
}
