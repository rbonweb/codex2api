package config

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// schemaNameRegex restricts PostgreSQL schema names to ASCII identifiers, to prevent DSN/DDL injection.
var schemaNameRegex = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsValidSchemaName validates a PostgreSQL schema name (starts with a letter or underscore, the rest are letters, digits or underscores, length <= 63).
func IsValidSchemaName(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	return schemaNameRegex.MatchString(name)
}

// DatabaseConfig is the core database configuration.
type DatabaseConfig struct {
	Driver   string
	Path     string
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
	Schema   string // PostgreSQL schema (search_path); an empty value keeps the database default behavior
	SSLMode  string
}

// DSN returns the connection string for the current driver.
func (d *DatabaseConfig) DSN() string {
	if strings.EqualFold(d.Driver, "sqlite") {
		return d.Path
	}
	sslMode := d.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		quoteDSNValue(d.Host), d.Port, quoteDSNValue(d.User), quoteDSNValue(d.Password), quoteDSNValue(d.DBName), quoteDSNValue(sslMode))
	if d.Schema != "" {
		// Set search_path at connection startup through PostgreSQL startup options, so it applies to every connection in the pool.
		// The schema was validated against the allowlist in Load(), so it is safe to concatenate here.
		dsn += fmt.Sprintf(" options='-c search_path=%s,public'", d.Schema)
	}
	return dsn
}

// quoteDSNValue quotes one keyword/value DSN value so a password with spaces,
// quotes or backslashes (common in URL-decoded DATABASE_URL passwords) cannot
// split into extra keywords.
func quoteDSNValue(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

// applyDatabaseURL fills the PostgreSQL fields from a postgres:// or
// postgresql:// connection string such as the one Neon or Render shows. Only
// sslmode is read from the query string. Errors never echo the URL, because it
// carries the password.
func applyDatabaseURL(d *DatabaseConfig, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("DATABASE_URL is not a valid URL (expected postgresql://USER:PASSWORD@HOST[:PORT]/DBNAME?sslmode=require)")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "postgres", "postgresql":
	default:
		return fmt.Errorf("DATABASE_URL must start with postgres:// or postgresql://")
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("DATABASE_URL has no host")
	}
	port := 5432
	if value := parsed.Port(); value != "" {
		p, err := strconv.Atoi(value)
		if err != nil || p <= 0 || p > 65535 {
			return fmt.Errorf("DATABASE_URL has an invalid port")
		}
		port = p
	}
	dbName := strings.TrimPrefix(parsed.Path, "/")
	if dbName == "" {
		return fmt.Errorf("DATABASE_URL has no database name")
	}
	d.Host, d.Port, d.DBName = host, port, dbName
	d.User, d.Password = "", ""
	if parsed.User != nil {
		d.User = parsed.User.Username()
		d.Password, _ = parsed.User.Password()
	}
	if sslMode := strings.TrimSpace(parsed.Query().Get("sslmode")); sslMode != "" {
		d.SSLMode = sslMode
	}
	return nil
}

// Label returns the database label for display.
func (d *DatabaseConfig) Label() string {
	if strings.EqualFold(d.Driver, "sqlite") {
		return "SQLite"
	}
	return "PostgreSQL"
}

// RedisConfig is the core Redis configuration
type RedisConfig struct {
	Addr               string
	Username           string
	Password           string
	DB                 int
	TLS                bool
	InsecureSkipVerify bool
}

// CacheConfig is the core cache configuration.
type CacheConfig struct {
	Driver               string
	Redis                RedisConfig
	ResponseContextStore ResponseContextStoreConfig
}

// ResponseContextStoreConfig is the local disk persistence layer for previous_response_id context under the memory driver.
// An empty Path means disabled; by default it lives in a separate file in the same directory as the SQLite main database, so it is not included in the main database backup.
type ResponseContextStoreConfig struct {
	Path     string
	TTL      time.Duration
	MaxBytes int64
}

// Label returns the cache label for display.
func (c *CacheConfig) Label() string {
	if strings.EqualFold(c.Driver, "memory") {
		return "Memory"
	}
	return "Redis"
}

// Config is the global core environment configuration (server parameters that are physically isolated)
// Business logic parameters (such as ProxyURL, APIKeys and MaxConcurrency) have all been moved to the database SystemSettings instead.
type Config struct {
	Port                      int
	BindAddress               string // listen address, default 0.0.0.0 (compatible with Docker / reverse proxy / public network); set to 127.0.0.1 for local-only access
	AdminSecret               string
	AllowAnonymousV1          bool // explicitly allows /v1/* to pass without auth when no API key is configured (disallowed by default)
	APIKeyAuthCacheEnabled    bool
	MaxRequestBodySize        int
	RequestMemoryBudgetBytes  int64 // Process-local retained HTTP/WS request body budget.
	SchedulerMaxWaiters       int   // Process-local waiting request budget.
	SchedulerMaxWaitersPerKey int
	Database                  DatabaseConfig
	Cache                     CacheConfig
	UseWebsocket              bool     // whether to enable WebSocket transport
	CodexUpstreamTransport    string   // http|auto|ws, default http; USE_WEBSOCKET is kept as a legacy switch for compatibility
	TrustedProxies            []string // Gin trusted reverse proxy CIDRs/IPs; by default loopback and private networks are trusted for compatibility with Docker reverse proxies; none/off/false/0 disable it
}

// applyTimezone makes the TZ environment variable (including the one in .env) truly apply to local-time semantics such as daily quota limits.
// Go's time.Local is locked as soon as the process first formats local time (the startup log before config.Load in main
// triggers it), so godotenv's os.Setenv afterwards cannot affect it; time.Local must be overridden explicitly.
func applyTimezone() {
	tz := strings.TrimSpace(os.Getenv("TZ"))
	tz = strings.TrimPrefix(tz, ":") // POSIX allows the ":Asia/Shanghai" form
	if tz == "" {
		return
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		log.Printf("warning: TZ=%q cannot be parsed (%v), continuing with %s", tz, err, time.Local)
		return
	}
	time.Local = loc
}

// Load loads the core environment configuration from the .env file and supports overrides from environment variables.
func Load(envPath string) (*Config, error) {
	// Try to load the .env file (optional; if it does not exist, ignore it and use the current environment variables)
	if envPath == "" {
		envPath = ".env"
	}
	_ = godotenv.Load(envPath)
	applyTimezone()

	cfg := &Config{
		Port:               8080,
		MaxRequestBodySize: 48 * 1024 * 1024,
	}

	// Web service port
	if port := os.Getenv("CODEX_PORT"); port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	} else if port := os.Getenv("PORT"); port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	}
	cfg.AdminSecret = strings.TrimSpace(os.Getenv("ADMIN_SECRET"))
	cfg.AllowAnonymousV1 = parseBoolEnv(os.Getenv("CODEX_ALLOW_ANONYMOUS"))
	cfg.APIKeyAuthCacheEnabled = true
	if value := strings.TrimSpace(os.Getenv("CODEX_API_KEY_AUTH_CACHE_ENABLED")); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("CODEX_API_KEY_AUTH_CACHE_ENABLED must be a boolean")
		}
		cfg.APIKeyAuthCacheEnabled = enabled
	}
	// Default bind is 0.0.0.0, for compatibility with common deployments such as Docker port mapping, reverse proxies and production servers.
	// Security is ensured jointly by the fail-closed middleware, first-start self-service setup (/api/admin/bootstrap) and the startup banner;
	// users who want strictly local-only access can set CODEX_BIND=127.0.0.1.
	cfg.BindAddress = strings.TrimSpace(os.Getenv("CODEX_BIND"))
	if cfg.BindAddress == "" {
		cfg.BindAddress = "0.0.0.0"
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_MAX_REQUEST_BODY_SIZE_MB")); v != "" {
		if mb, err := strconv.Atoi(v); err == nil && mb > 0 {
			cfg.MaxRequestBodySize = mb * 1024 * 1024
		}
	}
	cfg.RequestMemoryBudgetBytes = max(int64(128<<20), int64(cfg.MaxRequestBodySize))
	if value := strings.TrimSpace(os.Getenv("CODEX_REQUEST_MEMORY_BUDGET_MB")); value != "" {
		mb, err := strconv.ParseInt(value, 10, 64)
		if err != nil || mb <= 0 || mb > (1<<63-1)/(1<<20) {
			return nil, fmt.Errorf("CODEX_REQUEST_MEMORY_BUDGET_MB must be a positive MiB value")
		}
		cfg.RequestMemoryBudgetBytes = mb << 20
		if cfg.RequestMemoryBudgetBytes < int64(cfg.MaxRequestBodySize) {
			return nil, fmt.Errorf("CODEX_REQUEST_MEMORY_BUDGET_MB must be at least CODEX_MAX_REQUEST_BODY_SIZE_MB")
		}
	}
	cfg.TrustedProxies = parseTrustedProxiesEnv(os.Getenv("CODEX_TRUSTED_PROXIES"))
	for _, setting := range []struct {
		name   string
		target *int
	}{
		{"CODEX_SCHEDULER_MAX_WAITERS", &cfg.SchedulerMaxWaiters},
		{"CODEX_SCHEDULER_MAX_WAITERS_PER_KEY", &cfg.SchedulerMaxWaitersPerKey},
	} {
		if value := strings.TrimSpace(os.Getenv(setting.name)); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%s must be a positive integer", setting.name)
			}
			*setting.target = n
		}
	}

	// Codex upstream transport configuration. CODEX_UPSTREAM_TRANSPORT takes precedence; USE_WEBSOCKET is kept as a legacy switch.
	cfg.CodexUpstreamTransport = normalizeCodexUpstreamTransport(os.Getenv("CODEX_UPSTREAM_TRANSPORT"))
	if cfg.CodexUpstreamTransport == "" && parseBoolEnv(os.Getenv("USE_WEBSOCKET")) {
		cfg.CodexUpstreamTransport = "ws"
		cfg.UseWebsocket = true
	}
	if cfg.CodexUpstreamTransport == "" {
		cfg.CodexUpstreamTransport = "http"
	}
	if cfg.CodexUpstreamTransport == "ws" {
		cfg.UseWebsocket = true
	}

	// Database configuration
	cfg.Database.Driver = normalizeDriver(os.Getenv("DATABASE_DRIVER"), "postgres")
	cfg.Database.Path = strings.TrimSpace(os.Getenv("DATABASE_PATH"))
	cfg.Database.Host = os.Getenv("DATABASE_HOST")
	if v := os.Getenv("DATABASE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Database.Port = p
		}
	}
	cfg.Database.User = os.Getenv("DATABASE_USER")
	cfg.Database.Password = os.Getenv("DATABASE_PASSWORD")
	cfg.Database.DBName = os.Getenv("DATABASE_NAME")
	if v := strings.TrimSpace(os.Getenv("DATABASE_SCHEMA")); v != "" {
		if !IsValidSchemaName(v) {
			return nil, fmt.Errorf("invalid DATABASE_SCHEMA: %q (only letters, digits and underscores are allowed, it must not start with a digit, and the length must not exceed 63)", v)
		}
		cfg.Database.Schema = v
	}
	if v := os.Getenv("DATABASE_SSLMODE"); v != "" {
		cfg.Database.SSLMode = v
	}
	// DATABASE_URL (postgresql://USER:PASSWORD@HOST[:PORT]/DBNAME?sslmode=...)
	// configures PostgreSQL in one variable and wins over the split DATABASE_* ones.
	if raw := strings.TrimSpace(os.Getenv("DATABASE_URL")); raw != "" {
		if explicit := strings.TrimSpace(os.Getenv("DATABASE_DRIVER")); explicit != "" && normalizeDriver(explicit, "postgres") != "postgres" {
			return nil, fmt.Errorf("DATABASE_URL is only for PostgreSQL and cannot be set together with DATABASE_DRIVER=%s", explicit)
		}
		cfg.Database.Driver = "postgres"
		if err := applyDatabaseURL(&cfg.Database, raw); err != nil {
			return nil, err
		}
	}

	// Cache configuration
	cfg.Cache.Driver = normalizeDriver(os.Getenv("CACHE_DRIVER"), "redis")
	cfg.Cache.Redis.Addr = strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	cfg.Cache.Redis.Username = strings.TrimSpace(os.Getenv("REDIS_USERNAME"))
	cfg.Cache.Redis.Password = os.Getenv("REDIS_PASSWORD")
	if v := os.Getenv("REDIS_DB"); v != "" {
		if db, err := strconv.Atoi(v); err == nil {
			cfg.Cache.Redis.DB = db
		}
	}
	cfg.Cache.Redis.TLS = parseBoolEnv(os.Getenv("REDIS_TLS"))
	cfg.Cache.Redis.InsecureSkipVerify = parseBoolEnv(os.Getenv("REDIS_INSECURE_SKIP_VERIFY"))
	responseContextStore, err := loadResponseContextStoreConfig(cfg.Cache.Driver, cfg.Database)
	if err != nil {
		return nil, err
	}
	cfg.Cache.ResponseContextStore = responseContextStore

	// Validate the required physical-layer configuration
	switch cfg.Database.Driver {
	case "sqlite":
		if cfg.Database.Path == "" {
			return nil, fmt.Errorf("the SQLite database path (DATABASE_PATH) must be configured through .env or environment variables")
		}
	case "postgres":
		if cfg.Database.Host == "" {
			return nil, fmt.Errorf("PostgreSQL must be configured through .env or environment variables (DATABASE_URL or DATABASE_HOST)")
		}
	default:
		return nil, fmt.Errorf("unsupported database driver: %s", cfg.Database.Driver)
	}
	if cfg.Database.Port == 0 {
		cfg.Database.Port = 5432
	}
	if cfg.Database.SSLMode == "" {
		cfg.Database.SSLMode = "disable"
	}

	switch cfg.Cache.Driver {
	case "memory":
	case "redis":
		if cfg.Cache.Redis.Addr == "" {
			return nil, fmt.Errorf("Redis must be configured through .env or environment variables (REDIS_ADDR)")
		}
	default:
		return nil, fmt.Errorf("unsupported cache driver: %s", cfg.Cache.Driver)
	}

	return cfg, nil
}

func normalizeDriver(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func parseBoolEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func parseTrustedProxiesEnv(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return []string{"127.0.0.1", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	}
	switch strings.ToLower(value) {
	case "0", "false", "off", "none", "no":
		return nil
	}

	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	proxies := make([]string, 0, len(parts))
	for _, part := range parts {
		if proxy := strings.TrimSpace(part); proxy != "" {
			proxies = append(proxies, proxy)
		}
	}
	return proxies
}

func normalizeCodexUpstreamTransport(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "http", "https", "sse":
		return "http"
	case "auto":
		return "auto"
	case "ws", "websocket", "wss":
		return "ws"
	default:
		return ""
	}
}

const (
	defaultResponseContextStoreTTLHours = 48
	defaultResponseContextStoreMaxMB    = 2048
)

// loadResponseContextStoreConfig parses RESPONSE_CONTEXT_STORE_*.
//   - RESPONSE_CONTEXT_STORE_PATH: explicit path; off/none/false/0 disable it. When unset,
//     the memory cache with a SQLite main database uses response-context.db in the same directory as the main database by default.
//   - RESPONSE_CONTEXT_STORE_TTL_HOURS: minimum retention in hours (1~720, default 48).
//   - RESPONSE_CONTEXT_STORE_MAX_MB: upper limit on the total body size of deduplicated entries (64~65536, default 2048).
func loadResponseContextStoreConfig(cacheDriver string, db DatabaseConfig) (ResponseContextStoreConfig, error) {
	result := ResponseContextStoreConfig{
		TTL:      defaultResponseContextStoreTTLHours * time.Hour,
		MaxBytes: int64(defaultResponseContextStoreMaxMB) << 20,
	}
	if value := strings.TrimSpace(os.Getenv("RESPONSE_CONTEXT_STORE_TTL_HOURS")); value != "" {
		hours, err := strconv.Atoi(value)
		if err != nil || hours < 1 || hours > 720 {
			return result, fmt.Errorf("RESPONSE_CONTEXT_STORE_TTL_HOURS must be an integer from 1 to 720: %q", value)
		}
		result.TTL = time.Duration(hours) * time.Hour
	}
	if value := strings.TrimSpace(os.Getenv("RESPONSE_CONTEXT_STORE_MAX_MB")); value != "" {
		mb, err := strconv.ParseInt(value, 10, 64)
		if err != nil || mb < 64 || mb > 65536 {
			return result, fmt.Errorf("RESPONSE_CONTEXT_STORE_MAX_MB must be an integer from 64 to 65536: %q", value)
		}
		result.MaxBytes = mb << 20
	}
	path, explicit := os.LookupEnv("RESPONSE_CONTEXT_STORE_PATH")
	path = strings.TrimSpace(path)
	if explicit && path != "" {
		switch strings.ToLower(path) {
		case "off", "none", "false", "0":
			return result, nil
		}
		if cacheDriver != "memory" {
			return result, fmt.Errorf("RESPONSE_CONTEXT_STORE_PATH only supports the memory cache driver; redis mode uses the shared context backend")
		}
		result.Path = path
		return result, nil
	}
	// Redis is already a shared backend; the local persistence layer is enabled by default only under the memory driver.
	if cacheDriver != "memory" || db.Driver != "sqlite" {
		return result, nil
	}
	if dir := sqliteDatabaseDir(db.Path); dir != "" {
		result.Path = filepath.Join(dir, "response-context.db")
	}
	return result, nil
}

// sqliteDatabaseDir returns the directory of the SQLite main database file; it returns empty for an in-memory database or an unrecognized DSN.
func sqliteDatabaseDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || path == ":memory:" || strings.Contains(strings.ToLower(path), "mode=memory") {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(path), "file:") {
		path = path[len("file:"):]
		if index := strings.IndexByte(path, '?'); index >= 0 {
			path = path[:index]
		}
	}
	if path == "" || path == ":memory:" {
		return ""
	}
	return filepath.Dir(path)
}
