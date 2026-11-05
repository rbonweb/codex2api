# Codex2API configuration guide

This document describes all Codex2API configuration options and what they do.

## Contents

- [Configuration layers](#configuration-layers)
- [Environment variable configuration](#environment-variable-configuration)
- [System settings (database)](#system-settings-database)
- [Automatic model and price updates](#automatic-model-and-price-updates)
- [API Key model weekly request budget](#api-key-model-weekly-request-budget)
- [Quality check and target groups](#quality-check-and-target-groups)
- [Codex outbound attestation](#codex-outbound-attestation)
- [Configuration file example](#configuration-file-examples)
- [Configuration precedence](#configuration-precedence)

---

## Quality check and target groups

The admin panel's "Quality check" applies to Codex accounts. "Default check for new accounts" only affects accounts created afterwards and does not change the check switch of existing accounts.

- When automatic action is set to "No automatic action" or "Pause business scheduling", new accounts join checks according to the default switch.
- When automatic action is set to "Switch group", new accounts do not join checks automatically if the target group does not exist.
- Upgrading replaces the new-account triggers in SQLite and PostgreSQL, but keeps the check switches of existing accounts.

## Automatic model and price updates

In the admin panel, go to "Model pricing → Price sync", turn on "Automatically update models and official prices", select the sources you need (OpenAI corresponds to Codex), and save the interval. It is off by default; the default interval is daily, and it can be set to hourly. After an upgrade, deployments that already have official price polling enabled automatically get model discovery for the selected channels.

Each round refreshes the model catalogs of the selected channels in parallel. All discovery tasks share a 30-second budget, and then official prices, including those for new models, are synced. Codex reads the official catalog and samples the account list by plan.

Clients do not need to request the model list first, and there is no need to edit the built-in model list or restart the service. Codex models can still be discovered through the official catalog without a ChatGPT account; actual call permission is decided by the account and the upstream.

Models that an administrator disabled in the Codex registry are not re-enabled. Account-list learning only adds models and never removes a model because a single account lacks permission. Old non-built-in entries from the official catalog follow the existing sync cleanup rules. Manual prices take precedence over synced prices. A discovery failure does not block the next price sync; failures are reported in sync warnings and the service log. New models without a published price temporarily use the existing fallback price, and take effect automatically once a later poll obtains a price.

Price sync reads the official HTML pages and identifies prices by column name; it does not parse Markdown. Collapsed OpenAI models are filled in from the data embedded in the page, and long-context prices are read from the official price component that the page references. Only the data is decoded; no scripts are executed. If the structure changes, incomplete prices are rejected and the reference-source fallback is used. Before writing, the table structure, value types, non-negative finite prices, cached-read prices not higher than the matching input prices, and the long-context tiers of known models are validated. When official scraping fails, the format changes, price validation fails, or a model has no price, the system tries **LiteLLM → models.dev → project default pricing.json** in order, for each model. If there is still no valid price, the model's old synced override is cleared and the built-in default pricing is restored. A higher-priority price that has already been fetched is not overridden by later sources, channels that are not selected are not affected, and administrator-defined prices always take precedence.

"Reference JSON sources" provides manual import from LiteLLM, models.dev, the project default, and custom URLs. LiteLLM per-token prices are converted to the page's unified USD / 1M tokens. Third-party catalogs only use OpenAI first-party entries, to avoid mixing in reseller prices. The automatic fallback order is fixed and does not rewrite manually imported URLs. A successful fallback and the use of built-in default prices are shown as regular hints in the sync result; the recent warnings only record fetch, parse, and validation errors. When the 2K/4K variants of gpt-image-2 have no separate price list, they inherit the base model's token prices, and custom prices can still be set for each variant separately.

Daybreak-derived aliases (such as `gpt-6-sol-daybreak-blue` and `gpt-6.1-sol-daybreak-blue`) sync the full standard, Fast, and long-context prices from their base model, and the reference JSON does not need to list the aliases separately. When an alias has an exact price entry, that entry is used first. Custom prices for an alias take precedence, and a custom price for the base model is not overridden by the alias sync price. When no base-model price is found, the fallback continues in the established source order. The current default snapshots of the official `gpt-daybreak-blue-latest` and `gpt-daybreak-red-latest` are `gpt-5.6-sol` and `gpt-5.6-cyber` respectively; Cyber uses its own public pricing and no longer falls back to Sol. Sources: [Daybreak guide](https://developers.openai.com/api/docs/guides/daybreak), [official pricing page](https://developers.openai.com/api/docs/pricing).

"Sync official prices now" only syncs prices; model discovery is performed by scheduled polling. New protocols, special permissions, or special billing rules may still require a program update.

## Codex outbound attestation

Codex HTTP, WebSocket, and Live share the outbound attestation service, which selects the platform based on the final User-Agent: On Windows it automatically sends a platform-unavailable status; Linux does not add the header automatically; macOS tries native DeviceCheck, if generation fails, it sends the matching inner error status for the desktop client. This project always generates the attestation, and it ignores the client and account custom `X-Oai-Attestation` headers; Live sideband reuses the connection result frozen by the server. No new environment variables or database settings are needed; see [platform matrix, Mac dependencies, and Live snapshot](CODEX_ATTESTATION.md).

## Configuration layers

Codex2API uses a three-layer configuration architecture:

```
┌─────────────────────────────────────────────────────────────┐
│  Layer 1: Environment variables / .env file                  │
│  - Database connection, port, and basic authentication       │
│  - Physical-layer infrastructure configuration               │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│  Layer 2: System settings (SystemSettings table)             │
│  - Business settings: concurrency, rate limits, test settings│
│  - Editable at runtime from the admin panel                  │
└─────────────────────────────────────────────────────────────┘
│  Layer 3: Runtime in-memory state                            │
│  - Account-pool state, scheduling scores, cooldown state     │
│  - Restored from the database after a restart                 │
└─────────────────────────────────────────────────────────────┘
```

---

## Per-image billing for image generation

In the admin panel, open **Model pricing** (`/admin/model-pricing`), find the image model, switch "User image generation billing" to "Per successful image", enter a per-image USD price greater than 0, and save. It is still token-based billing by default; restoring the default prices also restores token billing. For example, with a price of `$0.05`, returning 2 successful images charges `$0.10`, and a key with a `$10` quota has `$9.90` left.

- Billing is based on the number of successful images, not HTTP requests; failures, cancellations, and internal retries are not charged. For multi-image jobs that partly succeed, only the successful part is settled.
- The image API (`/v1/images/generations`, `/v1/images/edits`, including streaming) records billing after a successful response; workspace jobs are settled after the images are saved, and results that cannot be saved or decoded are not charged.
- Token prices continue to account for upstream cost (`account_billed`); user cost (`user_billed`) is calculated per image and is used for the key's cumulative quota, total spend, and group/account budgets, without adding token charges on top. Text-model requests that use the image tool embedded in Responses still use token billing.
- Each usage record stores the billing method, unit price, and number of images charged; changing prices does not recalculate historical records. Settings are matched by the model pricing key that is actually in effect: the dated and 2K/4K aliases of GPT Image 2.5 share the corresponding Flare/Sunburst prices; the base, 2K, and 4K models of GPT Image 2 can be set separately.
- The public workspace shows the per-image price of the selected model above the generate button, and the remaining quota next to the key. Quota uses the same asynchronous settlement mechanism; this is not a pre-deducted balance or a concurrent balance reservation, so concurrent or batch requests can still exceed the remaining quota.

## Environment variable configuration

### Core service settings

| Variable | Required | Default | Description |
|------|------|--------|------|
| `CODEX_PORT` | No | 8080 | HTTP service port |
| `BIND_HOST` | No | `127.0.0.1` (SQLite) / `0.0.0.0` (PostgreSQL) | Docker port publish bind address (not the process listen address, which is controlled by `CODEX_BIND`). The SQLite compose file defaults to `127.0.0.1`, so only the local machine can access it; the standard compose file defaults to `0.0.0.0`, which means all network interfaces |
| `CODEX_MAX_REQUEST_BODY_SIZE_MB` | No | 48 | HTTP request body limit. The admin panel's MP4 live wallpaper upload is at most 40 MB; the default leaves headroom for multipart uploads |
| `CODEX_REQUEST_MEMORY_BUDGET_MB` | No | At least 128 | Total logical body budget per process for HTTP/WS (MiB), covering bodies being read or decompressed, queued and in-flight bodies, and Realtime session bodies. The default is the larger of 128 and the per-request limit; an explicit value cannot be smaller than the per-request limit. Takes effect after restart. When the budget is insufficient, HTTP returns 503 with `Retry-After: 1`, and WS closes with code 1013. This is not a hard RSS limit; the streaming multipart path for account import is still handled under its separate import limit |
| `ADMIN_SECRET` | No | - | Admin panel login secret |
| `CODEX_ALLOW_ANONYMOUS` | No | `false` | When set to `true`, `/v1/*` can be called directly even if no external API key is configured (internal test environments only) |
| `CODEX_SCHEDULER_ENGINE` | No | Empty | Forced scheduler engine value: `legacy` / `shadow` / `indexed`. When set, it takes precedence over the database setting, which suits container-level gradual rollout or emergency rollback |
| `CODEX_SCHEDULER_MAX_WAITERS` | No | `4096` | Total limit on account-scheduling waiting requests for this instance. Positive integer; takes effect after restart. When the queue is full, a retryable 503 is returned immediately |
| `CODEX_SCHEDULER_MAX_WAITERS_PER_KEY` | No | `256` | Scheduling waiting limit per API key for this instance. Positive integer; takes effect after restart. Anonymous requests share one count |
| `FAST_SCHEDULER_ENABLED` | No | `false` | Legacy compatibility switch. When `CODEX_SCHEDULER_ENGINE` is not set and the database has no `SchedulerEngine`, `true` maps to `indexed` |
| `TZ` | No | UTC | Time zone, for example `Asia/Shanghai` |

### Codex upstream stability settings

| Variable | Required | Default | Description |
|------|------|--------|------|
| `CODEX_UPSTREAM_TRANSPORT` | No | `http` | Codex upstream protocol: `http` / `auto` / `ws`. Under `auto`, HTTP inbound requests still use the HTTP upstream |
| `CODEX_PROXY_URL` | No | - | Global proxy URL, for cases where all Codex upstream requests should share one proxy configuration |
| `USE_WEBSOCKET` | No | `false` | Legacy switch. When `CODEX_UPSTREAM_TRANSPORT` is not set, `true` is equivalent to `CODEX_UPSTREAM_TRANSPORT=ws` |
| `CODEX_TRANSPORT_MODE` | No | `standard` | Codex HTTP transport: the default is standard Go TLS; `utls_chrome` can roll back to the old Chrome uTLS behavior |
| `CODEX_WS_SEND_USER_AGENT` | No | `true` | Whether the WS handshake sends Codex `User-Agent`/`Version`; set to `false` to turn it off |
| `CODEX_WS_USAGE_FRAME_PERSIST_SECONDS` | No | `30` | Minimum interval, in seconds, between database writes of per-frame WS usage snapshots. Positive integer; takes effect after restart. The in-memory snapshot is updated on every frame; database writes are done by an asynchronous worker that merges writes per account and does not block the upstream response stream. Writes are still forced when crossing the auto-pause threshold, on a 5pp change, or at 100%. Large account pools can raise this value to match the database's capacity |
| `CODEX_WS_USAGE_FRAME_INGEST` | No | `true` | Whether to consume the connection-level `codex.rate_limits` / `codex.response.metadata` quota frames that the WS upstream sends after each round and at the final state. When set to `false`, it falls back to WHAM probes and HTTP response headers. Takes effect after restart |
| `CODEX_SESSION_AFFINITY_TTL` | No | `1h` | TTL for stickiness between a Codex session and its account/proxy. Accepts `1h`, `90m`, or seconds |
| `CODEX_COMPACTION_AFFINITY_TTL` | No | `168h` | Source-affinity TTL for encrypted compaction state. The cache stores only the SHA-256 digest of the ciphertext, the source account, and the compatibility domain; known state never moves across official Codex upstreams |
| `CODEX_FINGERPRINT_DEBUG` | No | `false` | Outputs redacted fingerprint-policy diagnostic logs; tokens are not logged |
| `CODEX_REQUEST_COMPRESSION` | No | Follows system setting | Overrides the system setting "Codex HTTP request body compression". `zstd` / `on` / `true` / `1` force it on; `off` / `false` / `0` force it off. If the value is unset or unrecognized, the system setting applies. This is a deployment-level escape valve: the whole host can still be switched when the database is unreachable or the admin panel cannot be opened |
| `CODEX_MULTI_AGENT_V2_COMPAT` | No | `false` | Experimental: enables plaintext parameter compatibility for the Multi-Agent v2 collaboration tool in the official Codex client. Applies only to native Codex accounts and takes effect after restart. When enabled, matching v2 sub-task bodies are sent in plaintext. See `docs/CODEX_MULTI_AGENT_V2_COMPAT.md` |
| `CODEX_TELEMETRY_ENABLED` | No | Follows system setting | When set to `false`, the admin panel's "Client telemetry" switch is ignored and simulated telemetry sending is forcibly disabled at the deployment level |
| `CODEX_STATSIG_API_KEY` | No | Built-in public key | Overrides the public Statsig SDK key shared by Codex Desktop and CLI; used only when telemetry is enabled |
| `CODEX_SESSION_HEADER_MODE` | No | `native` | Outbound session header format. `native` sends the real client's `session-id` / `thread-id` / `x-client-request-id`; `legacy` falls back to the old `Session_id` (WS also sends `Conversation_id`) |
| `CODEX_SESSION_HEADER_ALIGN_CONVERGED` | No | `false` | When enabled, the `session-id` header uses the session identity converged by fingerprinting, aligning it with the `session_id` in turn metadata. Off by default: the request body's `prompt_cache_key` is always kept isolated, but whether the upstream also uses this header for cache grouping cannot be confirmed from the client source code |
| `DOWNSTREAM_HTTP_KEEPALIVE_INTERVAL` | No | `30s` | Downstream HTTP/SSE keepalive period, using a Go duration; `0` turns it off. Streaming endpoints establish an SSE 200 from the first heartbeat and send a comment; non-streaming endpoints send HTTP 102, except Codex/GPT Images: their non-streaming requests send no interim response and wait directly for the final JSON |
| `DOWNSTREAM_WS_KEEPALIVE_INTERVAL` | No | `45s` | Downstream WebSocket ping period, using a Go duration; `0` turns it off. Covers Responses, Realtime, and Live Sideband |

The quota-frame read path only updates memory and the scheduler; writes to usage, credits, plan, and cooldown share a background merge path. When the concurrent observation lock is contended, each account keeps at most 64 pending partial frames, processed in their original time order. On overflow, the latest bounded tail is kept, and the authoritative snapshot is filled in through existing probes. On shutdown, unfinished cache writes are dropped, and probes rebuild them after restart. A failed cooldown write is retried on a later observation and is not counted again as a failure.

> `CODEX_UPSTREAM_TRANSPORT` only controls whether `http` or `ws` is used when HTTP inbound requests are forwarded to the Codex upstream. The client-side WebSocket entry point works independently: connect with `GET ws://<host>/v1/responses`, send a `response.create` JSON as the first frame, and the server returns Responses event frames through the Codex upstream WS.
>
> Under WS transport, the server consumes the `codex.rate_limits` frames the upstream sends each round, plus the connection-level usage frames re-sent 1–2 seconds after the response reaches its final state, and updates the account's 5h/7d usage snapshots in real time. Auto-pause thresholds take effect without waiting for the WHAM probe cycle. These frames are delivered with the conversation itself and do not create extra upstream requests.

### Database settings

#### PostgreSQL mode

| Variable | Required | Default | Description |
|------|------|--------|------|
| `DATABASE_DRIVER` | Yes | postgres | Fixed value: postgres |
| `DATABASE_URL` | No | - | Full connection string, for example `postgresql://USER:PASSWORD@HOST[:PORT]/DBNAME?sslmode=require` (you can paste the connection string provided by Neon or Render directly). When set, it takes precedence over the separate variables below; only `sslmode` is read from the query string. Cannot be used together with `DATABASE_DRIVER=sqlite` |
| `DATABASE_HOST` | Yes (when `DATABASE_URL` is not set) | - | PostgreSQL host address |
| `DATABASE_PORT` | No | 5432 | PostgreSQL port |
| `DATABASE_USER` | Yes | - | PostgreSQL username |
| `DATABASE_PASSWORD` | Yes | - | PostgreSQL password |
| `DATABASE_NAME` | Yes | - | PostgreSQL database name |
| `DATABASE_SCHEMA` | No | - | PostgreSQL schema; suited to setups such as Supabase where several projects share one database. When set, startup runs `CREATE SCHEMA IF NOT EXISTS` automatically and points every connection's `search_path` at this schema. Only letters, digits, and underscores are allowed, with a length of ≤63. Leave empty for the default (usually `public`).|
| `DATABASE_SSLMODE` | No | disable | SSL mode: disable/require/verify-full |

### Image workspace

| Variable | Required | Default | Description |
|------|------|--------|------|
| `CODEX_IMAGES_MAIN_MODEL` | No | `gpt-5.6-luna` | Deployment default for the text model that drives image generation. When a specific model is chosen under "Codex → Image generation settings" in the admin panel, that admin setting takes precedence |
| `IMAGE_ASSET_DIR` | No | `/data/images` | Server directory where the admin image workspace saves image files; for Docker deployments, persisting `/data` is recommended |
| `IMAGE_ASSET_PUBLIC_BASE_URL` | No | Empty | Public base for image proxy URLs, for example `https://cdn.example.com`. It only changes the returned address; a reverse proxy must forward `/p/img/` to Codex2Api |
| `IMAGE_ASSET_SIGNING_SECRET` | No | Random value | Persistent signing secret for image proxy URLs. Production should set a fixed random value so historical image links do not break after a service restart |
| `IMAGE_UPSCALER_ENDPOINT` | No | Empty | RealESRGAN service address, for example `http://image-upscaler:8090`. When set, `upscale=2k/4k` must be processed successfully by this service, otherwise the async job fails |
| `IMAGE_UPSCALER_FIT` | No | `inside` | RealESRGAN target-size fitting mode; `inside` or `cover` |
| `BACKGROUND_ASSET_DIR` | No | `/data/backgrounds` | Server directory for background image and MP4 uploads in the admin panel. When not set, files are saved first to the `backgrounds` directory next to `IMAGE_ASSET_DIR` |

### Log directory

| Variable | Required | Default | Description |
|------|------|--------|------|
| `LOG_DIR` | No | `logs` | Upstream error log directory; platforms that allow writes only to a temporary disk can set `/tmp/logs` |
| `LOG_DISABLED` | No | `false` | When set to `true`, file-based error logs and security audit logs are disabled |
| `SECURITY_LOG_DIR` | No | `${LOG_DIR}/security` | Security audit log directory; follows `LOG_DIR` when not set |

#### SQLite mode

| Variable | Required | Default | Description |
|------|------|--------|------|
| `DATABASE_DRIVER` | Yes | sqlite | Fixed value: sqlite |
| `DATABASE_PATH` | Yes | - | SQLite database file path, for example `/data/codex2api.db` |

### Cache settings

#### Redis mode

| Variable | Required | Default | Description |
|------|------|--------|------|
| `CACHE_DRIVER` | Yes | redis | Fixed value: redis |
| `REDIS_ADDR` | Yes | - | Redis address. Supports `redis:6379`, `redis://default:pass@host:6379/0`, and `rediss://default:pass@host:6379/0` |
| `REDIS_USERNAME` | No | - | Redis ACL username; can be left empty if the URL already includes a username |
| `REDIS_PASSWORD` | No | - | Redis password; can be left empty if the URL already includes a password |
| `REDIS_DB` | No | 0 | Redis database number |
| `REDIS_TLS` | No | false | Enables TLS for Redis addresses in `host:port` form; `rediss://` enables it automatically |
| `REDIS_INSECURE_SKIP_VERIFY` | No | false | Skips TLS certificate verification; only recommended for self-signed certificates or troubleshooting |

> Cloud Redis services such as Aiven and Upstash usually require TLS. Prefer the `rediss://...` connection string provided by the platform. If you only enter `host:port`, set `REDIS_TLS=true`, otherwise startup may fail with `Redis connection failed: EOF`.

#### Memory cache mode

| Variable | Required | Default | Description |
|------|------|--------|------|
| `CACHE_DRIVER` | Yes | memory | Fixed value: memory |

#### API key authentication cache

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `CODEX_API_KEY_AUTH_CACHE_ENABLED` | No | `true` | Enables authentication L1/L2; setting `false` and restarting restores the legacy authentication-cache policy |

When enabled, keys that have groups, model permissions, validity periods, and quota settings can also be cached. The read order is local L1 → Redis L2 → database; Memory mode has only L1. The absolute TTL of L1 is 15 seconds, with at most 4,096 entries and a 16 MiB logical JSON snapshot budget, and a single entry is limited to 64 KiB; oversized entries go directly to the source. The Redis L2 TTL is 5 minutes and is isolated by database scope, authentication revision number, and key digest. The cache does not store raw keys or used quota. Keys confirmed not to exist enter only the bounded L1, with a 2-second TTL. The byte budget does not include Go object, map, or allocator overhead, and it is not a process RSS limit.

Enabling, disabling, deleting, or changing a key's configuration advances the `api_key_auth_cache_state` revision number within the same database transaction. Writes of cumulative usage do not advance the revision number. Each instance re-checks this revision number during active authentication, and a check result is reused for at most 250 milliseconds. Admin operations on this instance clear its L1 cache at once and notify other instances through Redis Pub/Sub. If a notification is lost, or an older-version instance changes data, the database re-check still detects the change. The 250 milliseconds is the maximum reuse time for a revision result; it does not include in-flight requests or infrastructure latency. If the database re-check fails, the response is 503. The system does not use an old snapshot it cannot confirm, and it does not accidentally enable anonymous access.

A key with a cumulative quota still queries `quota_used` in the database on every authentication. The model weekly budget still runs the database-authoritative count and request-idempotency check before forwarding. The original TTL for window statistics and the check policy for long-lived connections that are already established do not change. A revision-number change evicts all authentication entries on this instance; frequent key-configuration changes increase cold-cache fetches from the source. Snapshot backfill uses version isolation and a local generation check, so a late-completing old query cannot restore permissions from a newer version.

On startup, the revision table and the PostgreSQL/SQLite triggers are created automatically, so no manual migration is needed. These tables and triggers are kept even when both cache levels are turned off, which makes mixed-version deployments easier. The legacy policy caches metadata only for keys with no access constraints, and merges identical key queries that happen at the same moment.

The `api_key_auth_cache` field of `GET /api/admin/ops/overview` provides the switch, L1 entry and byte counts, local and remote hits, database configuration load count, dynamic quota read count, revision re-check count, invalidations, evictions, over-limit bypasses, and error counts. When evaluating the benefit, look separately at configuration fetches from the source and dynamic quota queries.

#### Codex client versions

The admin panel's "System settings → Codex" can sync and display the app versions and bundled CLI pairings for each Desktop / VSCode architecture. macOS uses the official appcast. On Windows, a third-party mapping is preferred, and if it fails, the official MSIX is read partially. VSCode uses the official native VSIX. Each package reads at most 12 MiB, keeps alpha CLIs, caches, and history, and does not download whole packages for analysis.

Leave the CLI/app version empty to use automatic pairing; manually entered fields override independently. In `auto` mode, the minimum CLI version only filters existing pairings. When no pairing is available, it returns `codex_client_version_unavailable` and never raises the version artificially. See [real version pairing and Windows fallback](CODEX_CLIENT_VERSIONS.md).

#### Codex client telemetry

**Experimental feature, off by default.** When enabled, ordinary Responses requests made with Codex OAuth send client telemetry asynchronously, using the selected Codex Desktop/CLI fingerprint. Analytics events are sent to `chatgpt.com/backend-api/codex/analytics-events/events`, and OTLP metrics are sent to `ab.chatgpt.com/otlp/v1/metrics`. Failures do not affect the proxy response. The account's proxy address is used, and when Resin is enabled, it is sent through the reverse proxy in the same way as `/responses`. Note: events such as tool calls, file changes, and hooks are randomly simulated and are not observations of real requests, so they may not match the request flow visible on the upstream side. Deployers decide for themselves whether to enable it.

The admin panel's "System settings → Codex → Client telemetry" can be switched on and off in real time, with the field `codex_telemetry_enabled` (off by default for both fresh installs and upgrades). `CODEX_TELEMETRY_ENABLED=false` is a deployment-level forced-off switch that ignores the admin setting. `CODEX_STATSIG_API_KEY` can override the built-in public SDK key; Codex Desktop and Codex CLI currently use the same key.

Events are simulated according to the Codex CLI structure: the first observed thread uses `codex_thread_initialized`, each round generates `codex_turn_event`, and at the end 4 `codex_hook_run` events are generated. `codex_dynamic_tool_call_event` occurs randomly in 40% of rounds, and when it does, 50% of those also generate `codex_command_execution_event`. `codex_file_change_event` occurs randomly in 20% of rounds and also generates `codex_accepted_line_fingerprints`, whose `repo_hash` is always `null`. These random events do not parse commands, tool calls, or diffs in the request.

The native `codex_turn_steer_event` corresponds only to the App Server `turn/steer` RPC; Responses requests cannot be reliably identified, so it is not simulated. Ordinary turns always use `steer_count=0`, and historical assistant/tool content, `previous_response_id`, and recovery markers are never inferred as resumed. Title and guardian sub-flows use separate initialization events. The first OTLP batch sends the 62 startup metrics from the HAR file, and then every 60 seconds it sends incremental turn/hook/tool metrics produced in that round. Including the 4 names that appear only in later samples, it covers 66 names in total.

#### Window usage and connection pool

When an API key has several RPM/RPD/cost/token windows enabled, Redis reads the statistics caches for those windows with a single `MGET`. If an entry is missing, corrupted, or fails to read, the system still falls back to the database in the original order. The definitions of calendar-day and sliding windows, and the 60-second TTL of the statistics cache, do not change. The Memory driver provides the same batch-read semantics. The RPM/RPD admission point has, in addition to the cached value, an in-process atomic counter: RPM counts in one-second buckets, and RPD counts over a sliding 24-hour window. Records within the same minute are merged and expire at their last admission (conservatively keeping at most a little under one extra minute). This count does not depend on whether the current request writes a usage log, but it is per process and is not persisted. Historical quota across several instances or after a restart still needs a shared, persistent metering scheme. At most 8,192 keys are kept. When the table is full and there is no record that can be reclaimed, a new key returns `503`, and records that are still valid are not evicted.

The three shared per-minute buckets for group/account budgets are read together through a pipeline. Concurrent fetches from the source for the same key are merged into one, and the existing 5-second local snapshot is still reused. Shared increments start at most 16 background writes; when the slots are full, the caller writes synchronously and takes the backpressure. When Redis fails, the database usage aggregation remains the fallback.

The Redis `usage_percent` in the operations overview and runtime status shows this process's connection-pool usage: `(total_conns - idle_conns) / pool_size`. It does not show Redis server CPU, memory, or data hit rate. `stale_conns` is the cumulative number of removed connections and does not take part in the current usage calculation. `wait_count`, `wait_duration_ns`, and `timeouts` are cumulative connection-pool wait and timeout metrics, and `pending_requests` is the number of requests currently waiting for a connection, which can be used to decide whether the pool size needs adjusting.

---

## System settings (database)

System settings are stored in the `SystemSettings` table of the database and can be changed in real time through the admin panel at `/admin/settings`.

### Codex image generation settings

In the admin panel's "System settings → Codex → Image generation settings", you can choose the text model that drives image generation. The choice is saved automatically and takes effect immediately for image generation requests this instance builds afterwards, and is restored from the database after a restart.

The admin API `PUT /api/admin/settings` uses the `codex_images_main_model` field; an empty string means "use the deployment default". The priority is: admin configuration → `CODEX_IMAGES_MAIN_MODEL` → built-in `gpt-5.6-luna`. `GET /api/admin/settings` also returns the read-only `codex_images_default_main_model`, which shows the deployment default. A model name must be no longer than 128 bytes, must not contain whitespace or control characters, and cannot be an image model such as `gpt-image-*`.

The setting applies to the admin and public image workspaces, `/v1/images/generations`, `/v1/images/edits`, and `/v1/responses` requests whose top-level `model` is an image model. If a native Responses request explicitly sets a text `model`, that model is still used. The image model is chosen by the workspace or API request. When the upstream explicitly rejects the text driver, the Images path still retries in the existing candidate order.

### Model list read limit

`models_list_read_max_bytes` limits the maximum number of bytes read from a successful Codex OAuth model-list response. The default is `8,388,608` bytes (8 MiB). The admin panel shows it in whole MiB, and the allowed range is 1–256 MiB. When a response exceeds the limit, the request fails explicitly, and a truncated JSON is never parsed as a complete model list.

### Responses context cache

Consecutive Responses requests rebuild their context by `previous_response_id`. Each Codex2API process has a bounded L1 cache, and three byte budgets are stored in the database. The admin panel displays and edits them in whole MiB, and the admin API uses raw byte counts.

| Admin API field | Default | Settings page range | Description |
|------|------|------|------|
| `response_cache_local_max_bytes` | 67,108,864 bytes (64 MiB) | Integer, 8-4096 MiB | Total logical JSON payload size that the L1 of a single process can keep |
| `response_cache_local_max_entry_bytes` | 8,388,608 bytes (8 MiB) | Integer, 1-256 MiB | Limit on a single context entering L1; it cannot exceed the local total |
| `response_cache_reconstruct_max_bytes` | 67,108,864 bytes (64 MiB) | Integer, 8-512 MiB | Maximum logical payload allowed when reading a context from the shared backend and rebuilding it |
| `response_cache_config_generation` | 1 | Read-only | Incremented when the configuration actually changes; clients cannot write it |

The settings page sends the three budgets as one atomic update. The admin API also accepts submitting only some of the budgets; the server merges them with the current values in a database transaction, validates them, and applies them to this instance only after the commit succeeds. The fixed limits do not change with these three settings: at most 2,000 entries, an absolute TTL of 45 minutes, and at most 200 raw items per entry. Lowering a budget shrinks the local L1 immediately and may evict existing contexts.

Redis mode stores the response context in the shared backend. A backend value that is within the rebuild limit but exceeds the single-entry or total admission budget of L1 can still serve the current request, but it is not promoted to the local L1. When the Memory cache is used with a SQLite file as the main database, the response context is also written by default to the local disk persistence layer (see the next section), so it can still be recovered from disk after L1 eviction, after exceeding the L1 admission budget, or after a process restart. If the persistence layer is turned off, only the process-local L1 is kept; when a context is known to be over limit or evicted, or when a required context is missing or expired, HTTP `409 previous_response_not_found` may be returned. A corrupted Redis value, or one that exceeds the rebuild limit, can also return 409. When the shared backend is temporarily unavailable, requests that depend on that context may return HTTP `503`.

#### Disk persistence layer in Memory mode

Enabled by default when the Memory driver is used with a SQLite main database. Contexts are written to a separate file, `response-context.db`, in the same directory as the main database. They are not written to the main database and are not included in its backups. Historical entries with identical content are stored only once (deduplicated by SHA-256 with reference counting), and are kept on a sliding basis by most recent access time. The persistence layer is only a cache; if it cannot be opened, the service still starts and falls back to pure L1.

| Environment variable | Default | Description |
| --- | --- | --- |
| `RESPONSE_CONTEXT_STORE_PATH` | `response-context.db` in the main database directory | Explicit path; `off` / `none` / `false` / `0` disable it. Only the Memory driver is supported; Redis mode uses the shared backend |
| `RESPONSE_CONTEXT_STORE_TTL_HOURS` | `48` | Minimum retention time (1–720 hours); extended on every read |
| `RESPONSE_CONTEXT_STORE_MAX_MB` | `2048` | Upper limit on the total size of deduplicated entry bodies (64–65536 MB); when exceeded, the least recently accessed entries are evicted down to 90% |

When a context is unavailable, the log prints one line `[response-context] unavailable`, including the reason, the query result, and the L1 usage and eviction counts, so that restarts, evictions, and over-limit cases can be told apart.

A generation is allocated and incremented only when a budget actually changes; an update with the same value or an empty update does not increment it. The current instance applies the change immediately after the database commit. Other instances poll every 5 seconds and apply only a newer generation; a single read waits at most 3 seconds. If synchronization fails, the last valid configuration is kept and the error is shown on the operations page; it recovers automatically after a later poll succeeds.

These budgets cover the replay context for HTTP Responses/Compact and native Responses WebSocket. A healthy native WS continuation still keeps `previous_response_id` and hands it to the original upstream connection. A native WS request with an explicit `store:false` saves a full snapshot after the response is successfully filtered and committed: Memory uses the process-local L1, and Redis also saves a shared snapshot so that the downstream can recover when it reconnects to another instance. This snapshot is not affected by the on-demand write policy. Snapshots are isolated by database scope, API key, permission scope, stable session, execution channel, and `stream_id`, and follow the budgets and absolute TTL above. Reading or promoting to L1 does not extend the original expiry time. When the L1 budget is exceeded but the entry is still within the rebuild limit, it can be served from Redis. If the entry count or the rebuild limit is exceeded, the entire snapshot is rejected, and ancestors are not silently lost by truncation.

Redis native WS snapshots atomically save the full history, account ID, credential generation, compatibility domain, and expiry time as a single record. When restoring encrypted reasoning/compaction items, the original account is preferred, and then the credential and compatibility domain are checked. If the account is not compatible, the system does not drop necessary reasoning just to force a continuation. A shared commit barrier is registered before the terminal state is sent, and other instances wait for the snapshot commit before reading. If the shared snapshot is temporarily unavailable and L1 cannot restore it, a recoverable `503` is returned; a full L1 hit does not query Redis additionally. Upstream entity connections and the connection pool still belong to the original instance; Redis shares the context and coordination state, and does not migrate entity WS connections.

When the original connection is lost, or the upstream explicitly rejects the old ID, the gateway recovers at most once, provided no content or tool call has been delivered yet: it removes `previous_response_id` and sends the full snapshot. Encrypted reasoning/compaction items are replayed only when the original account, credential generation, and compatibility domain all match. When an ancestor is missing, over limit, or its compatibility cannot be confirmed, it returns `status:409` and `code:previous_response_not_found`, so the client can resend the full input; a healthy downstream WS stays available. In Memory mode, native WebSocket snapshots are kept only in the process; in Redis mode, shared snapshots can be recovered across instances or after a restart within the original absolute TTL.

Both the asynchronous and synchronous write paths to the shared backend are uniformly limited to at most 16 in-flight writes, 64 MiB of in-flight logical body, and 64 waiters. The budget is acquired before copying or encoding, and the wait is at most 5 seconds. A valid single context that already exceeds 64 MiB may take the writer exclusively, so its logical limit is the larger of the normal budget and the largest single in-flight entry; large snapshots are not silently dropped. The normal write I/O deadline is 2 seconds, and the synchronous write at shutdown is 500 milliseconds. When saturated, response wrap-up and later turns on the same WS may wait for write budget; after a write failure, L1 can still serve, and when a snapshot is required but both L1 and the shared backend are missing it, 503 is returned.

The operations API `/api/admin/ops/overview` provides `request_memory`, with the body budget, current value, high-water mark, and rejection count, and `response_cache_writer`, with the in-flight and waiting write counts, logical bytes, timeouts, and rejection counts. `response_cache.backend_write_failures` counts backend write failures. L1 `current_bytes` is the sum of the logical sizes of all snapshots, and `shared_payload_bytes` is the body size after deduplication. Neither includes JSON-encoded copies, Go allocator, or container overhead.

The "bytes" here are the sum of the retained `json.RawMessage` lengths. They do not include map, slice, LRU, Go heap, or container overhead, so they are not a hard limit on RSS or process memory. During a rolling upgrade, when an old backend lacks a setting, the new frontend shows the default display values of 64/8/64 MiB and generation `0`; when an old backend lacks the response-cache operations object, the frontend shows a compatibility waiting state instead of crashing.

### Scheduling settings

| Field | Type | Default | Range | Description |
|------|------|--------|------|------|
| `MaxConcurrency` | int | 2 | ≥1 (no upper limit) | Maximum number of concurrent requests per account |
| `GlobalRPM` | int | 0 | 0-∞ | Global request limit per minute; 0 means no limit |
| `MaxRetries` | int | 2 | 0-10 | Existing finite retry budget, covering transport errors and the existing retryable 5xx responses (including 500/502/503/504); `0` disables this budget |
| `MaxRateLimitRetries` | int | 1 | 0-10 | Existing finite, separate retry budget for 429 responses; `0` disables this budget |
| `RetryIntervalMS` | int | 0 | 0-30000 | Milliseconds to wait before a normal retry; `0` keeps retries immediate |
| `TransportRetryPolicy` | string | `rotate` | `rotate` / `sticky` | On transport-error retries, switch to another account, or retry on the same account |
| `SchedulerEngine` | string | `legacy` | `legacy` / `shadow` / `indexed` | Scheduler execution engine; can be switched live on the settings page, and synced incrementally across instances through the database outbox |
| `FastSchedulerEnabled` | bool | false | - | Legacy compatibility field; new deployments should use `SchedulerEngine` |
| `CodexForceWebsocket` | bool | false | - | Forces the Codex upstream to use a long-lived WebSocket connection (reusing the connection pool), which is closer to the official CLI experience; when off, the original HTTP requests are used |
| `CodexWSKeepaliveEnabled` | bool | false | - | Enables keepalive for idle upstream WS connections (the background only sends pings, starts no new requests, and uses no account quota) |
| `CodexWSKeepaliveIntervalSec` | int | 60 | 10-600 | WS keepalive ping interval (seconds); takes effect only when `CodexWSKeepaliveEnabled` is on |
| `CodexWSDownstreamKeepaliveSlots` | int | 8 | 0-32 | Number of idle chat upstream connections kept per account after reasoning output ends; excess connections are reclaimed by LRU |
| `CodexWSStatelessSlots` | int | 8 | 0-32 | Per-account budget for blank upstream connections and the scan window of the stateless pool |
| `CodexWSHideUpstreamErrors` | bool | true | - | When the WS upstream finally fails, hides the raw error from the client and returns a unified friendly message; the raw error is still recorded in the admin logs and usage records |
| `CodexWSSilentRetryEnabled` | bool | true | - | When rate limiting, quota exhaustion, 5xx, a read error, or a timeout happens before the first WS packet, silently switches account and rebuilds the upstream WS |
| `CodexWSSilentMaxRetries` | int | 2 | 0-10 | Upper limit of silent retries before the first WS packet; `0` disables this budget |
| `SchedulerMode` | string | `round_robin` | - | Scheduling mode: `round_robin` (round-robin, ordered by scheduling-score weight), `remaining_quota` (prefers accounts with lower usage), or `fill_first` (sequential exhaustion: concentrates use on the account with the least remaining quota, then moves to the next one after exhaustion or rate limiting). Within the same priority and health tier, the indexed engine compares live occupancy over windows of up to 8 available candidates; quota modes still compare usage first. When windows are filtered out or concurrency is full, selection continues to fill in, and absolute minimum occupancy across the whole pool is not guaranteed. |
| `AffinityMode` | string | `bounded` | - | Session affinity: `bounded` (re-picks the account when it is unhealthy or its binding has been idle for more than 10 minutes; active sessions are not rotated, so the upstream prompt cache is kept), `off` (re-selects every time), `strict` (long-term stickiness) |

Scheduling priority decides the account tier first; within the same priority, health tier, scheduling score, and current load are compared. Session affinity only reuses an already bound account. When several end users share one API key, the downstream client can send `X-Codex2API-Affinity-Key`; the value is hashed first and used only for local account binding, and is not forwarded to the upstream.

The recommended rollout order for scheduler engines is `legacy → shadow → indexed`:

- `legacy` keeps the original full-pool scan, as a no-downtime fallback path.
- `shadow` still selects accounts with legacy, samples index availability once every 64 requests, and shows match/mismatch counts on the operations page; it is for short gradual rollouts and is not recommended for carrying full traffic long term.
- `indexed` uses a tiered in-memory index, sparse API key routing sub-pools, and event-driven waiting. As the number of accounts grows, steady-state selection no longer copies or scans the full account slice.

The filters and admission callbacks for indexed selection run outside the scheduling lock. After they return, the candidate generation, account status, and concurrency are checked again; `Disabled` / `DispatchPaused` likewise block the final placement. Existing session bindings, capacity-borrowing protection, and account constraints for stateful continuations remain in effect.

When accounts are fully loaded, the waiting queue is subject to both the global limit and the per-API-key limit, and all protocols and upstreams that use the account-pool waiting path share this budget. On HTTP queue overflow, it returns `503` with `Retry-After: 1`. For SSE output that has already been committed, it sends the failure event of the corresponding protocol. Responses WebSocket returns an error frame with `status:503` and keeps the connection available; the message suggests retrying after 1 second. This local overload does not enter the upstream account-switching loop that retries continuously, and it is not misreported as account quota exhaustion. Existing waiters are not canceled by lowering the limit. The environment variables are not a cross-instance quota and do not limit requests already running upstream.

Within the queue, requests rotate by API key, and waiters for the same key are woken in the order they were enqueued among requests that can be tried. A normal single-slot release wakes only one waiter. Existing continuation bindings and excluded accounts are used to skip non-matching notifications, and the remaining model, group, and scope filters still run outside the lock; on failure, the opportunity passes to the next waiter. One round of notifications tries the current waiting set at most once, and at most 8 notification-driven selections run at the same time; dense releases are merged into later capacity checks. Fairness applies to queued requests; it does not promise to bypass the global first-come-first-served order for new requests on the fast path, and it does not guarantee the same throughput for different filter conditions. SSE/WS heartbeats are not re-queued. Each account pool with waiters uses only one per-second recovery check timer, which stops automatically when the queue is empty; the expiry-recovery notification of account cooldown itself still applies.

Codex transient account rate limiting backs off along `15s → 30s → 60s → 120s → 240s → 300s`. Concurrent 429 responses within the same frozen window advance the backoff only once. A longer real `Retry-After` can extend that window (up to 5 minutes), while ordinary repeated 429 responses do not push the deadline back. A short freeze also blocks Spark scheduling, but exhaustion of the 5h/7d quota for ordinary models still does not consume the separate Spark quota. A short freeze does not write to the database and does not trigger a WHAM probe; when it expires, the local index is restored directly. The native Redis/Memory cache keeps the rate-limit type and backoff level, and merges deadlines atomically; a late short freeze cannot override a quota or authentication cooldown. During a rolling upgrade, old instances cannot recognize the new category, so it is recommended to evaluate short-freeze behavior only after all instances have been upgraded.

The `scheduler` metrics of the operations API add `fast_scanned_accounts` (actual number of candidate checks), `fast_filter_checks`, `fast_acquire_failures`, `fast_lock_wait_ns`, and `model_cooldown_cache_reads`. These are cumulative counts for this process, so the cost of each selection should be computed from the difference over time; a fast-path hit no longer means that no scan happened. `selection_duration_buckets` is a cumulative histogram with buckets `10us/100us/1ms/10ms/100ms/1s/+Inf`. It covers the same ordinary and new-session selections as `selection_total`; direct reuse of an existing binding is not counted in this histogram. Shared cooldown and outbox across instances do not provide a global per-account concurrency limit; concurrency slots are still counted separately by each instance.

The waiting queue also exposes `max_waiters`, `max_waiters_per_key`, `waiters`, `wait_rejected` (all queue rejections), `wait_rejected_per_key` (the subset rejected by the per-key limit), `wait_granted`, `wait_duration_ns`, and the cumulative `wait_duration_buckets` histogram with buckets `10ms/100ms/1s/10s/30s/+Inf`. Wait-time statistics include successful, canceled, and timed-out waits; rejected enqueues are not counted. The incremental ratio of `wait_wakeups / wait_granted` helps spot wasted wakeups, but it should not be treated as an upstream throughput metric. Docker deployments should pass the two new environment variables to the application container. The project's standard/SQLite compose `env_file` reads `.env`, and the dedicated 2004 compose file can override them with `environment`.

On startup, `scheduler_outbox` and `maintenance_jobs` are created automatically, along with their indexes and triggers; neither PostgreSQL nor SQLite needs a manual migration. Changes to accounts, API keys, groups, proxies, and scheduling settings across multiple instances are replayed incrementally by outbox watermark; high-frequency usage counts do not produce scheduling events. Once the environment variable `CODEX_SCHEDULER_ENGINE` is set, it fixes this instance's engine and overrides the admin panel value.

`ContinuousRetryPolicy` is a separate continuous-retry policy that is off by default and is persisted as JSON:

```json
{
  "enabled": false,
  "catch_all": false,
  "categories": ["transport", "http_429", "http_5xx", "stream_error"],
  "status_codes": [],
  "error_codes": []
}
```

When enabled, continuous retry starts if any of the category, the exact HTTP status code, or the exact upstream error code matches. Broad or deterministic failures such as `http_4xx`, 403, 404, context errors, and "all `response.failed`" must be explicitly selected by the administrator; 501 is already covered by the default `http_5xx` category. The ordinary custom-selection mode does not automatically upgrade unselected `invalid_request` or structured safety-policy rejections to unlimited retries.

`catch_all` is a super mode that is off by default. When it is on, the system no longer relies on lists of known categories or error codes. Apart from an explicit upstream `cyber_policy`, any real upstream HTTP, transport, stream-read, `error`, `response.failed`, or unknown failure enters continuous retry, including permanent quota, balance, authentication, invalid-request, and other structured safety-policy errors. An explicit upstream `cyber_policy` always ends the current request, with no account switch and no replay. Text reasoning accepts only an upstream HTTP `200` and a normal protocol terminal state; any other status, failed terminal state, or EOF without a terminal state discards the whole attempt and continues. The super switch in the admin panel sets both `enabled=true` and `catch_all=true` in one save; turning off the master switch also clears `catch_all`, so it is never hidden as enabled.

Continuous retry buffers each streaming upstream attempt in full; a failed attempt is discarded entirely, and only a normal terminal state is replayed at once. While the target endpoint waits for the upstream response headers, reads the response body, or reads stream data, it keeps the downstream connection alive: Responses, Chat Completions, and Images establish an SSE 200 when the first keepalive period arrives and send a `: keepalive` comment; Responses, Realtime, and Live Sideband WebSockets use Ping control frames. After the heartbeat has committed the SSE, any later upstream error uses a protocol error event and no longer changes the HTTP 200. Non-streaming JSON (including native Responses, compact, and Alpha Search, but not Codex/GPT Images) uses the standard HTTP `102 Processing` informational response and does not commit the final status or JSON. Cloudflare still requires the final response within 125 seconds after receiving a 102, so it only extends the wait and is not indefinite keepalive. `max_duration_seconds` sets the wall-clock limit of the unlimited budget (default 600 seconds, range 1 to 900 seconds). It starts when a request first enters unlimited retry, and later attempts do not reset it. When the deadline is reached, the upstream is canceled immediately and the most recent real upstream failure is returned; `504 upstream_timeout` is used only when there is no failure yet to return. The ordinary custom-selection mode does not retry unselected structured safety-policy rejections without limit; `catch_all` can override other rejections, but cannot override an explicit upstream `cyber_policy` or the local retry deadline.

When `stream=false` or `stream` is omitted, Codex/GPT Images do not send HTTP 102 and do not commit a 200 early; they wait directly for the final JSON, which avoids some reverse proxies forwarding informational responses incorrectly. Callers still need to set wait/idle timeouts long enough for the full generation time. Their `stream=true` requests continue to use SSE heartbeats. This exception does not change the 102 behavior of other non-streaming endpoints, and it does not change retry or deadline behavior.

The HTTP/SSE keepalive above covers `/v1/responses` (including native, stream, and non-stream), `/v1/chat/completions`, `/v1/responses/compact`, `/v1/alpha/search`, `/v1/images/generations`, and `/v1/images/edits` (for Codex/GPT, only `stream=true`). Image jobs and `POST /v1/live` do not enable this keepalive.

When an API key model request-count budget is configured, keepalive does not commit SSE 200 before quota admission is complete. Ordinary Responses and Chat Completions requests stop sending heartbeats after the downstream cancels, and keep the upstream usage read-back window of at most 5 seconds. Response reads in continuous retry still end immediately when the downstream cancels.

The staging limit for a single streaming attempt is 64 MiB. The first 8 MiB is kept in memory, and beyond that, data is written to a mode-0600 temporary file that is unlinked immediately. Exceeding the staging limit or a storage failure stops the attempt immediately as a local error. There is currently no process-wide staging budget across requests, so high-concurrency environments need separate concurrency limits and must monitor memory and temporary disk. If an SSE heartbeat has already committed the response headers during the Responses HTTP wait, the final successful account's `X-Codex-Turn-State` can no longer be sent, so the implementation omits that header rather than forwarding the failed account's state. An account-bound continuation that cannot be safely expanded into a self-contained request is also not forced onto another account.

Client cancellation, downstream write failure, WebSocket disconnection, entry validation, account-pool or concurrency scheduling, local prompt or output-policy rejection, staging resource failure, and replay failure after success all end immediately and are never rotated on as upstream errors. Ordinary image requests are still capped at 5 attempts; once an error is selected by a continuous policy (including `catch_all`), it passes the ordinary cap and continues until success or client cancellation. Image creation may generate and charge repeatedly, because the upstream may not support a reliable idempotency key. Super mode may also keep consuming tokens, request counts, balance, account quota, staging memory and disk, and may occupy API key and scope concurrency slots for a long time, blocking newer requests; it must be enabled only after explicitly accepting these risks.

An explicit upstream `cyber_policy` is a non-overridable security terminal state: both ordinary custom selection and super mode stop the current request immediately, keep the local audit and signed decision, and write a conversation lock and a verified-user cooldown according to the existing policy. It is never silently swallowed as an intermediate failure, and it does not rotate to another account to replay the same request.

The continuous-retry wait takes the largest of `retry_interval_ms`, jittered exponential backoff (starting at 250 ms, capped at 30 seconds), and the effective `Retry-After` (at most 5 minutes). Even if the upstream keeps returning a very small `Retry-After`, the local backoff still grows, so that unlimited mode does not form a high-frequency request loop.

### Test settings

| Field | Type | Default | Description |
|------|------|--------|------|
| `TestModel` | string | "gpt-5.5" | Model used for test connections |
| `TestContent` | string | "hi" | User input content sent to the upstream by test connections. For multiple lines, one line is picked at random each time. Supports the `{{time}}`, `{{date}}`, `{{datetime}}`, `{{timestamp}}`, `{{rand}}`, and `{{rand:min-max}}` variables |
| `TestConcurrency` | int | 50 | Concurrency for batch tests, range 1-200 |

### Proxy settings

| Field | Type | Default | Description |
|------|------|--------|------|
| `ProxyURL` | string | "" | Global proxy URL |
| `ProxyPoolEnabled` | bool | false | Enables the proxy pool. When on, unbound accounts are sticky-assigned from enabled proxies; accounts bound to a disabled or failed managed proxy do not connect directly; scheduling is refused when the pool is empty and there is no global proxy |
| `ResinURL` | string | "" | Resin sticky proxy pool address (including the token, in the form `http://127.0.0.1:2260/<token>`). Logs and the settings API only echo the masked `scheme://host` |
| `ResinPlatformName` | string | "" | Platform identifier on the Resin side. It is enabled only when filled in together with `ResinURL`; clearing either one disables it |

#### Egress chain priority

The Codex channel has three outbound configurations that coexist. Their precedence is fixed, each layer overrides the one below rather than adding to it:

1. **Resin reverse proxy** (global): when enabled, all outbound traffic of the Codex channel that carries account identity (`/responses`, compact, WebSocket, wham usage/reset-coupon/subscription queries, client telemetry, and token refresh) is routed through Resin, and the egress IP is provided by Resin with account stickiness. In this case, the proxy selected by layer 2 below is kept only in the audit label and does not take part in dialing. The fail-closed behavior of the proxy pool (empty pool, or bound managed proxy disabled) no longer applies to Codex accounts, so accounts are not skipped because of it.
2. **Proxy chain**: account `proxy_url` > group proxy > proxy pool (sticky by account ID) > global `ProxyURL`.
3. **Direct connection**: when the proxy pool is off and all of the above are empty, connect directly to the upstream.

The admin panel marks which layer currently carries outbound traffic on the "System settings → Resin" card, at the top of the proxy pool page, and on the proxy badges in the Codex account list. The read-only `codex_egress` field of the settings API gives the same result (`mode` is `resin` or `proxy_chain`).

### Account-level settings (single account)

The following fields are stored in the `accounts` table and can be set per account in the admin panel's account details or through the API:

| Field | Type | Default | Description |
|------|------|--------|------|
| `credit_enabled` | bool | false | Marks the account as using credit-based billing |
| `credit_skip_usage_window` | bool | false | Skips the 7-day/5-hour usage window penalty (for credit accounts) |
| `score_bias_override` | int/null | null | Manually overrides the scheduling weight score; `null` follows the plan default |
| `base_concurrency_override` | int/null | null | Manually overrides the base concurrency value (`≥1`, no upper limit); when `null`, first inherit the smallest effective value of the owning group, then fall back to the global default |
| `scheduler_priority` | int/null | null | Strict scheduling priority (`-100..100`); `null` restores the default `0` |
| `skip_warm_tier` | bool | false | Skips the warm tier; it only promotes warm to healthy and does not override risky/banned |

Bulk editing in the account list supports score bias, base concurrency, scheduling priority, tags, and groups. If a numeric field is checked but its input is left empty, `null` is sent, which resets that field to its inherited or default value. Unchecked fields stay unchanged.

### Group-level base concurrency

An account group can set `base_concurrency_override` (`≥1`, no upper limit; `null` means no override). Base concurrency is resolved as: account explicit override > smallest effective value among the owning groups > global `max_concurrency`. The final dynamic concurrency is still limited by health tier, usage protection, and smart pacing limits.

### WebSocket connection pool and 1009 degradation

- In the admin panel **Settings → WebSocket (Codex upstream) → Chat continuation connection retention** (`codex_ws_downstream_keepalive_slots`) limits how many idle chat upstream connections each account keeps while Codex output is finished and it is waiting for the user to continue. The default is 8, the range is 0–32, it is shared by all keys, and it is hot-updated after saving. When set to 0, the upstream connection is closed as soon as chat output finishes, and later input is recovered through a context that is still valid. Connections that are being dialed or are reasoning do not use the idle retention budget. After chat output finishes for ordinary sessions, stateless clients, overflow, one-off connections, and background execution lanes, they are retained by the same rule.
- The existing `codex_ws_stateless_slots` (number of blank warm-up connections retained) independently limits, for each account, the blank connections that do not carry user chat context, and controls the scan window of the stateless connection pool. The default is 8 and the range is 0–32. An explicit warm-up request with no chat input, instructions, tool definitions, templates, or continuation ID uses the blank budget. Before a chat starts on a blank connection for the first time, the connection leaves the blank budget and enters the reasoning state, and only after output ends does it enter the chat idle retention pool; the two retention pools do not occupy each other's slots. When set to 0, the general pool is not scanned and no blank connections are retained, but warm-up requests can still create a connection temporarily and close it after wrap-up. The sum of the two values is the normal idle retention amount, not a hard limit on all entity connections; reasoning connections are additionally constrained by the original account/key request concurrency.
- The connection budget is independent of `DynamicConcurrencyLimit`. The request concurrency of API keys and accounts keeps the original configuration, queue, and counting method. For example, if 4 keys each have a concurrency limit of 3 and the account has a concurrency limit of 3, at most 3 requests are still handled at the same time. To keep upstream connections for 12 independent chat sessions, set "Chat continuation connection retention" to 12 or 16; existing configuration is not changed automatically.
- Blank dials still reserve atomically against the original budget. Chat dials, in-flight connections, and wrap-up after removal from the pool only register the entity socket lifecycle, and the registration is released idempotently once the socket is actually closed. Chat reasoning can start normally even when the idle retention pool is full, and it does not return 503 because of this setting; the original key/account request concurrency and busy-wait rules still apply.
- After chat output finishes, the last business-use time is updated. When idle chat connections exceed the limit, the least recently used connections are closed by LRU, including connections that still have a valid continuation binding. Reclamation rechecks connection identity and pending requests under the account lock, and skips in-flight requests, dials, and blank connections. A late callback from an old connection does not affect a new one. When a reclaimed session receives input again, the upstream connection can be rebuilt from a still-valid and compatible full context snapshot. When the blank budget is insufficient, `503 upstream_ws_connection_capacity_exceeded` may still be returned.
- After lowering the chat retention count, acquiring a connection, finishing output, or the background check every 30 seconds evicts the oldest idle connections under the new limit; reasoning is not affected. In normal mode, an upstream WS that was used for user chat is also closed after 30 minutes idle since its last business use; Ping/Pong, downstream connections staying open, and background metadata do not extend it. Blank (never-used) connections are not subject to the chat idle limit; weak-network mode and rotation on reaching max age keep the original rules.
- Idle reclaim actually closes the old upstream socket and clears the corresponding session and continuation binding. Later requests re-handshake to create a new upstream WS and restore the full context when needed; a late cleanup of an old connection does not remove a new connection under the same pool key.
- `CODEX_WS_STATELESS_ONESHOT=1` disables generic slot reuse; after a response finishes normally, connections with a valid continuation binding are kept under the chat idle limit, and one-shot connections without a binding are destroyed immediately. Responses that fail or are not fully consumed still destroy the corresponding bad connection.
- Connection close logs distinguish local reclaim, idle timeout, rotation on reaching max age, upstream close frames, and network errors, and record the close code, continuation binding status, and the number of remaining connections. Redis continues to hold full continuation snapshots and recovery metadata, and the physical connection budget is coordinated within a single instance. Local and Redis contexts are kept for 45 minutes by default, which covers the recovery window after chat connections are reclaimed after 30 minutes idle; cache capacity eviction, account incompatibility, or expiry can still make recovery fail.
- Requests carrying `previous_response_id` only retrieve the original connection and re-verify the account, model, egress, and client identity; when busy, they wait cancellably and respect the existing busy wait limit. When the wait is exhausted they return `status:503`; API Key concurrency rejections return `status:429`. In both cases the downstream WS is kept for the next turn.
- If the upstream returns close 1009 before any content has been output downstream, or a local read triggers an equivalent read-limit error, the gateway keeps the same account lease and resolved proxy and degrades to HTTP at most once.
- 1009 is a transport limit: it does not lower account health and does not trigger an auth probe; once content has been output downstream, no HTTP degradation is started, to avoid duplicate requests and duplicate billing.

### Connection pool configuration

| Field | Type | Default | Range | Description |
|------|------|--------|------|------|
| `PgMaxConns` | int | 50 | 5-500 | Maximum PostgreSQL connections |
| `RedisPoolSize` | int | 30 | 5-500 | Redis connection pool size |

### Automatic cleanup configuration

| Field | Type | Default | Description |
|------|------|--------|------|
| `AutoCleanUnauthorized` | bool | false | Automatically clean up 401 accounts |
| `AutoCleanRateLimited` | bool | false | Automatically clean up 429 accounts |
| `AutoCleanFullUsage` | bool | false | Automatically clean up accounts at full usage |
| `AutoCleanError` | bool | false | Automatically clean up accounts in error status |

### Automatic activation of the 5h window

| Field | Type | Default | Description |
|------|------|--------|------|
| `AutoActivate5hWindowEnabled` | bool | false | After the 5h window resets, automatically sends one minimal real `/responses` request to start the next 5h timer (issue #581). Disabled by default; enabling it consumes a small amount of real quota. Only applies to accounts for which the upstream explicitly returns a 5h window and reset time; at most once per window. Skipped when the account is unavailable, automatically paused, or has no 5h window. It does not call `rate-limit-reset-credits/consume`, and it is also not the zero-cost `wham/usage` probe that runs when due. |

### Security settings

| Field | Type | Default | Description |
|------|------|--------|------|
| `AdminSecret` | string | "" | Admin panel password (stored in the database) |
| `AllowRemoteMigration` | bool | false | Allow remote migration (requires AdminSecret to be set) |

---

## API Key model weekly request budget

In the admin panel under **API Key → Advanced limits**, you can assign a weekly call count for specified models to the same key. For example, `gpt-6*` totals 50 calls per week, and after they are used up, other unmatched models can still be called; without `model_request_limits` configured, the original behavior is kept. Counts are assigned by the administrator, and the system does not assume a weekly quota for any upstream plan.

Configure `limits.model_request_limits` through the API Key create or update endpoint:

```json
{
  "limits": {
    "model_request_limits": [
      {
        "model": "gpt-6*",
        "window": "week",
        "max_requests": 50,
        "timezone": "Asia/Shanghai",
        "reset_weekday": 1,
        "reset_time": "00:00"
      }
    ]
  }
}
```

| Field | Description |
| --- | --- |
| `id` | Stable rule ID generated by the server; omit when adding a rule, keep it when changing the call limit or reordering |
| `model` | Final mapped model name; supports exact names and `*` wildcards; all models matched by one rule share its count |
| `window` | Currently only `week` is supported; defaults to `week` when omitted |
| `max_requests` | Maximum number of requests per fixed weekly window; must be a positive integer |
| `timezone` | IANA time zone, default `Asia/Shanghai`, independent of the server's `TZ` |
| `reset_weekday` | Reset weekday, `1` is Monday and `7` is Sunday; defaults to Monday when omitted or `0` |
| `reset_time` | Reset time within the time zone, `HH:MM`, default `00:00` |

Weekly windows are calculated on the calendar of the specified time zone, including daylight saving time changes. A window is a fixed period, for example from Monday 00:00 to next Monday 00:00 in Shanghai time; it is independent of the existing sliding `7d` limits on amount and tokens, and all limits apply at the same time. When several model rules match, each rule must have balance and each is counted once, so you can configure both a total budget for the `gpt-6*` series and a single-model budget for `gpt-6-astra` at the same time.

The counting unit is one external HTTP request, or one `response.create` on a WebSocket connection. After global and account model mapping completes, the gateway atomically deducts quota when it attempts to send upstream; internal account switching, transport fallback, and retries reuse the same request identity, and the same rule is counted at most once. A request that was already attempted counts even if it fails, times out, or is canceled; entry validation, no available account, and rejection before sending are not counted. A client re-sending a request counts as a new request. If an internal retry maps to another model, the newly matched rule also needs quota. Retrying across weeks does not deduct the same rule again, and the deducted count still belongs to the window where it was first deducted.

A rule's `id` together with its model matching condition, time zone, and reset schedule identifies one budget. Existing rules may only have `max_requests` or their list order changed, and keep their used count; when the model or reset schedule needs to change, delete the old rule and add a new one, and the new rule starts counting from zero. Raising the limit immediately provides more balance; lowering the limit below the used count stops the rule from allowing requests. An update request without `limits` keeps all limits; when `limits` is passed, it replaces the whole limits object, and `model_request_limits: []` removes the model weekly budgets.

Counts and request idempotency records are stored in separate tables in PostgreSQL / SQLite, and are created automatically at startup. Multiple PostgreSQL instances share the same authoritative count; SQLite lightweight mode uses the same semantics. Restarting the service, clearing the Redis/in-memory cache, deleting usage logs, or resetting amount quota does not clear the model weekly budget; it gains new quota naturally at the next window. When the quota-deduction database is unavailable, related requests with a model weekly budget return service unavailable, to avoid concurrent over-spending.

The admin Key edit window and the public `/key-usage` page show the used, remaining, and maximum counts for this week, along with the next reset time. The display is not affected by the `today` / `7d` / `30d` / `all` filters of the usage chart, and always shows the rule's current weekly window. This budget is used to allocate gateway call counts; it does not guarantee the same metering basis as an upstream plan. For over-limit responses and query endpoints, see the [API docs](API.md#api-key-model-weekly-request-budget).

## Configuration file examples

### Standard production environment (.env)

```bash
# ============================================================
# Codex2API production environment configuration
# ============================================================

# Service configuration
CODEX_PORT=8080
ADMIN_SECRET=your-secure-admin-password-here
TZ=Asia/Shanghai

# Database configuration (PostgreSQL)
DATABASE_DRIVER=postgres
DATABASE_HOST=postgres
DATABASE_PORT=5432
DATABASE_USER=codex2api
DATABASE_PASSWORD=your-strong-db-password
DATABASE_NAME=codex2api
DATABASE_SSLMODE=disable
IMAGE_ASSET_DIR=/data/images
LOG_DIR=logs
LOG_DISABLED=false

# Cache configuration (Redis)
CACHE_DRIVER=redis
REDIS_ADDR=redis:6379
REDIS_USERNAME=
REDIS_PASSWORD=your-redis-password
REDIS_DB=0
REDIS_TLS=false
REDIS_INSECURE_SKIP_VERIFY=false
```

### SQLite lightweight environment (.env)

```bash
# ============================================================
# Codex2API SQLite lightweight edition configuration
# ============================================================

# Service configuration
CODEX_PORT=8080
ADMIN_SECRET=your-admin-password
TZ=Asia/Shanghai

# Database configuration (SQLite)
DATABASE_DRIVER=sqlite
DATABASE_PATH=/data/codex2api.db
IMAGE_ASSET_DIR=/data/images
LOG_DIR=logs
LOG_DISABLED=false

# Cache configuration (in-memory)
CACHE_DRIVER=memory
```

### Development environment (.env)

```bash
# ============================================================
# Codex2API development environment configuration
# ============================================================

CODEX_PORT=8080
# ADMIN_SECRET=dev  # Can be left unset in development

# Local PostgreSQL
DATABASE_DRIVER=postgres
DATABASE_HOST=localhost
DATABASE_PORT=5432
DATABASE_USER=codex2api
DATABASE_PASSWORD=codex2api
DATABASE_NAME=codex2api

# Local Redis
CACHE_DRIVER=redis
REDIS_ADDR=localhost:6379
REDIS_USERNAME=
REDIS_PASSWORD=
REDIS_DB=0
REDIS_TLS=false

TZ=Asia/Shanghai
```

---

## Configuration precedence

When the same setting comes from multiple sources, the following precedence applies:

```
1. Environment variables (highest precedence)
   ↓
2. Variables in the .env file
   ↓
3. Database SystemSettings (business configuration)
   ↓
4. Program default values (lowest precedence)
```

### Special rules

**Admin Secret precedence:**

```
1. Environment variable ADMIN_SECRET
   ↓
2. Database SystemSettings.AdminSecret
   ↓
3. Empty value (no authentication)
```

**Database connection pool:**

- `PgMaxConns` takes effect immediately after it is changed, no restart needed
- `RedisPoolSize` takes effect only after a restart

**Scheduling parameters:**

- `MaxConcurrency`, `GlobalRPM`, and similar settings take effect immediately after being changed
- Changes made in the admin panel are automatically persisted to the database

---

## Configuration validation

### Validation at startup

The program automatically validates the configuration at startup:

```
✓ Database connection succeeded: PostgreSQL
✓ Cache connection succeeded: Redis
✓ Account pool initialized: 10/10 available
✓ System settings loaded
✓ HTTP service started: http://0.0.0.0:8080
```

### Configuration check API

```bash
# Health check
curl http://localhost:8080/health

# System overview (requires Admin Secret)
curl -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/ops/overview
```

---

## FAQ

### Q: Do I need to restart after changing environment variables?

**A:** Yes. Environment variables are loaded when the program starts, so the container must be restarted after a change for it to take effect.

### Q: How do I change configuration without restarting?

**A:** Business settings changed through the admin panel at `/admin/settings` (such as MaxConcurrency and GlobalRPM) take effect immediately.

### Q: Can I switch between SQLite and PostgreSQL?

**A:** Yes, but you need to:
1. Stop the service
2. Change DATABASE_DRIVER and the related configuration
3. Start the service (the new database will be initialized again)
4. Re-import account data

### Q: How do I view the configuration currently in effect?

**A:** The `/admin/settings` page in the admin panel shows system settings and their sources (env/database). The effective/applied generation of this instance's Responses context cache, the last sync time, and sync errors can be viewed at `/admin/ops`.

### Q: What if a configuration error prevents startup?

**A:** Check the log output. Common errors:
- `DATABASE_HOST is empty` - database host is not configured
- `REDIS_ADDR is empty` - Redis address is not configured in Redis mode
- `DATABASE_PATH is empty` - data path is not configured in SQLite mode

### Codex authorization keepalive in lazy mode

The admin setting `codex_oauth_keepalive_enabled` (default `false`) allows lazy mode to run Codex token renewal on its own. It uses the existing `background_refresh_interval_minutes` background check interval and the threshold of 5 minutes before AT expiry, does not change quota cooldown, and does not enable the generation probe. Normal mode already runs Codex renewal and does not depend on this switch.

Codex refresh adds the `codex_oauth_refresh_attempts` protection table, which is created automatically at startup and is compatible with PostgreSQL and SQLite. The table only stores the SHA-256 fingerprint of the old RT, the refresh operation ID, and the start time, and does not store plaintext tokens. A record is deleted after all related credentials are saved successfully; records with uncertain results are kept, to prevent the old RT from being consumed again across instances or after a restart. Existing credentials do not need to be re-imported; authorization that has already expired must be restored by signing in again.

## Adaptive intelligence-downgrade detection

Intelligence-downgrade detection supports exponential backoff with a configurable cap, recovery acceleration, and recurrence memory. It is disabled by default; for the configuration and state transition rules, see [Adaptive intelligence-downgrade detection](INTELLIGENCE_BACKOFF.md).
