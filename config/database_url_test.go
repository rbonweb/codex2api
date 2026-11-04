package config

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func clearDatabaseEnvForURLTest(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"DATABASE_URL", "DATABASE_DRIVER", "DATABASE_PATH", "DATABASE_HOST", "DATABASE_PORT",
		"DATABASE_USER", "DATABASE_PASSWORD", "DATABASE_NAME", "DATABASE_SCHEMA", "DATABASE_SSLMODE",
		"CACHE_DRIVER", "REDIS_ADDR",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("CACHE_DRIVER", "memory")
}

// A Neon connection string pasted as is must configure PostgreSQL, including a
// password that needs URL escaping and SSL from the query string.
func TestLoadReadsDatabaseURL(t *testing.T) {
	clearDatabaseEnvForURLTest(t)
	t.Setenv("DATABASE_URL", "postgresql://neondb_owner:p%40ss%20w'rd%5C@ep-cool-sun-a5b1.us-east-2.aws.neon.tech/neondb?sslmode=require&channel_binding=require")
	// DATABASE_URL wins over the split variables.
	t.Setenv("DATABASE_HOST", "ignored-host")
	t.Setenv("DATABASE_NAME", "ignored-db")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	db := cfg.Database
	if db.Driver != "postgres" || db.Host != "ep-cool-sun-a5b1.us-east-2.aws.neon.tech" || db.Port != 5432 ||
		db.User != "neondb_owner" || db.Password != `p@ss w'rd\` || db.DBName != "neondb" || db.SSLMode != "require" {
		t.Fatalf("database config = %+v", db)
	}

	parsed, err := pgconn.ParseConfig(db.DSN())
	if err != nil {
		t.Fatalf("DSN does not parse: %v", err)
	}
	if parsed.Host != db.Host || parsed.Port != 5432 || parsed.User != "neondb_owner" || parsed.Password != `p@ss w'rd\` || parsed.Database != "neondb" || parsed.TLSConfig == nil {
		t.Fatalf("DSN round trip = host %q port %d user %q password %q db %q tls %v", parsed.Host, parsed.Port, parsed.User, parsed.Password, parsed.Database, parsed.TLSConfig != nil)
	}
}

func TestLoadDatabaseURLPortAndDefaults(t *testing.T) {
	clearDatabaseEnvForURLTest(t)
	t.Setenv("DATABASE_URL", "postgres://app:secret@db.example.com:6543/codex")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.Port != 6543 || cfg.Database.SSLMode != "disable" || cfg.Database.DBName != "codex" {
		t.Fatalf("database config = %+v", cfg.Database)
	}
}

func TestLoadRejectsBadDatabaseURLWithoutLeakingPassword(t *testing.T) {
	cases := map[string]string{
		"mysql scheme": "mysql://app:topsecret@db.example.com/codex",
		"no host":      "postgres://app:topsecret@/codex",
		"bad port":     "postgres://app:topsecret@db.example.com:port/codex",
		"no database":  "postgres://app:topsecret@db.example.com",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			clearDatabaseEnvForURLTest(t)
			t.Setenv("DATABASE_URL", raw)
			_, err := Load("")
			if err == nil {
				t.Fatalf("Load accepted %q", raw)
			}
			if strings.Contains(err.Error(), "topsecret") {
				t.Fatalf("error leaks the password: %v", err)
			}
		})
	}
}

func TestLoadRejectsDatabaseURLWithSQLiteDriver(t *testing.T) {
	clearDatabaseEnvForURLTest(t)
	t.Setenv("DATABASE_DRIVER", "sqlite")
	t.Setenv("DATABASE_PATH", "/data/codex2api.db")
	t.Setenv("DATABASE_URL", "postgres://app:secret@db.example.com/codex")
	if _, err := Load(""); err == nil {
		t.Fatal("DATABASE_URL with DATABASE_DRIVER=sqlite must fail instead of silently picking one")
	}
}

// Split variables keep working, and a password with spaces or quotes no longer
// breaks the keyword/value DSN.
func TestDSNQuotesSplitVariables(t *testing.T) {
	d := DatabaseConfig{Driver: "postgres", Host: "postgres", Port: 5432, User: "codex2api", Password: `a b'c`, DBName: "codex2api", SSLMode: "disable"}
	parsed, err := pgconn.ParseConfig(d.DSN())
	if err != nil {
		t.Fatalf("DSN does not parse: %v", err)
	}
	if parsed.Password != `a b'c` || parsed.User != "codex2api" || parsed.Database != "codex2api" {
		t.Fatalf("DSN round trip = user %q password %q db %q", parsed.User, parsed.Password, parsed.Database)
	}
}
