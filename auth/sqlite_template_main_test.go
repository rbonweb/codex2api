package auth

import (
	"log"
	"os"
	"testing"

	"github.com/codex2api/database"
)

// TestMain initializes the SQLite schema template only once: after that, each test creates its database by copying the template directly and skipping migrations,
// bringing the per-test database creation cost under -race from ~1.2s down to milliseconds.
func TestMain(m *testing.M) {
	cleanup, err := database.PrepareSQLiteSchemaTemplate()
	if err != nil {
		log.Printf("SQLite schema template unavailable, tests fall back to full migrations: %v", err)
		os.Exit(m.Run())
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
