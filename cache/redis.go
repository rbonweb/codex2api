package cache

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisOptions describes the Redis connection settings used by the token cache.
type RedisOptions struct {
	Addr               string
	Username           string
	Password           string
	DB                 int
	PoolSize           int
	TLS                bool
	InsecureSkipVerify bool
}

// redisTokenCache is the Redis token cache (modeled on the sub2api OpenAITokenCache interface)
type redisTokenCache struct {
	client *redis.Client
}

var _ RuntimeOwnerStore = (*redisTokenCache)(nil)

type redisResponseContextRecord struct {
	Items []json.RawMessage `json:"items"`
}

// NewRedis creates the Redis token cache (uses the default when poolSize <= 0).
func NewRedis(addr, password string, db int, poolSize ...int) (TokenCache, error) {
	size := 0
	if len(poolSize) > 0 && poolSize[0] > 0 {
		size = poolSize[0]
	}
	return NewRedisWithOptions(RedisOptions{
		Addr:     addr,
		Password: password,
		DB:       db,
		PoolSize: size,
	})
}

// NewRedisWithOptions creates the Redis token cache. It supports redis:// and rediss:// URLs and TLS.
func NewRedisWithOptions(cfg RedisOptions) (TokenCache, error) {
	opts, err := buildRedisClientOptions(cfg)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opts)

	ping := func(ctx context.Context) error {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return client.Ping(pingCtx).Err()
	}
	if err := waitForRedisLoading(context.Background(), ping, redisLoadingWait(), time.Sleep); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("failed to connect to Redis: %w%s", err, redisConnectionHint(err, opts.TLSConfig != nil))
	}

	return &redisTokenCache{client: client}, nil
}

// defaultRedisLoadingWait is how long to wait by default for Redis to load its dataset.
// Loading a very large AOF/RDB (tens of GB) takes minutes after a restart; during that time Redis accepts connections
// but replies -LOADING to commands; failing immediately would put the process into a crash-restart loop.
const defaultRedisLoadingWait = 10 * time.Minute

func redisLoadingWait() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CODEX_REDIS_LOADING_WAIT_SECONDS"))
	if raw == "" {
		return defaultRedisLoadingWait
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		return defaultRedisLoadingWait
	}
	return time.Duration(seconds) * time.Second
}

// isRedisLoadingError reports whether the error is a -LOADING reply, meaning Redis is still loading its dataset.
func isRedisLoadingError(err error) bool {
	if err == nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(err.Error()), "LOADING")
}

// waitForRedisLoading pings Redis; only on a -LOADING reply does it poll and retry within the wait duration,
// while other errors (wrong address, wrong password, connection refused, etc.) keep the original fail-fast behavior.
func waitForRedisLoading(ctx context.Context, ping func(context.Context) error, wait time.Duration, sleep func(time.Duration)) error {
	err := ping(ctx)
	if err == nil || !isRedisLoadingError(err) {
		return err
	}
	if wait <= 0 {
		return err
	}
	log.Printf("Redis is loading the dataset into memory, waiting until ready (up to %s)...", wait)
	const retryInterval = 2 * time.Second
	var elapsed time.Duration
	for attempt := 1; ; attempt++ {
		if elapsed >= wait {
			return fmt.Errorf("timed out waiting for Redis to load the dataset (%s): %w", wait, err)
		}
		sleep(retryInterval)
		elapsed += retryInterval
		err = ping(ctx)
		if err == nil || !isRedisLoadingError(err) {
			return err
		}
		// Log about once every 30 seconds, to avoid flooding the log.
		if attempt%15 == 0 {
			log.Printf("Redis is still loading the dataset, continuing to wait...")
		}
	}
}

func buildRedisClientOptions(cfg RedisOptions) (*redis.Options, error) {
	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		return nil, fmt.Errorf("Redis address must not be empty")
	}

	var opts *redis.Options
	if isRedisURL(addr) {
		parsed, err := redis.ParseURL(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid Redis URL format: %w", err)
		}
		opts = parsed
		if cfg.DB != 0 {
			opts.DB = cfg.DB
		}
	} else {
		opts = &redis.Options{
			Addr: addr,
			DB:   cfg.DB,
		}
	}

	if strings.TrimSpace(cfg.Username) != "" {
		opts.Username = strings.TrimSpace(cfg.Username)
	}
	if cfg.Password != "" {
		opts.Password = cfg.Password
	}
	if cfg.PoolSize > 0 {
		opts.PoolSize = cfg.PoolSize
	}
	if cfg.TLS || strings.HasPrefix(strings.ToLower(addr), "rediss://") {
		ensureRedisTLSConfig(opts, cfg.InsecureSkipVerify)
	} else if cfg.InsecureSkipVerify && opts.TLSConfig != nil {
		opts.TLSConfig.InsecureSkipVerify = true
	}
	// Honor callers' cache, authentication and lease deadlines during socket
	// I/O as well as pool waits. Otherwise go-redis uses Background for I/O.
	opts.ContextTimeoutEnabled = true
	return opts, nil
}

func isRedisURL(addr string) bool {
	lower := strings.ToLower(addr)
	return strings.HasPrefix(lower, "redis://") || strings.HasPrefix(lower, "rediss://")
}

func ensureRedisTLSConfig(opts *redis.Options, insecureSkipVerify bool) {
	if opts.TLSConfig == nil {
		opts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: redisServerName(opts.Addr),
		}
	}
	if opts.TLSConfig.MinVersion == 0 {
		opts.TLSConfig.MinVersion = tls.VersionTLS12
	}
	if opts.TLSConfig.ServerName == "" {
		opts.TLSConfig.ServerName = redisServerName(opts.Addr)
	}
	if insecureSkipVerify {
		opts.TLSConfig.InsecureSkipVerify = true
	}
}

func redisServerName(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(addr, "[]")
}

func redisConnectionHint(err error, tlsEnabled bool) string {
	if tlsEnabled || err == nil {
		return ""
	}
	if err == io.EOF || strings.Contains(strings.ToLower(err.Error()), "eof") {
		return "(if you use a cloud Redis such as Aiven or Upstash, use a rediss:// address or set REDIS_TLS=true to enable TLS)"
	}
	return ""
}

// RedactRedisAddr masks credentials in redis:// / rediss:// URLs for logs.
func RedactRedisAddr(addr string) string {
	parsed, err := url.Parse(addr)
	if err != nil || parsed.User == nil || !isRedisURL(addr) {
		return addr
	}
	username := parsed.User.Username()
	if _, hasPassword := parsed.User.Password(); hasPassword {
		parsed.User = url.UserPassword(username, "redacted")
	} else {
		parsed.User = url.User(username)
	}
	return parsed.String()
}

// Close closes the Redis connection
func (tc *redisTokenCache) Driver() string {
	return "redis"
}

func (tc *redisTokenCache) Label() string {
	return "Redis"
}

func (tc *redisTokenCache) Close() error {
	return tc.client.Close()
}

// Ping checks Redis connectivity
func (tc *redisTokenCache) Ping(ctx context.Context) error {
	return tc.client.Ping(ctx).Err()
}

// Stats returns the Redis connection pool status
func (tc *redisTokenCache) Stats() PoolStats {
	stats := tc.client.PoolStats()
	return PoolStats{
		TotalConns:      stats.TotalConns,
		IdleConns:       stats.IdleConns,
		StaleConns:      stats.StaleConns,
		WaitCount:       stats.WaitCount,
		WaitDurationNs:  stats.WaitDurationNs,
		Timeouts:        stats.Timeouts,
		PendingRequests: stats.PendingRequests,
	}
}

// PoolSize returns the connection pool size setting
func (tc *redisTokenCache) PoolSize() int {
	return tc.client.Options().PoolSize
}

// SetPoolSize sets the connection pool size (go-redis does not support runtime changes, so it takes effect after a restart)
// This method only stores the setting for persistence; it takes effect only after the container restarts
func (tc *redisTokenCache) SetPoolSize(n int) {
	// go-redis v9 PoolSize cannot be changed after creation
	// This only records the value; after a restart, main.go uses the value saved in the database
	_ = n
}

// ==================== Access token cache ====================

func tokenKey(accountID int64) string {
	return fmt.Sprintf("codex:token:%d", accountID)
}

// GetAccessToken gets the cached AT
func (tc *redisTokenCache) GetAccessToken(ctx context.Context, accountID int64) (string, error) {
	val, err := tc.client.Get(ctx, tokenKey(accountID)).Result()
	if err == redis.Nil {
		return "", nil // cache miss
	}
	return val, err
}

// SetAccessToken caches the AT
func (tc *redisTokenCache) SetAccessToken(ctx context.Context, accountID int64, token string, ttl time.Duration) error {
	return tc.client.Set(ctx, tokenKey(accountID), token, ttl).Err()
}

// DeleteAccessToken deletes the cached AT
func (tc *redisTokenCache) DeleteAccessToken(ctx context.Context, accountID int64) error {
	return tc.client.Del(ctx, tokenKey(accountID)).Err()
}

// ==================== Distributed refresh lock ====================

func refreshLockKey(accountID int64) string {
	return fmt.Sprintf("codex:refresh_lock:%d", accountID)
}

// AcquireRefreshLock acquires the refresh lock (prevents concurrent refreshes of the same account)
func (tc *redisTokenCache) AcquireRefreshLock(ctx context.Context, accountID int64, ttl time.Duration) (bool, error) {
	ok, err := tc.client.SetNX(ctx, refreshLockKey(accountID), "1", ttl).Result()
	return ok, err
}

// ReleaseRefreshLock releases the refresh lock
func (tc *redisTokenCache) ReleaseRefreshLock(ctx context.Context, accountID int64) error {
	return tc.client.Del(ctx, refreshLockKey(accountID)).Err()
}

func redisLeaseKey(namespace, key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(namespace) + "\x00" + strings.TrimSpace(key)))
	return "codex:lease:" + hex.EncodeToString(sum[:])
}

func (tc *redisTokenCache) AcquireLease(ctx context.Context, namespace, key, owner string, ttl time.Duration) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return false, fmt.Errorf("lease owner is empty")
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return tc.client.SetNX(ctx, redisLeaseKey(namespace, key), owner, ttl).Result()
}

var releaseLeaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func (tc *redisTokenCache) ReleaseLease(ctx context.Context, namespace, key, owner string) error {
	return releaseLeaseScript.Run(ctx, tc.client, []string{redisLeaseKey(namespace, key)}, strings.TrimSpace(owner)).Err()
}

// ==================== Waiting for lock release ====================

// WaitForRefreshComplete waits for another process to finish refreshing (polls the lock and reads the cache)
func (tc *redisTokenCache) WaitForRefreshComplete(ctx context.Context, accountID int64, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		// Check whether the lock is still held
		exists, err := tc.client.Exists(ctx, refreshLockKey(accountID)).Result()
		if err != nil {
			return "", err
		}

		if exists == 0 {
			// The lock is released; try to read the new AT
			token, err := tc.GetAccessToken(ctx, accountID)
			if err != nil {
				return "", err
			}
			if token != "" {
				return token, nil
			}
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeoutTimer.C:
			return "", fmt.Errorf("timed out waiting for refresh")
		case <-ticker.C:
		}
	}
}

// ==================== Runtime-state cache ====================

func runtimeHashKey(prefix, raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return prefix + hex.EncodeToString(sum[:])
}

func sessionAffinityKey(raw string) string {
	return runtimeHashKey("codex:session:", raw)
}

func responseContextKey(responseID string) string {
	return runtimeHashKey("codex:response:", responseID)
}

func runtimeNamespace(namespace string) string {
	namespace = strings.ToLower(strings.TrimSpace(namespace))
	if namespace == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range namespace {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

func runtimeValueKey(namespace, raw string) string {
	return runtimeHashKey("codex:runtime:"+runtimeNamespace(namespace)+":", raw)
}

func (tc *redisTokenCache) SetSessionAffinity(ctx context.Context, key string, binding SessionAffinityBinding, ttl time.Duration) error {
	key = strings.TrimSpace(key)
	if key == "" || binding.AccountID == 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	payload, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	return tc.client.Set(ctx, sessionAffinityKey(key), payload, ttl).Err()
}

func (tc *redisTokenCache) GetSessionAffinity(ctx context.Context, key string) (SessionAffinityBinding, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return SessionAffinityBinding{}, false, nil
	}
	val, err := tc.client.Get(ctx, sessionAffinityKey(key)).Result()
	if err == redis.Nil {
		return SessionAffinityBinding{}, false, nil
	}
	if err != nil {
		return SessionAffinityBinding{}, false, err
	}
	var binding SessionAffinityBinding
	if err := json.Unmarshal([]byte(val), &binding); err != nil {
		return SessionAffinityBinding{}, false, err
	}
	if binding.AccountID == 0 {
		return SessionAffinityBinding{}, false, nil
	}
	return binding, true, nil
}

func (tc *redisTokenCache) DeleteSessionAffinity(ctx context.Context, key string, accountID int64) error {
	key = strings.TrimSpace(key)
	if key == "" || accountID == 0 {
		return nil
	}
	const script = `
local value = redis.call("GET", KEYS[1])
if not value then
  return 0
end
local ok, data = pcall(cjson.decode, value)
if ok and tonumber(data["account_id"]) == tonumber(ARGV[1]) then
  return redis.call("DEL", KEYS[1])
end
return 0
`
	return tc.client.Eval(ctx, script, []string{sessionAffinityKey(key)}, accountID).Err()
}

func (tc *redisTokenCache) SetResponseContext(ctx context.Context, responseID string, items []json.RawMessage, ttl time.Duration) error {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	normalizedItems, err := NormalizeResponseContextItems(items)
	if err != nil {
		return err
	}
	record := redisResponseContextRecord{Items: normalizedItems}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return tc.client.Set(ctx, responseContextKey(responseID), payload, ttl).Err()
}

// SetResponseContextReadOnly accepts immutable cache-owned item bodies. Neither
// normalization nor encoding modifies them, avoiding a full historical clone
// at the proxy's asynchronous handoff while preserving the normal wire format.
func (tc *redisTokenCache) SetResponseContextReadOnly(ctx context.Context, responseID string, items []json.RawMessage, ttl time.Duration) error {
	return tc.SetResponseContext(ctx, responseID, items, ttl)
}

func (tc *redisTokenCache) GetResponseContext(ctx context.Context, responseID string) ([]json.RawMessage, error) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil, nil
	}
	val, err := tc.client.Get(ctx, responseContextKey(responseID)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record redisResponseContextRecord
	if err := json.Unmarshal([]byte(val), &record); err != nil {
		return nil, err
	}
	items := make([]json.RawMessage, len(record.Items))
	for i, item := range record.Items {
		items[i] = append(json.RawMessage(nil), item...)
	}
	normalizedItems, err := NormalizeResponseContextItems(items)
	if err != nil {
		return nil, err
	}
	return normalizedItems, nil
}

// GetResponseContextBounded reads at most maxWireBytes+1 bytes so an oversized
// Redis value can be rejected before the complete value is fetched or decoded.
func (tc *redisTokenCache) GetResponseContextBounded(ctx context.Context, responseID string, maxWireBytes int64) (ResponseContextReadResult, error) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return ResponseContextReadResult{Status: ResponseContextReadMiss}, nil
	}
	if maxWireBytes < 0 {
		maxWireBytes = 0
	}
	val, err := tc.client.GetRange(ctx, responseContextKey(responseID), 0, maxWireBytes).Bytes()
	if err == redis.Nil {
		return ResponseContextReadResult{Status: ResponseContextReadMiss}, nil
	}
	if err != nil {
		return ResponseContextReadResult{}, err
	}
	if len(val) == 0 {
		return ResponseContextReadResult{Status: ResponseContextReadMiss}, nil
	}
	if int64(len(val)) > maxWireBytes {
		return ResponseContextReadResult{Status: ResponseContextReadTooLarge}, nil
	}
	var record redisResponseContextRecord
	if err := json.Unmarshal(val, &record); err != nil {
		return ResponseContextReadResult{Status: ResponseContextReadCorrupt}, nil
	}
	if len(record.Items) == 0 {
		return ResponseContextReadResult{Status: ResponseContextReadMiss}, nil
	}
	normalizedItems, err := NormalizeResponseContextItems(record.Items)
	if err != nil {
		return ResponseContextReadResult{Status: ResponseContextReadCorrupt}, nil
	}
	return ResponseContextReadResult{Status: ResponseContextReadFound, Items: normalizedItems}, nil
}

func (tc *redisTokenCache) SetRuntime(ctx context.Context, namespace string, key string, value json.RawMessage, ttl time.Duration) error {
	key = strings.TrimSpace(key)
	if key == "" || len(value) == 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	return tc.client.Set(ctx, runtimeValueKey(namespace, key), []byte(value), ttl).Err()
}

func (tc *redisTokenCache) GetRuntime(ctx context.Context, namespace string, key string) (json.RawMessage, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false, nil
	}
	val, err := tc.client.Get(ctx, runtimeValueKey(namespace, key)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return append(json.RawMessage(nil), val...), true, nil
}

func (tc *redisTokenCache) DeleteRuntime(ctx context.Context, namespace string, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	return tc.client.Del(ctx, runtimeValueKey(namespace, key)).Err()
}

var claimRuntimeOwnerScript = redis.NewScript(`
local previous = redis.call("GET", KEYS[1])
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[2])
return previous
`)

var compareAndRefreshRuntimeOwnerScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  redis.call("PEXPIRE", KEYS[1], ARGV[2])
  return 1
end
return 0
`)

var compareAndDeleteRuntimeOwnerScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func runtimeOwnerTTLMillis(ttl time.Duration) int64 {
	if ttl <= 0 {
		ttl = time.Minute
	}
	millis := ttl.Milliseconds()
	if millis < 1 {
		return 1
	}
	return millis
}

func (tc *redisTokenCache) ClaimRuntimeOwner(ctx context.Context, namespace, key string, owner []byte, ttl time.Duration) ([]byte, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(owner) == 0 {
		return nil, nil
	}
	result, err := claimRuntimeOwnerScript.Run(
		ctx,
		tc.client,
		[]string{runtimeValueKey(namespace, key)},
		owner,
		runtimeOwnerTTLMillis(ttl),
	).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	switch value := result.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(value), nil
	case []byte:
		return append([]byte(nil), value...), nil
	default:
		return nil, fmt.Errorf("unexpected runtime owner claim result %T", result)
	}
}

func (tc *redisTokenCache) CompareAndRefreshRuntimeOwner(ctx context.Context, namespace, key string, expected []byte, ttl time.Duration) (bool, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(expected) == 0 {
		return false, nil
	}
	result, err := compareAndRefreshRuntimeOwnerScript.Run(
		ctx,
		tc.client,
		[]string{runtimeValueKey(namespace, key)},
		expected,
		runtimeOwnerTTLMillis(ttl),
	).Int64()
	return result == 1, err
}

func (tc *redisTokenCache) CompareAndDeleteRuntimeOwner(ctx context.Context, namespace, key string, expected []byte) (bool, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(expected) == 0 {
		return false, nil
	}
	result, err := compareAndDeleteRuntimeOwnerScript.Run(
		ctx,
		tc.client,
		[]string{runtimeValueKey(namespace, key)},
		expected,
	).Int64()
	return result == 1, err
}

func (tc *redisTokenCache) IncrRuntimeCounters(ctx context.Context, namespace string, key string, deltas map[string]float64, ttl time.Duration) error {
	key = strings.TrimSpace(key)
	if key == "" || len(deltas) == 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	hashKey := runtimeValueKey(namespace, key)
	_, err := tc.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for field, delta := range deltas {
			if delta == 0 {
				continue
			}
			pipe.HIncrByFloat(ctx, hashKey, field, delta)
		}
		pipe.Expire(ctx, hashKey, ttl)
		return nil
	})
	return err
}

func (tc *redisTokenCache) GetRuntimeCounters(ctx context.Context, namespace string, key string) (map[string]float64, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil
	}
	raw, err := tc.client.HGetAll(ctx, runtimeValueKey(namespace, key)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]float64, len(raw))
	for field, value := range raw {
		parsed, convErr := strconv.ParseFloat(value, 64)
		if convErr != nil {
			continue
		}
		out[field] = parsed
	}
	return out, nil
}

// SharedAcrossInstances reports that the Redis runtime-state cache is shared across instances.
func (tc *redisTokenCache) SharedAcrossInstances() bool { return true }
