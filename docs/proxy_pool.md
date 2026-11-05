# Enhanced proxy pool management

This document describes the enhanced proxy pool management feature of codex2api.

## Overview

Enhanced proxy pool management provides the following core features:

1. **Proxy health checks**
   - Periodic HTTP HEAD checks
   - Response time monitoring
   - HTTP/SOCKS5 proxy support

2. **Proxy rotation strategies**
   - Round-robin
   - Weighted selection
   - Least connections

3. **Proxy pool status monitoring**
   - Healthy proxy list
   - Proxy statistics
   - Real-time status updates

4. **Account-level proxy priority**
   - A `proxy_url` configured on the account takes priority
   - Refresh Token refresh, test connection, and usage probes stick to a proxy pool egress selected by account ID
   - When the proxy pool is not enabled, falls back to the global `ProxyURL`

5. **Proxy performance statistics**
   - Success rate tracking
   - Latency monitoring
   - Active connection count
   - Total request count

6. **Automatic isolation of failing proxies**
   - Consecutive failure threshold detection
   - Automatically isolate failing proxies
   - Automatically recover after the isolation period ends

## Core data structures

### ProxyPool

```go
type ProxyPool struct {
    proxies   []*ProxyEntry
    stats     map[string]*ProxyStats
    healthy   []*ProxyEntry
    // ... configuration and runtime state
}
```

### ProxyEntry

```go
type ProxyEntry struct {
    URL                 string
    Healthy             bool
    LastCheck           time.Time
    Latency             time.Duration
    SuccessRate         float64
    Weight              int64
    ActiveConns         int64
    TotalRequests       int64
    FailedRequests      int64
    Status              ProxyHealthStatus
    IsolatedAt          time.Time
    ConsecutiveFailures int
}
```

### ProxyStats

```go
type ProxyStats struct {
    URL                 string
    Healthy             bool
    LastCheck           time.Time
    Latency             time.Duration
    LatencyMs           float64
    SuccessRate         float64
    Weight              int64
    ActiveConns         int64
    TotalRequests       int64
    FailedRequests      int64
    Status              string
    ConsecutiveFailures int
}
```

## API usage examples

### Create a proxy pool

```go
config := &auth.ProxyPoolConfig{
    Strategy:           auth.StrategyRoundRobin,
    CheckInterval:      30 * time.Second,
    Timeout:            10 * time.Second,
    IsolationThreshold: 3,
    IsolationDuration:  5 * time.Minute,
    HealthCheckURL:     "http://www.google.com/generate_204",
}

pool := auth.NewProxyPool(config)
```

### Add a proxy

```go
pool.AddProxy("http://proxy1:8080", 10)
pool.AddProxy("http://proxy2:8080", 5)
```

### Select a proxy

```go
// Use the default strategy (round-robin)
entry := pool.Select()
if entry != nil {
    fmt.Println("Selected proxy:", entry.URL)
}

// Use the weighted strategy
pool.SetStrategy(auth.StrategyWeighted)
entry := pool.Select()

// Use the least-connections strategy
pool.SetStrategy(auth.StrategyLeastConnections)
entry := pool.Select()
```

### Mark success or failure

```go
// Mark the proxy as successful
pool.MarkSuccess("http://proxy1:8080")

// Mark the proxy as failed
pool.MarkFailure("http://proxy1:8080")
```

### Get statistics

```go
stats := pool.GetStats()
for url, stat := range stats {
    fmt.Printf("Proxy: %s, Success Rate: %.2f, Latency: %.2fms\n",
        url, stat.SuccessRate, stat.LatencyMs)
}

status := pool.GetPoolStatus()
fmt.Printf("Total: %d, Healthy: %d, Isolated: %d\n",
    status.Total, status.Healthy, status.Isolated)
```

### Start health checks

```go
// Set the callback
pool.SetOnHealthCheck(func(result *auth.HealthCheckResult) {
    if !result.Healthy {
        log.Printf("Proxy %s health check failed: %v", result.URL, result.Error)
    }
})

pool.SetOnIsolation(func(entry *auth.ProxyEntry) {
    log.Printf("Proxy %s is isolated due to consecutive failures", entry.URL)
})

pool.SetOnRecovery(func(entry *auth.ProxyEntry) {
    log.Printf("Proxy %s has recovered", entry.URL)
})

// Start periodic health checks
pool.StartHealthCheck()

// Stop
pool.Stop()
```

### Connection management

```go
// Acquire a connection
if pool.AcquireConnection("http://proxy1:8080") {
    // Use the proxy...

    // Release the connection
    pool.ReleaseConnection("http://proxy1:8080")
}
```

## Strategies

### Round-robin

Selects each proxy in turn. Suited to evenly distributed load.

### Weighted selection

Selects randomly according to proxy weight; a higher-weight proxy has a greater chance of being selected. Suited to proxies with uneven performance.

### Least connections

Selects the proxy with the fewest active connections. Suited to long-lived connections, where it balances load better.

## Health check mechanism

1. **Periodic checks**: check all proxies at the configured interval
2. **Response time monitoring**: record the response time of each check
3. **Health status updates**: update each proxy's health status based on the check result
4. **Automatic isolation**: proxies that exceed the consecutive failure threshold are isolated automatically
5. **Automatic recovery**: after the isolation period ends, the proxy rejoins health checks and recovers automatically after a success

## Environment variables

```bash
# Proxy pool strategy: round_robin, weighted, least_connections
PROXY_POOL_STRATEGY=round_robin

# Health check interval
PROXY_POOL_CHECK_INTERVAL=30s

# Health check timeout
PROXY_POOL_TIMEOUT=10s

# Isolation threshold (consecutive failures)
PROXY_POOL_ISOLATION_THRESHOLD=3

# Isolation duration
PROXY_POOL_ISOLATION_DURATION=5m

# Health check URL
PROXY_POOL_HEALTH_CHECK_URL=http://www.google.com/generate_204
```

## Proxy resolution priority

Requests and internal refreshes (Refresh Token, account tests, usage probes) resolve their egress in this priority order:

```text
Account proxy_url > group proxy > account-ID sticky proxy pool > global ProxyURL > direct connection
```

Constraints (issue #517):

- When an account's `proxy_url` points to a managed proxy in the pool, the egress is pinned: after that proxy is disabled or fails testing, the account does not switch to another proxy and does not connect directly; it recovers automatically after re-enabling.
- Deleting a managed proxy unbinds the accounts that still reference it.
- Unbound accounts are sticky-selected from the currently enabled proxies that have not failed testing; if one becomes unavailable, switch to another entry in the pool.
- **Once the proxy pool is enabled, there is no direct connection.** When the pool is empty and there is no global `ProxyURL`, the account cannot be scheduled, requests return no available account, and refresh refuses a direct connection.
- A custom `proxy_url` that was never added to the proxy pool is still used as is.

Changes to the global `ProxyURL` or the proxy pool config take effect on subsequent requests immediately, without a service restart.

## Migrating proxy bindings with account export and import

Account export writes only credentials by default, so proxy bindings must be reconfigured after moving to a new machine. Add `include_proxy=1` to the export and add `import_proxy=true` to the import, so the "account pool plus proxy binding" can be moved as one unit. Codex `/accounts/import` supports this; for parameter details, see [API.md](API.md).

### Write order

The import side strictly runs in the order **write the proxy table, then reload the in-memory proxy pool, and only then write accounts**; if the reload fails midway, the entire import is aborted.

The order must not be reversed. The reason is the fail-closed constraint in the previous section: an account bound to a URL that is registered as a managed proxy but not yet in the enabled set is judged to have no usable egress and cannot be scheduled. Writing accounts first and then reloading the proxy pool opens exactly such a window between those two steps, and accounts imported during the window cannot be scheduled. Continuing to write accounts after a failed reload makes this window permanent.

### Proxy registration rules

- At most 500 entries are registered at once. When the limit is exceeded, **none are registered**, and all accounts fall back to the proxy filled in on the form. Registering half of them would leave the other half of the accounts bound to URLs that are not in the pool, and that half-applied state is much harder to explain than a full rollback.
- URLs only get whitespace trimming and validity checks, the same as manual adding; the scheme case is not rewritten, and no trailing slash is added or removed. The registered entry and the value written to the account's `proxy_url` are the same string, so the two sides are identical; a difference of one character, such as a trailing slash, makes them two different proxies and the account still fails closed. So the key is not "what form it is normalized to," but that "both sides must be exactly identical". Different spellings of the same proxy each get their own row, the same as manual adding.
- Entries with an invalid format are skipped, and their accounts fall back to the form proxy; one bad proxy does not ruin the whole import batch.
- Registration uses `ON CONFLICT (url) DO NOTHING`. A proxy with the same URL that already exists on the target side is **not** revived and its label is not changed. If it is disabled or in a failed-test state, the accounts bound to it are not scheduled, and the import response includes a warning.
- Newly registered proxies get the `imported-<YYYYMMDD-HHmm>` label, so they can be filtered by batch and cleaned up later on the proxy management page.
- Proxy registration happens before account writes, so **even if every account in the batch fails to import** (for example, invalid credentials), the proxies remain in the proxy table. This is an inevitable result of the order above, not a defect; clean them up by the `imported-` label.

### Enabled state and existing bindings

- Proxies marked disabled on the source side are **always imported as enabled**, and the response includes a warning. Copying the disabled state would make the accounts bound to them fail closed immediately, and the user would see "import succeeded but every account is unusable"; importing as enabled, plus an explicit notice, loses no information, and the operator disables them manually if needed.
- When an import hits an account that already exists on the target side, the proxy carried by the file **does not override** that account's existing binding; it only fills empty bindings. A proxy in the file is passive data, and the target side's bindings may already have been carefully assigned. The `proxy_url` filled in on the form is the operator's explicit rebinding intent, so it keeps the existing override semantics.

### Parts not included in account export

A proxy bound only to a group and delivered by that group is not the account's own binding, so account export does not carry it; the target side must first create a group with the same name.

## Store integration

The project provides `EnhancedProxyPool` and `StoreProxyPoolIntegration` for integrating with the existing Store:

```go
// Create the enhanced proxy pool
enhancedPool := auth.NewEnhancedProxyPool(db, config)

// Initialize
err := enhancedPool.Init(ctx)

// Get a proxy
proxyURL := enhancedPool.NextProxy()

// Mark success or failure
enhancedPool.MarkProxySuccess(proxyURL)
enhancedPool.MarkProxyFailure(proxyURL)
```

## File list

- `auth/proxy_pool.go` - core proxy pool implementation
- `auth/proxy_pool_test.go` - proxy pool unit tests
- `auth/proxy_pool_integration.go` - Store integration implementation
- `auth/proxy_pool_integration_test.go` - integration tests

## Testing

Run the tests:

```bash
cd /path/to/project
go test -v ./auth/
```

Check coverage:

```bash
go test -cover ./auth/
```
