package database

import (
	"path/filepath"
	"testing"
)

func newSQLiteTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api-test.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func withPricingOverrides(t *testing.T, overrides map[string]ModelPricingOverride) {
	t.Helper()
	previous := currentModelPricingOverrides()
	t.Cleanup(func() { SetModelPricingOverrides(previous) })
	SetModelPricingOverrides(overrides)
}
