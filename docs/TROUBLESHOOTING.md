# Codex2API Troubleshooting Guide

This document provides troubleshooting methods and solutions for common Codex2API issues.

## Contents

- [Service startup issues](#service-startup-issues)
- [Database issues](#database-issues)
- [Account pool issues](#account-pool-issues)
- [API request issues](#api-request-issues)
  - [Responses context-continuation requests](#symptom-409-previous_response_not_found)
- [Performance issues](#performance-issues)
- [Network/proxy issues](#networkproxy-issues)
- [Log analysis](#log-analysis)
- [WS 1009 and first-event latency](#ws-1009-and-first-event-latency)

---

## Service startup issues

### Symptom: container fails to start

**Troubleshooting steps:**

1. **View the container logs**
```bash
docker compose logs -f codex2api
```

2. **Check common errors**

| Error message | Cause | Solution |
|----------|------|----------|
| `DATABASE_HOST is empty` | Database host not configured | Check the `.env` file |
| `REDIS_ADDR is empty` | Redis mode without an address configured | Check the `.env` file |
| `Redis connection failed: EOF` | Cloud Redis requires TLS, but it currently connects in plaintext | Use a `rediss://...` address, or set `REDIS_TLS=true` |
| `DATABASE_PATH is empty` | SQLite path not configured | Check the `.env` file |
| `connection refused` | Database not ready | Wait for dependent services to start |

**Health check script:**

```bash
#!/bin/bash
# health-check.sh

echo "Checking services..."

# Check container status
if ! docker compose ps | grep -q "Up"; then
    echo "ERROR: Containers not running"
    docker compose ps
    exit 1
fi

# Check the health endpoint
if ! curl -sf http://localhost:8080/health > /dev/null; then
    echo "ERROR: Health check failed"
    docker compose logs --tail=50 codex2api
    exit 1
fi

echo "All services healthy!"
```

### Symptom: port already in use

**Troubleshooting:**
```bash
# Check port usage
sudo lsof -i :8080
sudo netstat -tlnp | grep 8080

# Solution: change CODEX_PORT in .env
CODEX_PORT=8081
```

---

## Database issues

### Symptom: database connection fails

**PostgreSQL mode:**

```bash
# 1. Check the PostgreSQL container
docker compose ps postgres
docker compose logs postgres

# 2. Test the connection
docker exec -it codex2api-postgres psql -U codex2api -d codex2api -c "SELECT 1;"

# 3. Check the network
docker compose exec codex2api ping postgres
```

**SQLite mode:**

```bash
# 1. Check the data directory permissions
ls -la /path/to/sqlite/data/

# 2. Check the database file
sqlite3 /data/codex2api.db ".tables"

# 3. Fix the permissions
chmod 755 /data
chmod 644 /data/codex2api.db
```

### Symptom: poor database performance

**Troubleshooting:**

```bash
# View slow queries (PostgreSQL)
docker exec codex2api-postgres psql -U codex2api -c "
SELECT query, calls, mean_time, total_time
FROM pg_stat_statements
ORDER BY mean_time DESC
LIMIT 10;
"

# View the connection count
docker exec codex2api-postgres psql -U codex2api -c "
SELECT count(*) FROM pg_stat_activity;
"
```

**Optimization suggestions:**

1. Adjust the `PgMaxConns` setting
2. Add database indexes
3. Periodically clean up old logs

```sql
-- Add indexes for optimization
CREATE INDEX CONCURRENTLY idx_usage_logs_created_at ON usage_logs(created_at);
CREATE INDEX CONCURRENTLY idx_usage_logs_account_id ON usage_logs(account_id);
```

---

## Account pool issues

### Symptom: all accounts are unavailable

**Troubleshooting:**

```bash
# 1. Check account status
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/accounts | jq

# 2. View health check results
curl http://localhost:8080/health
```

**Common causes:**

| Status | Cause | Solution |
|------|------|----------|
| error | Account verification failed | Check the Refresh Token and refresh it manually |
| cooldown | Rate limit or error cooldown triggered | Wait for the cooldown to end or clear it manually |
| banned | 401 Unauthorized | Check whether the account has been banned |

If refreshing an account reports `unsupported_country_region_territory` or `Country, region, or territory not supported`, the refresh request was usually not sent out through an egress in a supported region. Check the account's own `proxy_url`, the proxy pool and the global `ProxyURL`. The internal refresh path applies in this order: `account proxy_url > group proxy > account ID sticky proxy pool > global ProxyURL`; **with the proxy pool enabled, it never connects directly**, and accounts bound to a disabled managed proxy refuse to refresh until that proxy is re-enabled.

**Batch refresh script:**

```bash
#!/bin/bash
# refresh-all.sh

ADMIN_KEY="your-secret"
BASE_URL="http://localhost:8080"

# Get all account IDs
ids=$(curl -s -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/accounts" | jq -r '.accounts[].id')

# Refresh one by one
for id in $ids; do
    echo "Refreshing account $id..."
    curl -s -X POST -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/accounts/$id/refresh"
    sleep 1
done
```

### Symptom: scheduling is abnormal and always selects the same account

**Troubleshooting:**

1. Check the distribution of account health tiers
2. View the scheduling score details

```bash
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/accounts | jq '.accounts[] | {id, health_tier, scheduler_score}'
```

**Solutions:**

- Trigger account re-scoring: restart the service or wait for the automatic recovery probe
- Check whether any account has stayed in the cooldown state for a long time

---

## API request issues

### Symptom: 401 Invalid API Key

**Troubleshooting:**

```bash
# 1. Check whether an API Key is configured
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/keys

# 2. Authentication is still required by default; anonymous access to regular public endpoints is only allowed when no key is configured and CODEX_ALLOW_ANONYMOUS=true is explicitly set
# 3. If configured, confirm the request header format
curl -H "Authorization: Bearer sk-your-key" http://localhost:8080/v1/models
```

### Symptom: 429 Too Many Requests

**Troubleshooting:**

```bash
# Check the global RPM setting
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/settings | jq '.global_rpm'

# Check the account pool status
curl http://localhost:8080/health
```

**Causes:**

1. **Global RPM limit**: increase `global_rpm` or set it to 0
2. **Account pool exhausted**: all accounts are in cooldown
3. **Per-account concurrency limit**: increase `max_concurrency`

### Symptom: 502 Bad Gateway

**Troubleshooting:**

```bash
# View the error log
docker compose logs -f codex2api | grep -i "upstream\|error"

# Check whether the account token has expired
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/accounts | jq '.accounts[].status'
```

**Common causes:**

- The upstream Codex API is unavailable
- The account's Access Token has expired
- Network connection problem

### Symptom: 503 Service Unavailable

**Troubleshooting:**

```bash
# Check the number of available accounts in the pool
curl http://localhost:8080/health

# Check whether any accounts are available
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/accounts | jq '.accounts[] | select(.status == "ready")'
```

**Solutions:**

1. Add new accounts
2. Clear accounts in the cooldown state
3. Wait for the cooldown to end

### Symptom: 409 previous_response_not_found

This error means the context required by `previous_response_id` has been determined to be impossible to rebuild on the current path (older versions returned the error code `response_context_unavailable`). Common causes include: in Memory mode with the disk persistence layer turned off, a single context exceeds the L1 admission budget; the dependent context has been evicted by the count or byte LRU; required context is missing or has expired (including exceeding the persistence retention period); or the backend value is corrupted or exceeds the rebuild limit. The `[response-context] unavailable` line in the log gives the specific reason.

```bash
# View the current budget, logical usage, evictions and unavailable counts
curl -s -H "X-Admin-Key: your-secret" \
  http://localhost:8080/api/admin/ops/overview |
  jq '.response_cache | {
    effective_config,
    applied_config,
    entries,
    max_entries,
    current_bytes,
    max_bytes,
    count_evictions,
    byte_evictions,
    oversize_bypasses,
    oversize_rejections,
    known_unavailable_errors,
    last_config_sync_at,
    last_config_sync_error
  }'
```

**Recommended actions:**

1. Do not retry unconditionally with the same `previous_response_id` once it has been determined to be unavailable; resend the complete required context or start a new response chain.
2. In Memory mode, focus on `oversize_rejections`, `count_evictions` and `byte_evictions`. The local total and per-entry admission can be adjusted on the settings page, but the total is not a hard RSS limit.
3. In Redis mode, focus on `last_config_sync_error`, the Redis health status and `reconstruct_max_bytes`. A shared value that is within the rebuild limit but exceeds L1 admission is counted in `oversize_bypasses`; it can still serve the request, and there is no need to enlarge L1 just for this counter.
4. When the log shows `[quota-failover] rejected`, the session-pinned account's quota is exhausted or it was automatically paused, and the request carries state that cannot be migrated to another account: `item_reference`, encrypted compaction items, other non-reasoning encrypted items, or tool output without a paired call. The `reason` in the log and the counters indicate the specific cause. Encrypted reasoning does not cause a rejection: it moves with the request to the new account, and if the new account rejects it with `invalid_encrypted_content`, the gateway strips the encrypted reasoning and retries once, keeping messages and tool calls intact.

### Symptom: previous response context backend returns 503

When a request really depends on the tool-call context of the previous response and the shared response-context backend is temporarily unavailable, it returns HTTP 503 with the error code `service_unavailable`.

```bash
curl -s -H "X-Admin-Key: your-secret" \
  http://localhost:8080/api/admin/ops/overview |
  jq '{redis: .redis, response_cache: {
    remote_hits: .response_cache.remote_hits,
    remote_misses: .response_cache.remote_misses,
    last_config_sync_at: .response_cache.last_config_sync_at,
    last_config_sync_error: .response_cache.last_config_sync_error
  }}'
```

First check the Redis network, TLS, authentication and connection pool, then retry with a backoff strategy. Backend transport errors are not counted in `remote_misses`, because they are not explicit cache misses.

---

## Performance issues

### Symptom: high response latency

**Troubleshooting:**

```bash
# 1. Check system resources
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/ops/overview | jq '.cpu, .memory, .postgres, .redis'

# 2. View the latency distribution
curl -s -H "X-Admin-Key: your-secret" "http://localhost:8080/api/admin/usage/logs?start=2024-01-01T00:00:00Z&end=2024-01-02T00:00:00Z" | jq '.logs[] | .duration_ms'

# 3. Check the database connection pool
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/ops/overview | jq '.postgres'
```

**Optimization suggestions:**

1. **Increase the connection pool size**
```bash
# Admin panel settings
PgMaxConns: 100
RedisPoolSize: 50
```

2. **Switch to the indexed scheduler**
```bash
CODEX_SCHEDULER_ENGINE=indexed
```

3. **Optimize the proxy configuration**
- Use a closer proxy node
- Enable the proxy pool

### Symptom: RPM unchanged, but CPU rises as accounts are added or pegs every core

First look at the scheduling hot path rather than only the total RPM:

```bash
curl -s -H "X-Admin-Key: your-secret" \
  http://localhost:8080/api/admin/ops/overview |
  jq '.scheduler | {
    engine,
    selection_total,
    selection_fast_hit,
    selection_slow_hit,
    slow_scanned_accounts,
    waiters,
    wait_wakeups,
    routing_cache_entries,
    routing_cache_accounts,
    shadow_checks,
    shadow_mismatches,
    outbox_backlog,
    outbox_lag_ms,
    outbox_errors
  }'
```

How to judge:

- `engine=legacy` and `slow_scanned_accounts` growing quickly with requests means CPU is mostly spent on the O(N) full-pool filtering and scoring done on every account selection.
- With `engine=indexed`, if `selection_slow_hit` or `slow_scanned_accounts` still grows, check whether the lazy fallback is enabled, or whether there are call paths that have not been migrated yet.
- `routing_cache_entries=0` is not necessarily abnormal: only sparse API Keys whose candidate ratio is no more than about 1/4 of the full pool build a sub-pool; dense rules reuse the global index.
- If `outbox_backlog` keeps growing, `outbox_lag_ms > 5000`, or the error count increases, the cross-instance in-memory projection is lagging; check the database connection, triggers and instance logs first.
- `waiters` is high but `wait_wakeups` does not grow: check whether all accounts have been permanently disabled; normal concurrency release or cooldown recovery should produce wake-up events.

For a production switch, first set it to `shadow` in the admin panel and observe under real traffic whether `shadow_mismatches` stays at 0 or can be explained by transient concurrency races, then switch to `indexed`. If account selection becomes abnormal, switch back to `legacy` immediately; the database outbox and maintenance job table can be kept, and there is no need to roll back the schema. The environment variable `CODEX_SCHEDULER_ENGINE` takes priority over the admin panel; if the page switch has no effect, check the container environment first.

### Scheduler outbox version requirements and rollback

- **The minimum PostgreSQL version is 11**: the outbox trigger uses the `CREATE TRIGGER ... EXECUTE FUNCTION` syntax (PG 10 and earlier only accept `EXECUTE PROCEDURE`), and the `credential_generation NOT NULL DEFAULT` column added to the `accounts` table is a metadata-only change only on PG 11+. Self-managed PG ≤ 10 deployments must upgrade the database before upgrading the service. The PostgreSQL version used by the official compose file is not affected.
- **Rolling back to an older binary**: the trigger stays in the database and keeps writing events to `scheduler_outbox`, but the older version has no consumer or cleanup task, so the table keeps growing. For a short-term rollback you can leave it alone (it is consumed and cleaned up automatically after upgrading again, or you can run `TRUNCATE scheduler_outbox` directly); for a long-term rollback, run the following cleanup (PostgreSQL):

```sql
DROP TRIGGER IF EXISTS scheduler_outbox_accounts_insert ON accounts;
DROP TRIGGER IF EXISTS scheduler_outbox_accounts_update ON accounts;
DROP TRIGGER IF EXISTS scheduler_outbox_accounts_delete ON accounts;
DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_insert ON api_keys;
DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_update ON api_keys;
DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_delete ON api_keys;
DROP TRIGGER IF EXISTS scheduler_outbox_groups_insert ON account_groups;
DROP TRIGGER IF EXISTS scheduler_outbox_groups_update ON account_groups;
DROP TRIGGER IF EXISTS scheduler_outbox_groups_delete ON account_groups;
DROP TRIGGER IF EXISTS scheduler_outbox_members_insert ON account_group_members;
DROP TRIGGER IF EXISTS scheduler_outbox_members_delete ON account_group_members;
DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_insert ON account_model_cooldowns;
DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_update ON account_model_cooldowns;
DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_delete ON account_model_cooldowns;
DROP TRIGGER IF EXISTS scheduler_outbox_proxies_insert ON proxies;
DROP TRIGGER IF EXISTS scheduler_outbox_proxies_update ON proxies;
DROP TRIGGER IF EXISTS scheduler_outbox_proxies_delete ON proxies;
DROP TRIGGER IF EXISTS scheduler_outbox_settings_update ON system_settings;
DROP FUNCTION IF EXISTS codex2api_scheduler_outbox_row();
DROP TABLE IF EXISTS scheduler_outbox;
DROP TABLE IF EXISTS maintenance_jobs;
```

For SQLite deployments, run the corresponding `DROP TRIGGER IF EXISTS <same-named trigger>;` and `DROP TABLE IF EXISTS scheduler_outbox; DROP TABLE IF EXISTS maintenance_jobs;` statements.

### Symptom: high memory usage

**Troubleshooting:**

```bash
# View container memory
docker stats codex2api --no-stream

# Compare process RSS, Go heap/GC and response-context logical bytes
curl -s -H "X-Admin-Key: your-secret" \
  http://localhost:8080/api/admin/ops/overview |
  jq '{memory: .memory, request_memory: .request_memory, response_cache: .response_cache, response_cache_writer: .response_cache_writer}'

# View Go memory profiles (if pprof is enabled)
curl http://localhost:8080/debug/pprof/heap > heap.prof
```

`response_cache.current_bytes`, `high_water_bytes` and `largest_entry_bytes` are logical bytes of the JSON payload. They do not include Go objects, slices, LRU, allocator or container overhead, cannot be added directly to RSS, and are not a hard process memory limit. On Linux/Docker, `memory.process_bytes` is the process RSS; on non-Linux systems it falls back to Go `Sys`. Use `heap_alloc_bytes`, `heap_inuse_bytes`, `heap_released_bytes` and `num_gc` together to judge heap growth and reclamation.

**Optimization:**

1. Limit log retention time
2. Also compare L1's `current_bytes` with `shared_payload_bytes`. Several snapshots can share a body, so lowering the logical budget mainly reduces replayable historical responses and may increase 409s in Memory mode, without necessarily lowering RSS proportionally.
3. Check the in-flight bytes, waiter count and rejection count of `request_memory` and `response_cache_writer`. `CODEX_REQUEST_MEMORY_BUDGET_MB` controls in-process body admission; it does not include all output, JSON working copies or Go heap overhead. Lowering it returns retryable 503/1013 responses sooner; raising it should be matched to the memory actually available.
4. Distinguish cumulative allocations per request from the live heap after the request ends. Merged stream flushing mainly lowers the flush frequency and cannot replace body lifecycle management; slow Redis and slow clients can also amplify in-flight memory.

---

## Network/proxy issues

### Symptom: upstream request timeout

**Troubleshooting:**

```bash
# 1. Test a direct connection to the Codex API
curl -v https://api.openai.com/v1/models -H "Authorization: Bearer your-token"

# 2. Test proxy connectivity
curl -x http://proxy:port https://api.openai.com/v1/models

# 3. Test inside the container
docker compose exec codex2api wget -O- https://api.openai.com/v1/models
```

**Solutions:**

1. Check the proxy configuration
2. Switch to a different proxy node
3. Increase the timeout (at the code level)

### Symptom: SSL/TLS errors

**Troubleshooting:**

```bash
# Check the certificate
echo | openssl s_client -connect api.openai.com:443 2>/dev/null | openssl x509 -noout -dates

# Update the CA certificates
docker compose exec codex2api apk add --no-cache ca-certificates
```

### Proxy test script

```bash
#!/bin/bash
# test-proxy.sh

PROXY_URL="http://proxy.example.com:8080"

echo "Testing proxy: $PROXY_URL"

# Test proxy connectivity
start_time=$(date +%s%N)
response=$(curl -s -o /dev/null -w "%{http_code}|%{time_total}" -x "$PROXY_URL" --max-time 10 https://api.openai.com/v1/models)
end_time=$(date +%s%N)

http_code=$(echo $response | cut -d'|' -f1)
time_total=$(echo $response | cut -d'|' -f2)

if [ "$http_code" = "200" ]; then
    echo "✓ Proxy is working (latency: ${time_total}s)"
else
    echo "✗ Proxy failed (HTTP $http_code)"
fi
```

---

## Log analysis

### Log levels

| Level | Meaning | Example |
|------|------|------|
| INFO | Normal operation | Account refresh succeeded, request completed |
| WARN | Warning | Account entered cooldown, request retried |
| ERROR | Error | Database connection failed, upstream error |
| FATAL | Fatal error | Startup failure, panic |

### Key log patterns

```bash
# Monitor errors in real time
docker compose logs -f codex2api | grep -i "error\|fail\|panic"

# Count status code distribution (per the log example; column 5 is the HTTP status code)
docker compose logs codex2api | awk '{print $5}' | sort | uniq -c | sort -rn

# Find slow requests (per the log example, column 6 is the latency, e.g. 523ms; here we filter requests over 1000ms)
docker compose logs codex2api | awk '{lat=$6; gsub(/ms/,"",lat); if (lat+0 > 1000) print $0}' | sort -k6,6n | tail -20

# Count requests per account (per the log example, the email is in square brackets and contains @)
docker compose logs codex2api | awk '{for(i=1;i<=NF;i++){if($i ~ /@/){gsub(/[\[\]]/,"",$i); print $i}}}' | sort | uniq -c | sort -rn
```

### Log field reference

```
2024/01/01 12:00:00 api/server.go:123: POST /v1/chat/completions 200 523ms gpt-5.5 effort=medium [user@example.com] [proxy1]
│                 │                │    │                        │   │     │          │          │                │
│                 │                │    │                        │   │     │          │          │                └── proxy used
│                 │                │    │                        │   │     │          └── account email
│                 │                │    │                        │   │     │          └── reasoning effort label
│                 │                │    │                        │   │     └── model name
│                 │                │    │                        │   └── response time
│                 │                │    │                        └── HTTP status code
│                 │                │    └── request path
│                 │                └── HTTP method
│                 └── source file and line number
```

### WS 1009 and first-event latency

The upstream WebSocket may return close 1009 only after waiting a long time. If no content has yet been sent to the client at that point, Codex2API keeps the same account and proxy and downgrades to HTTP once; after content has been sent, it does not downgrade.

Search for logs containing `WebSocket 1009 HTTP fallback attempt ended` and split the time by the following fields:

```text
fallback_id
source=peer_close|local_read_limit
attempt
account
endpoint
status
ws_elapsed_ms
http_elapsed_ms
http_first_event_ms
total_first_event_ms
total_elapsed_ms
```

`ws_elapsed_ms` is the WS wait that cannot be avoided before the fallback, and `http_first_event_ms` is the first-event time after the HTTP attempt really starts; the same `fallback_id` can link follow-up HTTP retries. There is currently no reliable evidence that 1009 has a uniform threshold tied to the token count or the final request body byte count, so when troubleshooting, do not guess from this or switch protocols early.

### Diagnostic script

```bash
#!/bin/bash
# diagnose.sh - comprehensive diagnostic script

BASE_URL="http://localhost:8080"
ADMIN_KEY="${ADMIN_KEY:-default}"

echo "========== Codex2API Diagnostic Report =========="
echo "Time: $(date)"
echo ""

echo "[1/6] Service health check..."
health=$(curl -sf "$BASE_URL/health" 2>/dev/null && echo "OK" || echo "FAILED")
echo "Health: $health"
if [ "$health" = "FAILED" ]; then
    echo "Error: service is not responding, check the container status"
    docker compose ps
    exit 1
fi
echo ""

echo "[2/6] Account pool status..."
curl -sf -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/stats" 2>/dev/null | jq .
echo ""

echo "[3/6] Database connection..."
db_status=$(curl -sf -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/ops/overview" 2>/dev/null | jq -r '.postgres.healthy')
echo "PostgreSQL: $db_status"
echo ""

echo "[4/6] Cache status..."
cache_status=$(curl -sf -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/ops/overview" 2>/dev/null | jq -r '.redis.healthy')
echo "Redis: $cache_status"
echo ""

echo "[5/6] System resources..."
curl -sf -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/ops/overview" 2>/dev/null | jq '{cpu: .cpu, memory: .memory, response_cache: .response_cache}'
echo ""

echo "[6/6] Recent errors..."
docker compose logs --since=1h codex2api 2>/dev/null | grep -i "error" | tail -5 || echo "No recent errors"
echo ""

echo "========== Diagnosis complete =========="
```

---

## Quick reference for common questions

### Q: How do I reset the Admin Secret?

```bash
# Method 1: via environment variable
docker compose down
export ADMIN_SECRET=new-secret
docker compose up -d

# Method 2: modify the database directly
docker exec -it codex2api-postgres psql -U codex2api -c "UPDATE system_settings SET admin_secret = 'new-secret';"
```

### Q: How do I clear all logs?

```bash
# Via the API
curl -X DELETE -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/usage/logs

# Or clear the database directly
docker exec codex2api-postgres psql -U codex2api -c "TRUNCATE usage_logs;"
```

### Q: How do I export all accounts?

```bash
curl -H "X-Admin-Key: your-secret" "http://localhost:8080/api/admin/accounts/export" > accounts_backup.json
```

### Q: Accounts show error after the service starts?

1. Check network connectivity
2. Refresh the account Token manually
3. Check whether the account has been banned

```bash
# Batch refresh
curl -X POST -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/accounts/batch-test \
  -H "Content-Type: application/json" \
  -d '{"ids": [1, 2, 3], "concurrency": 3}'
```

### Codex Token refresh and authorization expiry

By default, normal mode runs a background check every 2 minutes and uses the Refresh Token to renew when the remaining validity of the Access Token is less than 5 minutes. Codex accounts in quota cooldown are still renewed, but renewal does not lift the 5h/7d cooldown and does not send generation requests. Accounts that are disabled, manually paused, or explicitly authorization-expired are not forcibly restored by this mechanism.

Lazy mode does not actively renew by default. Turning on "Keep Codex authorization in lazy mode" under "Settings → Codex → Probe scheduling" keeps Codex credentials renewed separately, while still not enabling real responses probes. The account details show the most recent token renewal time and any renewal failure message. The `expires_in` returned by upstream is the validity period of the AT and must not be treated as a fixed validity period for the RT.

Refresh first records a persistent protection record, then exchanges the tokens, and updates the workspace credentials that use the same RT in the same database transaction. The result is published to memory and cache only after saving succeeds; a brief database write failure only retries the save and does not consume the RT again. Once the exchange has started, closing the admin page does not cancel the critical steps.

- `refresh_token_expired` / `refresh_token_revoked` / `refresh_token_invalidated`: authorization has expired; log in again and import the latest credentials. This is not the same as the account being banned.
- `refresh_token_reused`: first check whether another instance or client has rotated the same credentials. Concurrent refreshes within the project read and reuse the saved new credentials; external programs are not synced automatically.
- "The result of the previous Token refresh is unconfirmed" or "Failed to save the new Token": the upstream may have already consumed the old RT, but the local save could not be confirmed as complete. The protection record persists across restarts and does not allow the old RT to be consumed again because of a lock timeout or an account state reset. Re-authorize and import new credentials; do not delete the protection record to force a retry.

The upstream OAuth and the local database cannot form a single transaction; if a process crash or network interruption occurs between the exchange and the save, manual re-authorization may still be needed. When upgrading multiple instances, upgrade them all together before enabling refresh; old versions do not recognize the new persisted protection records.
