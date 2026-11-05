package cache

import (
	"context"
	"encoding/json"
	"time"
)

// PoolStats is the unified representation of cache connection pool status.
// For the memory cache, these values are used to expose a consistent observability interface to the admin panel.
type PoolStats struct {
	TotalConns      uint32
	IdleConns       uint32
	StaleConns      uint32
	WaitCount       uint32
	WaitDurationNs  int64
	Timeouts        uint32
	PendingRequests uint32
}

// InUse excludes idle connections. StaleConns is a cumulative removal counter,
// not a subset of the current pool, and must not be subtracted here.
func (s PoolStats) InUse() uint32 {
	if s.IdleConns >= s.TotalConns {
		return 0
	}
	return s.TotalConns - s.IdleConns
}

type SessionAffinityBinding struct {
	AccountID int64  `json:"account_id"`
	ProxyURL  string `json:"proxy_url,omitempty"`
}

// ResponseContextReadStatus classifies a bounded shared-backend lookup without
// changing the TokenCache compatibility interface.
type ResponseContextReadStatus uint8

const (
	ResponseContextReadMiss ResponseContextReadStatus = iota
	ResponseContextReadFound
	ResponseContextReadTooLarge
	ResponseContextReadCorrupt
)

// ResponseContextReadResult is returned by optional bounded response-context
// readers. Transport failures remain ordinary errors.
type ResponseContextReadResult struct {
	Status ResponseContextReadStatus
	Items  []json.RawMessage
}

// BoundedResponseContextReader is an additive capability for shared backends.
// Implementations may reject oversized wire values before deserialization.
// TokenCache implementations that do not provide it remain supported through
// GetResponseContext followed by a logical-size check in the proxy.
type BoundedResponseContextReader interface {
	GetResponseContextBounded(ctx context.Context, responseID string, maxWireBytes int64) (ResponseContextReadResult, error)
}

// RuntimeOwnerStore is an optional atomic ownership capability used by
// cross-instance coordinators. Claim replaces the current owner and returns
// the previous value; refresh and delete are compare-and-act operations so a
// stale cleanup can never remove a newer owner.
type RuntimeOwnerStore interface {
	ClaimRuntimeOwner(ctx context.Context, namespace, key string, owner []byte, ttl time.Duration) ([]byte, error)
	CompareAndRefreshRuntimeOwner(ctx context.Context, namespace, key string, expected []byte, ttl time.Duration) (bool, error)
	CompareAndDeleteRuntimeOwner(ctx context.Context, namespace, key string, expected []byte) (bool, error)
}

// TokenCache is the unified interface for the token cache, refresh locks and short-lived runtime-state cache.
type TokenCache interface {
	Driver() string
	Label() string
	Close() error
	Ping(ctx context.Context) error
	Stats() PoolStats
	PoolSize() int
	SetPoolSize(n int)
	GetAccessToken(ctx context.Context, accountID int64) (string, error)
	SetAccessToken(ctx context.Context, accountID int64, token string, ttl time.Duration) error
	DeleteAccessToken(ctx context.Context, accountID int64) error
	AcquireRefreshLock(ctx context.Context, accountID int64, ttl time.Duration) (bool, error)
	ReleaseRefreshLock(ctx context.Context, accountID int64) error
	AcquireLease(ctx context.Context, namespace, key, owner string, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, namespace, key, owner string) error
	WaitForRefreshComplete(ctx context.Context, accountID int64, timeout time.Duration) (string, error)
	SetSessionAffinity(ctx context.Context, key string, binding SessionAffinityBinding, ttl time.Duration) error
	GetSessionAffinity(ctx context.Context, key string) (SessionAffinityBinding, bool, error)
	DeleteSessionAffinity(ctx context.Context, key string, accountID int64) error
	SetResponseContext(ctx context.Context, responseID string, items []json.RawMessage, ttl time.Duration) error
	GetResponseContext(ctx context.Context, responseID string) ([]json.RawMessage, error)
	SetRuntime(ctx context.Context, namespace string, key string, value json.RawMessage, ttl time.Duration) error
	GetRuntime(ctx context.Context, namespace string, key string) (json.RawMessage, bool, error)
	DeleteRuntime(ctx context.Context, namespace string, key string) error
	// IncrRuntimeCounters atomically adds to a group of float counters (multiple fields under the same key) and refreshes the expiry.
	// The Redis driver runs one pipelined HINCRBYFLOAT, to share short-window increments across instances;
	// the memory driver does this under a single lock, with the same semantics but visible only to this process.
	IncrRuntimeCounters(ctx context.Context, namespace string, key string, deltas map[string]float64, ttl time.Duration) error
	// GetRuntimeCounters reads all fields written by IncrRuntimeCounters. It returns nil when the key does not exist.
	GetRuntimeCounters(ctx context.Context, namespace string, key string) (map[string]float64, error)
	// SharedAcrossInstances reports whether the runtime-state cache is shared across instances (true for Redis, false for in-process memory).
	// Callers use it to decide whether the extra round-trip cost is worth paying for cross-instance consistency.
	SharedAcrossInstances() bool
}
