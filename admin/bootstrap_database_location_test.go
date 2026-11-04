package admin

import (
	"strings"
	"testing"
)

func TestBootstrapDatabaseLocationReadsDatabaseURLWithoutCredentials(t *testing.T) {
	t.Setenv("DATABASE_HOST", "ignored-host")
	t.Setenv("DATABASE_NAME", "ignored-db")
	t.Setenv("DATABASE_PORT", "")
	t.Setenv("DATABASE_URL", "postgresql://neondb_owner:topsecret@ep-cool-sun-a5b1.us-east-2.aws.neon.tech/neondb?sslmode=require")
	got := bootstrapDatabaseLocation("postgres")
	if got != "ep-cool-sun-a5b1.us-east-2.aws.neon.tech/neondb" {
		t.Fatalf("location = %q", got)
	}
	if strings.Contains(got, "topsecret") || strings.Contains(got, "neondb_owner") {
		t.Fatalf("location leaks credentials: %q", got)
	}

	t.Setenv("DATABASE_URL", "")
	if got := bootstrapDatabaseLocation("postgres"); got != "ignored-host/ignored-db" {
		t.Fatalf("split variables location = %q", got)
	}
}
