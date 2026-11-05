package database

import (
	"log"
	"os"
	"testing"
)

// TestMain initializes the SQLite schema template only once: after that, each test that creates a database copies the template directly and skips the migrations,
// reducing the per-test database creation overhead of ~1.2s under -race to milliseconds.
func TestMain(m *testing.M) {
	cleanup, err := PrepareSQLiteSchemaTemplate()
	if err != nil {
		log.Printf("SQLite schema template unavailable, tests fall back to full migrations: %v", err)
		os.Exit(m.Run())
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
