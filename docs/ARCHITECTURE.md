# Codex2API Architecture Design Document

This document describes the overall architecture design, core components and data flow of Codex2API.

## Table of Contents

- [Architecture Overview](#architecture-overview)
- [Core Components](#core-components)
- [Data Flow](#data-flow)
- [Scheduling System](#scheduling-system)
- [Storage Layer](#storage-layer)
- [Security Design](#security-design)

---

## Architecture Overview

### Overall Architecture Diagram

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Client                                 │
│  - Web browser (/admin)                                                       │
│  - API calls (/v1/*)                                                         │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Ingress layer                                │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  Nginx / Load balancer                                                  │   │
│  │  - SSL termination                                                        │   │
│  │  - Static resource cache                                                    │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Codex2API service (Go + Gin)                          │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  HTTP routing layer                                                        │   │
│  │  ├── /admin/*     → React SPA (go:embed)                          │   │
│  │  ├── /api/admin/* → Admin API (admin.Handler)                       │   │
│  │  ├── /v1/*        → Public API (proxy.Handler)                       │   │
│  │  └── /health      → Health check                                       │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                       │                                     │
│  ┌────────────────────────────────────┼─────────────────────────────────┐  │
│  │           Middleware layer                                    │  │
│  │  ┌────────────────┐  ┌───────────┐ │  ┌────────────────────────────┐ │  │
│  │  │ Recovery       │  │ Logger    │ │  │ API Key Auth              │ │  │
│  │  │ ( panic recovery )  │  │ (request log) │ │  │ (public API authentication)            │ │  │
│  │  └────────────────┘  └───────────┘ │  └────────────────────────────┘ │  │
│  │  ┌────────────────┐  ┌───────────┐ │  ┌────────────────────────────┐ │  │
│  │  │ Admin Auth     │  │ RPM Limit │ │  │ CORS (development environment)             │ │  │
│  │  │ (admin API authentication) │  │ (global rate limit) │ │  │                           │ │  │
│  │  └────────────────┘  └───────────┘ │  └────────────────────────────┘ │  │
│  └────────────────────────────────────┼─────────────────────────────────┘  │
│                                       │                                     │
│  ┌────────────────────────────────────▼─────────────────────────────────┐  │
│  │                         Business logic layer                                    │  │
│  │  ┌──────────────────────┐  ┌──────────────────────────────────────┐ │  │
│  │  │   Admin panel logic        │  │         Proxy forwarding logic                  │ │  │
│  │  │   (admin.Handler)    │  │        (proxy.Handler)               │ │  │
│  │  │  • Account CRUD         │  │                                     │ │  │
│  │  │  • Usage statistics          │  │  • Account selection (auth.Store)            │ │  │
│  │  │  • System settings          │  │  • Request translation (translator)            │ │  │
│  │  │  • Proxy pool management        │  │  • Upstream request (executor)              │ │  │
│  │  └──────────────────────┘  │  • Streaming handling (SSE)                   │ │  │
│  │                            └──────────────────────────────────────┘ │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
│                                       │                                     │
│  ┌────────────────────────────────────▼─────────────────────────────────┐  │
│  │                          Data access layer                                   │  │
│  │  ┌──────────────────┐  ┌──────────────┐  ┌──────────────────────┐   │  │
│  │  │   Database       │  │    Cache     │  │      Token Store     │   │  │
│  │  │  (PostgreSQL/    │  │ (Redis/      │  │  (in-memory account pool state)      │   │  │
│  │  │   SQLite)        │  │  Memory)     │  │                      │   │  │
│  │  │                  │  │              │  │  • Scheduling score           │   │  │
│  │  │ • Accounts       │  │ • Session    │  │  • Concurrency count           │   │  │
│  │  │ • Usage Logs     │  │ • Rate Limit │  │  • Health status           │   │  │
│  │  │ • Settings       │  │              │  │  • Cooldown time           │   │  │
│  │  └──────────────────┘  └──────────────┘  └──────────────────────┘   │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Upstream service (Codex/OpenAI)                            │
│  - api.openai.com                                                          │
│  - api.codex.openai.com                                                    │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Core Components

### 1. Routing handler (Handler)

#### proxy.Handler - public API handler

```go
type Handler struct {
    store      *auth.Store          // account pool
    configKeys map[string]bool      // static API keys
    db         *database.DB         // database
    dbKeys     map[string]bool      // dynamic API key cache
    dbKeysUntil time.Time           // cache expiry time
}
```

**Responsibilities:**
- `/v1/chat/completions` - Chat Completions endpoint
- `/v1/responses` - Responses endpoint
- `/v1/models` - Model list
- API key authentication
- Request translation (OpenAI → Codex)
- Response translation (Codex → OpenAI)

#### admin.Handler - admin panel handler

```go
type Handler struct {
    store          *auth.Store      // account pool
    cache          cache.TokenCache // cache
    db             *database.DB     // database
    rateLimiter    *proxy.RateLimiter // rate limiter
    cpuSampler     *cpuSampler      // CPU sampler
    // ... other fields
}
```

**Responsibilities:**
- Account management (CRUD, import/export)
- Usage statistics and logs
- System settings management
- Proxy pool management
- Operations monitoring data

### 2. Account scheduler (auth.Store)

```go
type Store struct {
    accounts          []*Account                    // write-side account list
    accountSnapshot   atomic.Pointer[accountListSnapshot] // read-only request snapshot
    fastScheduler     atomic.Pointer[FastScheduler] // index by priority/health tier
    schedulerEngine   atomic.Value                  // legacy/shadow/indexed
    availability      atomic.Pointer[availabilityHub]
    maxConcurrency    int64
}

type Account struct {
    DBID          int64             // database ID
    RefreshToken  string            // refresh token
    AccessToken   string            // access token
    Email         string            // email
    PlanType      string            // plan type

    // runtime state
    activeReqs    int64             // active request count
    totalReqs     int64             // total request count
    healthTier    HealthTier        // health tier
    schedulerScore float64          // scheduling score

    // sync mechanism
    mu            sync.RWMutex      // read-write lock
}
```

**Scheduling flow:**

```
┌─────────────────────────────────────────────────────────────┐
│                      Account selection flow                            │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│  1. Filter stage                                                │
│     • Exclude accounts with Status = error                            │
│     • Exclude accounts with HealthTier = banned                       │
│     • Exclude accounts in cooldown                                   │
│     • Exclude accounts with an empty AccessToken                           │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│  2. Scoring stage                                                │
│     • Compute health tier (healthy/warm/risky/banned)              │
│     • Compute scheduling score (100 baseline + penalties/rewards)                │
│     • Compute dynamic concurrency limit                                       │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│  3. Selection stage                                                │
│     • Exclude accounts that have reached the concurrency limit                                 │
│     • Sort by scheduling priority first                                     │
│     • Within the same priority, sort by health tier and scheduling score                       │
│     • Indexed mode uses a cursor or affinity hash within the highest priority/health tier         │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│  4. Use stage                                                │
│     • Increment the active request count                                       │
│     • Execute the upstream request                                           │
│     • Decrement the active request count                                       │
│     • Report success/failure                                          │
└─────────────────────────────────────────────────────────────┘
```

**Large account pool execution model:**

1. After a write-side change, the account set publishes an immutable pointer snapshot; request reads of the snapshot no longer copy `[]*Account`.
2. `indexed` buckets by scheduling priority and health tier, and in steady state usually checks only one candidate; for sparse API key/group rules, it builds bounded sub-pool caches on demand, while dense rules reuse the global index directly.
3. Account concurrency releases, cooldown recovery and outbox changes are broadcast over a generation channel. Requests waiting for an account subscribe to events and retry; they do not poll the whole pool at a fixed interval.
4. Database triggers write accounts, API keys, groups, proxies and settings that change routing into `scheduler_outbox`. Each instance batch-merges by watermark and incrementally updates its in-memory projection; an account-selection miss no longer triggers a request-level full-database reconcile.

`legacy` keeps the old scan implementation; `shadow` has the old path do the actual account selection and samples index availability at 1/64; only `indexed` makes the index result authoritative. The three modes can be switched at runtime without changing the `/v1/*` protocol.

The `scheduler` object of the operations endpoint `/api/admin/ops/overview` exposes cumulative account-selection time, the number of accounts scanned by the legacy path, waits/wake-ups, route sub-pool cache, shadow differences, and outbox watermark, backlog, lag and error count.

### 3. Request executor (proxy.Executor)

```go
// ExecuteRequest executes the upstream request
func ExecuteRequest(ctx context.Context, account *auth.Account,
    body []byte, sessionID, proxyURL string) (*http.Response, error)
```

**Features:**
- TLS fingerprint impersonation
- User-Agent randomization
- Proxy support (global/account/proxy pool)
- Connection pool reuse
- Persistent WebSocket connection pool, bounded by the account's dynamic concurrency limit
- SSE stream reading

If the upstream WS returns close 1009 before the first downstream content, the executor keeps the current account lease and the resolved proxy, and degrades to HTTP at most once. This path does not re-enter account scheduling; once content has been output, there is no degradation.

### 4. Protocol translator (proxy.Translator)

```go
// TranslateRequest OpenAI Chat → Codex Responses
func TranslateRequest(body []byte) ([]byte, error)

// TranslateStreamChunk Codex SSE → OpenAI SSE
func TranslateStreamChunk(data []byte, model, chunkID string) ([]byte, bool)
```

**Conversion details:**
- `messages` → `input`
- `max_tokens/temperature` → removed (Codex does not support them)
- `reasoning_effort` → `reasoning.effort`
- SSE event type conversion

---

## Data Flow

### Request processing flow

```
┌─────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│ Client  │────▶│   Router    │────▶│  Middleware │────▶│   Handler   │
└─────────┘     └─────────────┘     └─────────────┘     └──────┬──────┘
                                                               │
                    ┌──────────────────────────────────────────┼──────────┐
                    │                                          │          │
                    ▼                                          ▼          ▼
            ┌──────────────┐                         ┌──────────────┐  ┌─────────┐
            │ Account Store│                         │   Executor   │  │  Usage  │
            │   (Select)   │────────────────────────▶│   (Upstream) │  │ Logger  │
            └──────────────┘                         └──────┬───────┘  └─────────┘
                                                            │
                                                            ▼
                                                    ┌──────────────┐
                                                    │    Codex     │
                                                    │    API       │
                                                    └──────────────┘
```

### Account pool state management

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          Account state machine                                      │
│                                                                         │
│    ┌──────────┐                                                        │
│    │  Initial │                                                        │
│    └────┬─────┘                                                        │
│         │ Import/Add                                                    │
│         ▼                                                               │
│    ┌──────────┐     ┌──────────┐     ┌──────────┐                      │
│    │  ready   │────▶│ cooldown │────▶│  ready   │                      │
│    └────┬─────┘     └──────────┘     └──────────┘                      │
│         │          (429/401 cooldown)       (cooldown ends)                       │
│         │                                                               │
│         │ Error                                                        │
│         ▼                                                               │
│    ┌──────────┐     ┌──────────┐                                       │
│    │  error   │────▶│ deleted  │                                       │
│    └──────────┘     └──────────┘                                       │
│                    (soft delete)                                              │
└─────────────────────────────────────────────────────────────────────────┘
```

### Cooldown mechanism

```go
// Cooldown types and durations
type CooldownRule struct {
    RateLimited  time.Duration  // 429 rate limit: parsed from response headers or inferred from the plan type
    Unauthorized time.Duration  // 401 unauthorized: 5 minutes ~ 24 hours
    Timeout      time.Duration  // timeout: 15 minutes
    ServerError  time.Duration  // 5xx error: 15 minutes
}

// Cooldown policy by plan type
const (
    FreeCooldown     = 7 * 24 * time.Hour  // Free plan 429: 7 days
    TeamCooldown5h   = 5 * time.Hour       // Team 5h window used up: 5 hours
    TeamCooldown7d   = 7 * 24 * time.Hour  // Team 7d window used up: 7 days
)
```

---

## Scheduling System

### Health Tier

```go
type HealthTier int

const (
    Healthy HealthTier = iota  // healthy
    Warm                        // warm (minor recent issues)
    Risky                       // risky (major recent issues)
    Banned                      // banned
)
```

**Tier criteria:**

| Tier | Condition | Dynamic concurrency |
|------|------|----------|
| Healthy | No recent errors | MaxConcurrency |
| Warm | Minor errors | MaxConcurrency / 2 |
| Risky | Severe errors | 1 |
| Banned | 401 unauthorized | 0 |

### Scheduling score calculation

```go
type SchedulerScore struct {
    BaseScore float64 = 100

    // penalty terms (P)
    UnauthorizedPenalty float64  // 401: -50, 24h decay
    RateLimitPenalty    float64  // 429: -22, 1h decay
    TimeoutPenalty      float64  // timeout: -18, 15min decay
    ServerPenalty       float64  // 5xx: -12, 15min decay
    FailurePenalty      float64  // consecutive failures: -6 each, max -24
    UsagePenalty7d      float64  // 7d usage: ≥70% -8, ≥100% -40
    LatencyPenalty      float64  // latency: ≥5s -4, ≥20s -15
    SuccessRatePenalty  float64  // success rate: <75% -8, <50% -15

    // reward terms (B)
    SuccessBonus        float64  // consecutive successes: +2 each, max +12
}

// Final score = BaseScore + ΣB - ΣP
```

### Scheduling algorithm pseudocode

```
function SelectAccount():
    candidates = []

    for account in accounts:
        // 1. Base filtering
        if account.Status == "error": continue
        if account.HealthTier == Banned: continue
        if account.IsInCooldown(): continue
        if account.AccessToken == "": continue

        // 2. Compute dynamic limits
        concurrencyLimit = calculateDynamicLimit(account.HealthTier)
        if account.ActiveRequests >= concurrencyLimit: continue

        // 3. Compute scheduling score
        account.SchedulerScore = calculateScore(account)

        candidates.append(account)

    if candidates is empty:
        return null

    // 4. Sort
    sort candidates by:
        - HealthTier (Healthy > Warm > Risky)
        - SchedulerScore (desc)
        - ActiveRequests (asc)

    // 5. Randomly shuffle (15% probability)
    if random() < 0.15 and len(candidates) > 1:
        return candidates[random(0, min(3, len(candidates)))]

    return candidates[0]
```

---

## Storage Layer

### Database schema

```sql
-- Accounts table
CREATE TABLE accounts (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    status VARCHAR(50) DEFAULT 'ready',
    credentials JSONB DEFAULT '{}',      -- email, refresh_token, access_token, etc.
    proxy_url VARCHAR(500),
    cooldown_until TIMESTAMP,
    cooldown_reason VARCHAR(50),
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW(),
    deleted_at TIMESTAMP                 -- soft delete
);

-- Usage log table
CREATE TABLE usage_logs (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT REFERENCES accounts(id),
    account_email VARCHAR(255),
    endpoint VARCHAR(100),
    model VARCHAR(100),
    status_code INTEGER,
    duration_ms INTEGER,
    first_token_ms INTEGER,
    prompt_tokens INTEGER DEFAULT 0,
    completion_tokens INTEGER DEFAULT 0,
    total_tokens INTEGER DEFAULT 0,
    reasoning_effort VARCHAR(20),
    stream BOOLEAN DEFAULT FALSE,
    service_tier VARCHAR(20),
    created_at TIMESTAMP DEFAULT NOW()
);

-- Hourly usage summary: aggregates usage_logs by UTC hour plus API key/channel/model/endpoint/status code/streaming/internal request.
-- Updated synchronously in the same transaction that writes the log, and cleared together with the logs; history from before the upgrade is backfilled in the background in id-based chunks.
-- Long-range statistics on the usage page and the self-service usage page read the hourly summary; only the partial hours at both ends scan the detail rows.
CREATE TABLE usage_log_hourly (
    bucket TIMESTAMPTZ NOT NULL,
    api_key_id BIGINT, api_key_name TEXT, api_key_masked TEXT, channel TEXT,
    model TEXT, effective_model TEXT, inbound_endpoint TEXT, endpoint TEXT,
    status_code INTEGER, stream BOOLEAN, internal INTEGER,
    requests BIGINT, total_tokens BIGINT, user_billed DOUBLE PRECISION, ...,
    PRIMARY KEY (bucket, api_key_id, api_key_name, api_key_masked, channel, model,
                 effective_model, inbound_endpoint, endpoint, status_code, stream, internal)
);

-- System settings table
CREATE TABLE system_settings (
    id INTEGER PRIMARY KEY DEFAULT 1,
    max_concurrency INTEGER DEFAULT 2,
    global_rpm INTEGER DEFAULT 0,
    test_model VARCHAR(100) DEFAULT 'gpt-5.5',
    test_concurrency INTEGER DEFAULT 50,
    proxy_url VARCHAR(500),
    pg_max_conns INTEGER DEFAULT 50,
    redis_pool_size INTEGER DEFAULT 30,
    response_cache_local_max_bytes BIGINT NOT NULL DEFAULT 67108864,
    response_cache_local_max_entry_bytes BIGINT NOT NULL DEFAULT 8388608,
    response_cache_reconstruct_max_bytes BIGINT NOT NULL DEFAULT 67108864,
    response_cache_config_generation BIGINT NOT NULL DEFAULT 1,
    auto_clean_unauthorized BOOLEAN DEFAULT FALSE,
    auto_clean_rate_limited BOOLEAN DEFAULT FALSE,
    admin_secret VARCHAR(255)
);

-- API keys table
CREATE TABLE api_keys (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(255),
    key_hash VARCHAR(255) UNIQUE NOT NULL,
    created_at TIMESTAMP DEFAULT NOW()
);

-- Proxy pool table
CREATE TABLE proxies (
    id BIGSERIAL PRIMARY KEY,
    url VARCHAR(500) NOT NULL,
    label VARCHAR(255),
    enabled BOOLEAN DEFAULT TRUE,
    last_tested_at TIMESTAMP,
    last_test_result VARCHAR(50),
    latency_ms INTEGER
);

-- Account events table (for trend analysis)
CREATE TABLE account_events (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT REFERENCES accounts(id),
    event_type VARCHAR(50),  -- added, deleted, status_changed
    event_data JSONB,
    created_at TIMESTAMP DEFAULT NOW()
);
```

### Cache strategy

| Data | Cache strategy |
| --- | --- |
| API key auth config | Default L1 15 seconds / Redis L2 5 minutes, isolated by database scope and transaction revision number; with the two-tier cache disabled, the legacy `api-key` strategy is used |
| API key request count, cost, token window statistics | Aggregated from the database and written to `api-key-limits`, TTL 60 seconds; multiple windows are read in batch |
| Shared usage delta for key × account | `api-key-scope-delta` minute buckets, TTL 5 minutes; the latest three buckets share a 5-second local read snapshot |
| Access Token | Dedicated token cache; write TTL is determined by the credential's validity period |
| Session affinity | Local binding and shared cache; controlled by session TTL and account availability |
| Account and model cooldown | Runtime cache; TTL matches the cooldown end time |

Runtime Redis keys are built with a namespace and a digest; business keys are not output directly as Redis keys. The RPM/RPD of an API key currently reads the aggregated usage snapshot; the strict weekly model budget is controlled by database transactions and request idempotency records.

API key runtime reads offer two optional batch interfaces: `RuntimeBatchReader` for JSON window snapshots (Redis `MGET`), and `RuntimeCounterBatchReader` for shared usage hashes (Redis Pipeline `HGETALL`). Each Redis batch holds at most 128 keys; the legacy `TokenCache` adapter can keep reading item by item. The memory driver copies the snapshot under the lock, so callers never get a reference that could modify the underlying cache.

Reading auth config first re-checks the database revision number (each process reuses it for at most 250 ms), then queries L1, Redis L2 and the database in turn. Config fetches from the origin are merged per key/local generation; L1 results copy the mutable fields for each caller. The cumulative `quota_used` does not enter the snapshot, and keys with a cumulative quota still query the database on every request; the weekly model budget keeps the authoritative transaction before forwarding.

The global revision number in `api_key_auth_cache_state` is committed in the same transaction as key configuration changes; row updates guarantee the commit order, and the maximum auto-increment ID of the outbox, which may commit out of order, is not used as the version. Usage updates do not advance the version. The Redis key contains the database scope, revision number and key digest, so a late backfill from an old request can only write the old version. The local generation advances synchronously on invalidation, preventing in-flight queries from putting results back into L1. Admin operations invalidate synchronously and send Pub/Sub; periodic checks of the persisted revision number cover missed notifications, lost messages and stale-version writes.

L1 is limited to 4,096 entries, a 16 MiB logical JSON byte budget, and a 64 KiB budget per entry; large entries can still serve the current request from the database, but are not cached. Memory mode does not copy a second L2. Config origin fetches and revision queries can both be cancelled and awaited on service shutdown; cancelling a single request does not interrupt reads shared by other callers. Auth Redis reads and writes have a 300 ms socket/context budget, and on failure fall back to the database; when the database revision number cannot be verified, return 503.

Group/account budgets use separate origin merging: a batch of reads is not interrupted when the first caller cancels; shared reads still have a two-second cap. Background shared incremental writes are limited to 16 slots, and fall back to synchronous writes when saturated.

#### Responses context cache

Consecutive `previous_response_id` requests use a single bounded L1 that is independent per process. When a response completes, the tail of up to 200 raw items is first kept by complete call/output groups, then written to L1; the default total is 64 MiB, per-entry admission is 8 MiB, up to 2,000 entries, absolute TTL 45 minutes. The LRU is bounded by both entry count and logical byte budget, and a hit does not extend the absolute TTL.

```text
Completed response
   │
   ├─ pair-safe tail trimming (up to 200 items)
   │
   ├─ In-process L1 (64 MiB total / 8 MiB entry / 2,000 entries / 45m)
   │
   └─ Redis shared response context (Redis mode only)

Consecutive previous_response_id request
   │
   ├─ L1 hit ───────────────────────────────▶ rebuild context
   │
   └─ L1 miss
       ├─ Bounded Redis read → normalize → rebuild limit check
       │    ├─ Meets L1 admission → serve the request and promote it to L1
       │    └─ Exceeds L1 but not the rebuild limit → serve this request only, no promotion
       └─ Memory mode → no shared response context fallback
```

The three database budgets separately control the L1 total, L1 per-entry admission, and the shared backend rebuild limit. Only a committed transaction that actually changes a budget allocates an incremented generation, applied immediately to the writing instance; same-value or empty updates do not increment it. Other instances read every 5 seconds, wait at most 3 seconds per read, and apply only newer generations. On read failure, the last valid configuration is kept and the sync error is recorded for display on the operations page.

When the memory cache is paired with a SQLite file main database, the local disk persistence layer (`cache/durable_response_context.go`) is the L2 by default: entries evicted from L1, exceeding the L1 admission budget, or still present after a process restart are recovered from disk, while native WebSocket shared-source records remain in-process only. With the persistence layer off, only L1 remains; when a known limit was exceeded or evicted, or a required context is missing or expired, consecutive Responses/Compact requests may return HTTP `409 previous_response_not_found`. A corrupted Redis value or one over the rebuild limit also returns 409; a shared backend transport failure, when the request really depends on that context, may return HTTP `503`. A context in Redis that is under the rebuild limit does not fail merely because it exceeds the L1 admission budget.

The client-native Responses WebSocket keeps `previous_response_id` on a healthy connection and sends only the current turn's delta upstream; recovery after the original connection is lost still depends on a complete local or shared snapshot, and is subject to the budgets, integrity and 409/503 error semantics above. The native Codex path for HTTP Responses/Compact must rebuild the parent reference within the current ownership.

The `response_cache` field of `GET /api/admin/ops/overview` returns, from a single locked snapshot, the effective/applied generation, the logical bytes of current/limit/high-water/maximum observed entries, local/remote hits and misses, expirations, entry/byte evictions, over-limit bypasses, Memory over-limit rejections and the final 409 count. The bytes here are the sum of retained JSON payload lengths; they exclude Go objects, LRU, allocator or container overhead, and are not a hard RSS limit. Process memory needs to be judged together with Linux/Docker RSS, Go `HeapAlloc` / `HeapInuse` / `HeapReleased` and `NumGC`.

---

## Security Design

### Authentication mechanism

```
┌─────────────────────────────────────────────────────────────┐
│                      Authentication levels                                │
├─────────────────────────────────────────────────────────────┤
│  Level 1: Transport layer                                             │
│  • HTTPS/TLS encryption                                           │
│  • HSTS header                                                │
├─────────────────────────────────────────────────────────────┤
│  Level 2: API authentication                                           │
│  • API Key Bearer Token                                     │
│  • 5-minute cache, reducing DB queries                                   │
├─────────────────────────────────────────────────────────────┤
│  Level 3: Admin authentication                                         │
│  • X-Admin-Key request header                                       │
│  • Supports environment variables or database configuration                                  │
│  • Audit logs for all admin operations                                      │
└─────────────────────────────────────────────────────────────┘
```

### Sensitive data handling

```go
// Token storage
func StoreToken(db *DB, accountID int64, token string) {
    // the database stores only the encrypted token
    encrypted := encrypt(token, encryptionKey)
    db.Exec("UPDATE accounts SET credentials = jsonb_set(credentials, '{access_token}', $1) WHERE id = $2",
        encrypted, accountID)

    // also cached in Redis (encrypted storage)
    redis.Set(ctx, fmt.Sprintf("token:%d", accountID), encrypted, 30*time.Minute)
}

// Log redaction
func SanitizeLog(msg string) string {
    // hide token details
    msg = regexp.MustCompile(`token=[^\s]+`).ReplaceAllString(msg, "token=***")
    msg = regexp.MustCompile(`Bearer [^\s]+`).ReplaceAllString(msg, "Bearer ***")
    return msg
}
```

### Rate limiting and protection

| Level | Mechanism | Description |
|------|------|------|
| Global | RPM rate limit | Based on the token bucket algorithm |
| API Key | RPM/RPD/cost/Token window | Database aggregation + 60-second statistics cache; RPM/RPD additionally have in-process admission counters |
| Downstream WS | Connections | 64 per key, 4096 globally, 16 per IP for anonymous clients |
| Account | Concurrency limit | Dynamically adjusted |
| IP | Connection limit | Prevents too many connections from a single IP |
| Application | Request size | Limits the request body size |

---

## Scalability Design

### Horizontal scaling

```
┌─────────────────────────────────────────────────────────────┐
│                      Load balancer                              │
│                   (Nginx / HAProxy)                         │
└─────────────────────────────────────────────────────────────┘
          │              │              │
          ▼              ▼              ▼
┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
│  Codex2API #1   │ │  Codex2API #2   │ │  Codex2API #3   │
│                 │ │                 │ │                 │
│  + PostgreSQL   │ │  + PostgreSQL   │ │  + PostgreSQL   │
│  + Redis        │ │  + Redis        │ │  + Redis        │
└─────────────────┘ └─────────────────┘ └─────────────────┘
          │              │              │
          └──────────────┼──────────────┘
                         ▼
              ┌─────────────────┐
              │   Shared Redis    │
              │  (Session/lock)   │
              └─────────────────┘
```

### Component replacement

| Component | Current implementation | Can be replaced with |
|------|----------|----------|
| Database | PostgreSQL | MySQL, TiDB |
| Cache | Redis | Memcached, local cache |
| HTTP | Gin | Echo, Fiber |
| Frontend | React | Vue, Svelte |
