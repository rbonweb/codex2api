package admin

import (
	"log"
	"os"
	"testing"

	"github.com/codex2api/database"
)

// TestMain initializes the SQLite schema template only once: after that, each test copies the template to create its database and skips migrations,
// which cuts the per-test database setup cost under -race from about 1.2s to milliseconds.
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
