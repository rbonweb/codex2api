package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// sqliteSchemaTemplate points to an SQLite database file that has already run all migrations. Once registered,
// New("sqlite", path) copies this template directly and skips the schema migrations when the target file does not exist yet.
//
// Only for the test process: hundreds of tests each create a database in TempDir, and each time fully replay 700 lines of DDL;
// the pure-Go SQLite engine is ~25x slower under -race, one migration takes 1.2s, while copying the template takes only 1ms.
// Production startup never registers the template, so the path and behavior are completely unchanged.
var sqliteSchemaTemplate atomic.Pointer[string]

// UseSQLiteSchemaTemplate registers the template file path (or clears it with an empty string).
func UseSQLiteSchemaTemplate(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		sqliteSchemaTemplate.Store(nil)
		return
	}
	sqliteSchemaTemplate.Store(&path)
}

// PrepareSQLiteSchemaTemplate fully initializes an SQLite database once in a temp directory and registers it as the template,
// for the TestMain of each test package to call; the returned cleanup unregisters the template and deletes the temp directory.
func PrepareSQLiteSchemaTemplate() (cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "codex2api-sqlite-template-")
	if err != nil {
		return nil, err
	}
	cleanup = func() {
		UseSQLiteSchemaTemplate("")
		_ = os.RemoveAll(dir)
	}
	// Building the template must go through the real migration path, so first make sure no leftover registration remains.
	UseSQLiteSchemaTemplate("")
	path := filepath.Join(dir, "schema-template.db")
	db, err := New("sqlite", path)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to initialize SQLite template: %w", err)
	}
	// Merge the WAL content back into the main file, so the template only needs the single .db file to be copied in full.
	if _, err := db.conn.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = db.Close()
		cleanup()
		return nil, fmt.Errorf("failed to checkpoint SQLite template: %w", err)
	}
	if err := db.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to close SQLite template: %w", err)
	}
	UseSQLiteSchemaTemplate(path)
	return cleanup, nil
}

// sqliteSchemaTemplateTarget checks whether the raw DSN is a "regular file path that does not exist yet".
// Only this case can safely use the template: existing files (which may be an old schema and need a real migration),
// :memory: and DSNs with file: or query parameters always take the original path.
func sqliteSchemaTemplateTarget(rawDSN string) (string, bool) {
	if sqliteSchemaTemplate.Load() == nil {
		return "", false
	}
	path := strings.TrimSpace(rawDSN)
	if path == "" || path == ":memory:" || strings.Contains(path, "?") ||
		strings.HasPrefix(strings.ToLower(path), "file:") {
		return "", false
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	return path, true
}

// applySQLiteSchemaTemplate copies the template to target; any failure returns false, so the caller
// falls back to the real migration path, and a failed copy never turns into a half-written database.
func applySQLiteSchemaTemplate(target string) bool {
	tmpl := sqliteSchemaTemplate.Load()
	if tmpl == nil {
		return false
	}
	src, err := os.Open(*tmpl)
	if err != nil {
		return false
	}
	defer src.Close()
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(target)
		return false
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(target)
		return false
	}
	return true
}
