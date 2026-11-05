package proxy

import (
	"log"
	"os"
	"testing"

	"github.com/codex2api/database"
)

// TestMain initializes the SQLite schema template only once: afterwards each test creates its database by copying the template and skipping migration,
// reducing the per-test ~1.2s database creation overhead under -race to milliseconds.
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
