package imagestore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LocalBackend writes images to the local file system. The ref is the absolute file path, exactly as before.
type LocalBackend struct {
	dir string
}

// NewLocalBackend creates a local backend. An empty dir means the caller decides (the caller joins the absolute path when actually writing).
//
// We still hold a dir here, so the admin-side path allowlist check can get its boundary from one place.
func NewLocalBackend(dir string) (*LocalBackend, error) {
	dir = strings.TrimSpace(dir)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create image directory: %w", err)
		}
	}
	return &LocalBackend{dir: dir}, nil
}

// Name implements Backend.
func (b *LocalBackend) Name() string { return BackendLocal }

// Dir returns the root directory.
func (b *LocalBackend) Dir() string { return b.dir }

// Save writes the bytes to dir/key (key is the file name). It returns the absolute path as the ref.
func (b *LocalBackend) Save(_ context.Context, key string, data []byte, _ string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("imagestore.local: key is empty")
	}
	if b.dir == "" {
		return "", fmt.Errorf("imagestore.local: directory is not set")
	}
	name := filepath.Base(key)
	if name != key || name == "." || name == ".." || strings.ContainsAny(key, `/\`) {
		return "", fmt.Errorf("imagestore.local: key must not contain a path separator: %q", key)
	}
	full := filepath.Join(b.dir, name)
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(full)
	if err != nil {
		return full, nil
	}
	return abs, nil
}

// validateLocalRef is defense in depth on the read/delete side: a ref is produced by Save and stored in the database, and normal data
// never contains a ".." segment (Join and Abs both clean it). This only blocks traversal forms; it neither forces an absolute path nor
// forces the path to lie inside the current dir. LocalDir is a runtime setting, historical assets may have been written to an old directory, and before the refactor
// the old version stored relative refs in the database when IMAGE_ASSET_DIR was a relative path.
func validateLocalRef(ref string) error {
	if slices.Contains(strings.Split(filepath.ToSlash(ref), "/"), "..") {
		return fmt.Errorf("imagestore.local: invalid ref: %q", ref)
	}
	return nil
}

// Open reads the local file as a stream.
func (b *LocalBackend) Open(_ context.Context, ref string) (io.ReadCloser, int64, error) {
	if ref == "" {
		return nil, 0, os.ErrNotExist
	}
	if err := validateLocalRef(ref); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(ref)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// Read reads everything at once, for building thumbnails.
func (b *LocalBackend) Read(_ context.Context, ref string) ([]byte, error) {
	if ref == "" {
		return nil, os.ErrNotExist
	}
	if err := validateLocalRef(ref); err != nil {
		return nil, err
	}
	return os.ReadFile(ref)
}

// Delete deletes the file; a missing file counts as success.
func (b *LocalBackend) Delete(_ context.Context, ref string) error {
	if ref == "" {
		return nil
	}
	if err := validateLocalRef(ref); err != nil {
		return err
	}
	if err := os.Remove(ref); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Compile-time interface check.
var _ Backend = (*LocalBackend)(nil)

// helper: builds a reader/buffer for use outside Save.
var _ = bytes.NewReader

// SaveReader copies an already encoded image without loading it into memory.
func (b *LocalBackend) SaveReader(ctx context.Context, key string, source io.ReadSeeker, size int64, mime string) (string, error) {
	if b.dir == "" || key == "" || filepath.Base(key) != key || key == "." || key == ".." || strings.ContainsAny(key, `/\`) {
		return "", fmt.Errorf("invalid local image key")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(b.dir, ".image-write-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	defer os.Remove(name)
	n, err := io.Copy(f, source)
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if size >= 0 && n != size {
		return "", io.ErrUnexpectedEOF
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	full := filepath.Join(b.dir, key)
	if err = os.Rename(name, full); err != nil {
		return "", err
	}
	return filepath.Abs(full)
}
