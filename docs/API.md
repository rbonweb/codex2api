# Codex2API API Documentation

This document describes all API endpoints of Codex2API, the request and response formats, and the error code reference.

## Contents

- [Overview](#overview)
- [Authentication](#authentication)
- [Public API](#public-api)
  - [Chat Completions](#1-chat-completions)
  - [Responses](#2-responses)
  - [Images](#3-images)
  - [List Models](#4-list-models)
  - [Health Check](#5-health-check)
- [Admin API](#admin-api)
  - [Statistics](#statistics)
  - [Account Management](#account-management) — add RT / AT accounts, batch import, export, migration
  - [Intelligence Check](#intelligence-check) — fixed-account checks, automatic actions, and per-run history
  - [Usage Statistics](#usage-statistics)
  - [API Key Management](#api-key-management)
  - [System Settings](#system-settings)
  - [Proxy Pool Management](#proxy-pool-management)
  - [Operations Monitoring](#operations-monitoring)
  - [Model Management](#model-management)
  - [Image Studio](#image-generation-workspace) — text-to-image and image-to-image jobs, and gallery management
  - [OAuth Authorization](#oauth-authorization) — get tokens through the PKCE flow
- [Supported Models](#supported-models)
- [Error Codes](#error-codes)
- [Rate Limits](#rate-limiting)

---

## Overview

Codex2API provides an OpenAI-compatible API and also includes the full admin panel API.

**Service Tier semantics:** On the request side, `fast` / `priority` are always forwarded upstream as `priority`; other values (`auto`/`default`/`flex`/`scale`, etc.) are not forwarded. Usage logs distinguish three fields: `requested_service_tier` (the client's requested intent), `actual_service_tier` (the tier returned by the upstream, taken as-is from `response.completed.response.service_tier`), and `billing_service_tier` (the tier used for billing, decided by the tier billing policy `BillingTierPolicy`). By default, `actual` caps the tier at the requested tier: the upstream can only lower billing by reporting a cheaper tier; it cannot raise a call that did not request Fast to Fast, and an unknown tier cannot change billing. `requested` always bills by the requested intent. Note: on the ChatGPT OAuth / Codex backend path, Fast is handled by server-side routing upstream, so `service_tier` is not an end-to-end verifiable field: an upstream reply of `default` does not mean Fast was not applied (official note in openai/codex#14204; interleaved A/B measurements in #494 still showed about 1.5× generation throughput when `default` was returned). Therefore, the "upstream-returned tier" only reflects the value the upstream declares, and it cannot be used alone to decide whether acceleration took effect.

**Base URL:** `http://localhost:8080` (default port)

**Request format:**

- Request header: `Content-Type: application/json`
- Authentication header: `Authorization: Bearer <api_key>`

---

## Authentication

### API Key Authentication

The public API (`/v1/*`) requires an API key for authentication.

**Request headers:**

```http
Authorization: Bearer sk-xxxxxxxxxxxxxxxxxxxxxxxx
```

When multiple end users share the same API key, you can optionally pass a stable local affinity identifier:

```http
X-Codex2API-Affinity-Key: tenant-user-or-conversation-id
```

This header takes priority over other session affinity signals. Codex2API first derives a SHA-256 value from the raw value and keeps only the local routing identifier; the raw value is neither stored nor forwarded to the upstream.

**How to configure:**

1. Configure it on the `/admin/settings` page of the admin panel
2. Regular public endpoints allow anonymous access only when no API key is configured and `CODEX_ALLOW_ANONYMOUS=true` is explicitly enabled; anonymous access is denied by default. Asynchronous image jobs require an API key created in the admin panel.

### Admin Secret Authentication

The admin API (`/api/admin/*`) requires the admin secret for authentication.

**Request headers:**

```http
X-Admin-Key: your-admin-secret
```

or

```http
Authorization: Bearer your-admin-secret
```

**How to configure:**

- Environment variable: `ADMIN_SECRET`
- Database: set it in the admin panel

---

## Public API

### 1. Chat Completions

**Endpoint:** `POST /v1/chat/completions`

**Description:** OpenAI-style Chat Completions endpoint, supporting streaming and non-streaming responses.

**Request example:**

```json
{
  "model": "gpt-5.5",
  "messages": [
    { "role": "system", "content": "You are a helpful assistant." },
    { "role": "user", "content": "Hello!" }
  ],
  "stream": false,
  "reasoning_effort": "medium",
  "service_tier": "fast"
}
```

**Parameters:**

| Parameter        | Type    | Required | Description                                 |
| ---------------- | ------- | ---- | ------------------------------------------- |
| model            | string  | Yes      | Model name, see [Supported Models](#supported-models) |
| messages         | array   | Yes      | Message list                                |
| stream           | boolean | No       | Whether to enable streaming responses. Default false |
| reasoning_effort | string  | No       | Codex supports none/minimal/low/medium/high/xhigh/ultra; max is kept or normalized to xhigh depending on the final model's capability |
| service_tier     | string  | No       | For Codex, fast maps to priority and ultrafast is kept; auto/default do not specify an upstream tier |
| max_tokens       | integer | No       | Maximum number of output tokens (not supported by Codex; filtered out) |
| temperature      | float   | No       | Temperature (not supported by Codex; filtered out) |

**Non-streaming response example:**

```json
{
  "id": "chatcmpl-xxxxxxxx",
  "object": "chat.completion",
  "created": 1712345678,
  "model": "gpt-5.5",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Hello! How can I help you today?"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 25,
    "completion_tokens": 15,
    "total_tokens": 40
  }
}
```

**Streaming response example:**

```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.5","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.5","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.5","choices":[{"index":0,"delta":{"content":"!"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

**Custom tools:** Chat requests support `tools[].type="custom"` and nested `custom` declarations; history and responses use `tool_calls[].custom.name/input`. `custom.input` is always raw text, even when its content happens to be JSON; ordinary functions still use `function.arguments`. Streaming custom input is carried in `delta.tool_calls[].custom.input`, and the call ID stays paired with `tool_call_id` in the tool result. Non-streaming responses also recover calls that are missing from the final response, taken from `output_item.done`.

### 2. Responses

**Endpoint:** `POST /v1/responses`

**Description:** Codex Responses-compatible endpoint. The gateway applies the input normalization that the Codex upstream requires; it does not pass through all official Responses fields unconditionally.

For the full phased boundaries and the order of later implementation, see [Responses compatibility notes](RESPONSES_COMPATIBILITY.md).

The default mode keeps the existing compatibility behavior. If a client needs to check for known lossy conversions, it can send `X-Codex2API-Strict-Compatibility: true`, or set `CODEX_STRICT_RESPONSES_COMPAT=true`. The gateway checks fields and schema against the native Codex account actually selected. When something cannot be preserved, an HTTP request that has not been committed yet returns `400 unsupported_feature` with the list of fields, a committed SSE stream returns `response.failed`, and a WebSocket returns a `status:400` error frame. No-op values such as `background:false/null` pass; `background:true`, a non-empty `conversation`, and `store:true` are rejected; reasoning, format aliases, and service tiers are validated against the actual conversion semantics. Requests whose tool or output schema constraints would be changed by sanitization are rejected too. For the full scope and limits, see [Responses compatibility notes](RESPONSES_COMPATIBILITY.md).

**Request example:**

```json
{
  "model": "gpt-5.5",
  "input": [
    { "role": "system", "content": "You are a helpful assistant." },
    { "role": "user", "content": "Hello!" }
  ],
  "stream": false,
  "reasoning": {
    "effort": "medium"
  },
  "service_tier": "fast",
  "include": ["reasoning.encrypted_content"]
}
```

**Parameters:**

| Parameter            | Type         | Required | Description |
| -------------------- | ------------ | ---- | -------------------------------------------------------------------------------------------------- |
| model                | string       | Yes      | Model name |
| input                | array/string | Yes      | Input content (array or string) |
| stream               | boolean      | No       | Whether to enable streaming responses. Default false. SSE (streaming) is returned only when `stream=true` is passed explicitly; otherwise a regular JSON response is returned. |
| reasoning.effort     | string       | No       | Codex supports none/minimal/low/medium/high/xhigh/ultra; max is kept or normalized to xhigh depending on the final model's capability |
| service_tier         | string       | No       | fast maps to priority and ultrafast is kept; auto/default do not specify an upstream tier |
| include              | array        | No       | Extra fields to include |
| previous_response_id | string       | No       | ID of the previous response, used to keep context continuous |

The context for `previous_response_id` is looked up first in the bounded L1 of the current process. The default absolute TTL for local and Redis-shared context is 45 minutes, which covers the recovery window after a chat upstream connection is reclaimed following 30 minutes of idleness, and it is still subject to cache capacity and account compatibility limits. Authenticated requests are isolated by API key ID; requests that pass after `CODEX_ALLOW_ANONYMOUS=true` is explicitly enabled, with no API key configured, share the `anon` namespace. In Redis mode, an L1 miss can be rebuilt from the shared backend; when the backend value does not exceed the rebuild limit but does exceed the L1 admission budget, the request can still be served, but the value is not promoted to L1. In Memory mode with the disk persistence layer turned off, there is no response-context fallback, and when the dependent context is judged over-limit, evicted, or missing, the response may be HTTP `409 previous_response_not_found`. When the shared backend is temporarily unavailable and the request depends on that context, HTTP `503 service_unavailable` may be returned. Every non-empty parent reference in native Codex HTTP requires proof from the local or shared snapshot that currently owns it; ordinary text parent turns add no new HTTP session storage, so the client should resend the full history and omit the parent ID. Overlapping tool history is deduplicated only when a complete parent-prefix replay can be proven; otherwise it returns 409. On the client's native Responses WebSocket entry, when the upstream connection is healthy, `previous_response_id` is kept and only the current turn's delta is sent; at the start of a turn, still-valid L1 ancestor references are kept; snapshot merging and serialization run only after the response is successfully committed and written to the local cache, while the shared backend (Redis) write completes in the background and does not sit on the first-token path. The cache collects `response.output_item.done`, so messages, `phase`, tool calls and results, and Lite `additional_tools` declarations are kept even when the final `response.output` is empty. `response.completed` and `response.incomplete` can both be written after a successful commit, still subject to the write policy, TTL, and capacity limits; in on-demand write mode, a WebSocket session whose `store` is not explicitly `false` is eligible for writing from the root turn; a native WS session with explicit `store:false` (such as Codex CLI) uses an isolated full snapshot and shared-backend recovery, which does not mean the gateway caches no history at all; it is still subject to TTL, capacity, and integrity limits. When the continuation chain is invalidated, the account is switched, or the connection falls back to HTTP, context is sent again only if the required context can be recovered; missing, over-limit, or non-portable encrypted state returns a `previous_response_not_found` error frame, a shared-backend failure returns `service_unavailable`, and such recovery failures end the current turn with an error frame, with named streams carrying `stream_id`; the connection can still receive new full-context requests. Protocol, authentication, or connection-resource errors may still close the connection. Clients should resend the full context and start a new response chain. When an ancestor has already expired or been evicted before the turn starts, the gateway does not disguise a delta as a full snapshot, and later healthy turns on that chain cannot fill in the history by themselves. The L1 cache TTL is 45 minutes; in Memory mode, a restart loses native WebSocket snapshots. Setting the environment variable `CODEX_WS_CONTINUATION_FAIL_OPEN=true` falls back to the old behavior: when context cannot be recovered, `previous_response_id` is stripped and the request is forwarded as-is, so the upstream sees no history; a response produced by this lossy degradation is not written back to the replay cache, and incomplete snapshots are not trusted after the escape valve is closed.

For native WebSocket structured output, `gpt-6-astra` and `gpt-5.6-luna` keep JSON Schema `minLength` / `maxLength` when Responses Lite is explicitly enabled. The Lite signal can come from `client_metadata.ws_request_header_x_openai_internal_codex_responses_lite=true`, the `X-OpenAI-Internal-Codex-Responses-Lite: true` request header, or a metadata marker injected by Payload Rules. This exemption applies only to structured output; existing tool-argument sanitization keeps its conservative rules. Length constraints are kept temporarily during the entry preparation stage and are handled uniformly just before final egress, based on the final model, the Lite signal after rule rewriting, the account's Lite capability, and the actual transport. Other models, requests where Lite is not finally enabled, HTTP, and Compact requests keep the original sanitization strategy, and HTTP-downgraded requests do not carry constraints that do not apply.

The native WebSocket entry is `GET /v1/responses`. The client sends `response.create` each turn. During generation it may send `response.interrupt` with `response_id` and `mode: "discard_partial_items"`; the gateway routes it to the current WebSocket upstream lease and keeps the original order of acknowledgments and terminal events; see [Responses compatibility: WebSocket streams and continuation](RESPONSES_COMPATIBILITY.md#websocket-streams-and-continuation). The current gateway explicitly rejects `response.append` and the official GPT-6 `response.steer`, returning a `status:400` `unsupported_feature` WebSocket error frame, and never disguises them as a completed steer. To change direction midway, wait for the current turn to finish, then start a new `response.create` with `previous_response_id`. After validation and API key limits pass, a newer request on the same API key, with the same channel/group routing scope and the same session, preempts an older request that is still running; the old upstream is canceled first to release the account and concurrency slots. Within the same `stream_id` (including the default stream), requests are FIFO; different independent streams can run in parallel; each connection allows at most 16 active turns and 32 named streams. Each API key keeps at most 64 concurrent downstream WebSocket connections, with a global limit of 4096; when anonymous access is explicitly enabled, anonymous connections are additionally limited to 16 per IP, and exceeding that returns `503` with `Retry-After: 5`. Named-stream events and request-level errors carry back `stream_id`; a named-stream request error or preemption does not close other healthy streams, while the default stream keeps the original protocol-error close behavior. The upstream connection pool is isolated per stream; the same client connection can fork from a `store:false` response already committed on another stream, but it must still hit the full cache and satisfy the authorization scope, and when it points to the same parent response it may still wait for the original upstream lease. Different API keys or different routing scopes do not cancel each other. Session affinity is enabled only when `prompt_cache_key`, an explicit session header, `previous_response_id`, turn state, a dedicated affinity key, or content that can be stably derived into a session exists; a bare API-key fallback identity does not merge unrelated requests. Redis mode supports cross-instance preemption; Memory mode applies only within the current process.

**Response example:**

```json
{
  "id": "resp_xxxxxxxx",
  "object": "response",
  "created": 1712345678,
  "model": "gpt-5.5",
  "output": [
    {
      "type": "message",
      "role": "assistant",
      "content": [
        {
          "type": "output_text",
          "text": "Hello! How can I help you today?"
        }
      ]
    }
  ],
  "usage": {
    "input_tokens": 25,
    "output_tokens": 15,
    "total_tokens": 40
  }
}
```

### 3. Images

#### Public Image Studio Quota Lookup

`GET /api/image-studio/quota` queries the cumulative USD quota of the current key, using `Authorization: Bearer YOUR_API_KEY`. It is controlled only by the image studio portal switch; it does not depend on the public usage page switch, and it does not accept another key specified through a query parameter.

```json
{
  "quota_limit": 25,
  "quota_used": 6.4,
  "quota_remaining": 18.6,
  "expires_at": null,
  "status": "active",
  "refresh_after_seconds": 6,
  "image_pricing": {
    "gpt-image-2": { "user_billing_mode": "per_image", "image_unit_price": 0.05 }
  }
}
```

`quota_remaining` is `null` when there is no cumulative quota cap; for a limited quota, the remaining value is at least `0`. `status` is `active`, `quota_exhausted`, or `expired`; an invalid or disabled key returns `401`, and when the image studio portal is off it returns `404`. The response sets `Cache-Control: no-store` and does not return the raw key.

The public studio shows the remaining quota next to the key; clicking it shows the amount used, the total, and the validity period. While the page is visible it refreshes every 30 seconds, and it also refreshes when the user returns to the page or a task completes. Consumption is settled asynchronously: after a task completes, the quota is queried again after `refresh_after_seconds`, which is the current usage ingestion interval plus 1 second. Model-call counts and other rate-limit rules still apply separately.

After the quota is exhausted, the current key can still query its own quota and read existing public studio jobs and images; generate, edit, and delete operations are still rejected. An expired key can only query quota and cannot read works or generate; a disabled key cannot use these endpoints. The existing quota checks on `/v1/*` are unchanged.

#### Generate Images

**Endpoint:** `POST /v1/images/generations`

**Description:** OpenAI Images-compatible entry point. External requests use `gpt-image-2`, `gpt-image-2.5-flare`, or `gpt-image-2.5-sunburst` (date snapshots and the `-2k` / `-4k` tier suffixes are supported). Internally, following the `CLIProxyAPI/` and `sub2api/` paths, they are converted to Codex `/responses`: the main model defaults to `gpt-5.6-luna` (it prefers the text model set in "System Settings → Codex → Image Generation Settings"; if none is set, it falls back to the `CODEX_IMAGES_MAIN_MODEL` environment variable; when the upstream rejects it, it retries by switching to the next driver model in this order: `gpt-5.5` → `gpt-5.6-terra` → `gpt-5.6-sol` → `gpt-6-astra`), and the image model is written to `tools[0].model`.

For Codex/GPT image generation and editing, when `stream=false` or `stream` is omitted, only the final JSON is returned; no HTTP `102 Processing` is sent and HTTP 200 is not committed early. `stream=true` still uses the existing SSE heartbeat. `DOWNSTREAM_HTTP_KEEPALIVE_INTERVAL` no longer keeps alive the Codex/GPT non-streaming branch of these two entry points. Clients and reverse proxies still need wait/idle timeouts long enough for the full generation time; for asynchronous calls, use the image job endpoints below.

GPT Image 2.5 supports `auto`, `low`, `medium`, `high`, `xhigh`, and `max`. The quality parameter is passed unchanged to the image tool; when the studio switches back to an older model, `xhigh` / `max` are adjusted to `high`. Omitting the model still defaults to `gpt-image-2`. `-2k` / `-4k` are this project's size and upscale aliases; the suffix is stripped before the request goes upstream.

When using `/v1/responses` directly, the text controller goes in the top-level `model` and the image model goes in `tools[].model`; an explicit text controller takes precedence over the admin image-generation settings and `CODEX_IMAGES_MAIN_MODEL`. If the top-level `model` is set directly to an image model, the text controller configured in the admin panel is used. When the tool model is omitted, it is still filled in as `gpt-image-2`.

For the Images entry, 2.5 token billing separates text input and image input, each with its own cache: the built-in rates are $5, $8, $1.25, and $2 per million tokens, respectively, and image output is $30 per million tokens ([official pricing](https://developers.openai.com/api/docs/models/gpt-image-2.5-sunburst), checked 2026-09-09). Flare and Sunburst each have independent pricing keys; date snapshots and size aliases use the rates of the corresponding base model; custom overrides take precedence. The usage log adds `image_input_tokens`, `image_output_tokens`, and `cached_image_input_tokens`, which are subsets of total input, output, and cached tokens and are not billed twice. Historical logs cannot be backfilled with image token details that were not recorded.

**Request example:**

```json
{
  "model": "gpt-image-2",
  "prompt": "Draw a small orange cat",
  "size": "1024x1024",
  "quality": "high",
  "response_format": "b64_json"
}
```

**Exact size:** The upstream Codex treats `size` as a hint; the image shape is decided by the driver model from the prompt, and it may return an aspect ratio different from the request (for example, a request for `1024x1024` may return `1536x1024`). When `size` is an explicit `WIDTHxHEIGHT`, `/v1/images/generations` and `/v1/images/edits` (JSON and multipart) add the target pixels, orientation, and aspect-ratio instructions to the upstream request, and finalize each result image to the exact requested size.

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `strict_size` | bool | `true` | When set to `false`, upstream pixels are returned as-is without finalization |
| `upscale_fit` | string | `pad` | `pad` keeps the whole image and centers it with transparent padding, without cropping; `cover` fills the canvas and crops the overflow |

When `size` is omitted or `size: "auto"`, behavior is unchanged. The upscale for the `-2k` / `-4k` aliases still runs, and `upscale_fit` applies as well.

#### Edit Images

**Endpoint:** `POST /v1/images/edits`

**Description:** Supports JSON `images[].image_url` and multipart `image` / `image[]` uploads. `mask.image_url` or multipart `mask` can be used for mask-based editing (masks are supported only by the gpt-image family).

**JSON request example:**

```json
{
  "model": "gpt-image-2",
  "prompt": "Replace the background with aurora lights",
  "images": [{ "image_url": "https://example.com/source.png" }],
  "output_format": "png"
}
```

**Response example:**

```json
{
  "created": 1710000000,
  "model": "gpt-image-2",
  "data": [
    {
      "b64_json": "..."
    }
  ],
  "usage": {
    "images": 1
  }
}
```

#### Asynchronous Image Jobs

- `POST /v1/images/jobs`: authenticates with an API key created in the admin panel; returns HTTP 202 and `job`.
- `GET /v1/images/jobs/:id`: query with the same API key that created the job; other keys get 404.
- `POST /v1/images/jobs/results`: batch-query the compact results of up to 500 jobs; compatible with `/v1/images/jobs/result`.
- `GET /v1/images/jobs/:id/output`: streams the generated result as Base64; only images that explicitly chose `delete_after_read` are cleaned up after full delivery.
- `POST /v1/images/jobs/:id/ack`: the URL downloader confirms the result is saved; idempotently cleans up the images in this job that are marked `delete_after_read`.
- `GET /v1/images/jobs/:id/result`: query the compact status and result with the same API key; does not return the prompt, `params_json`, input images, key display info, or the image Base64 cache. Suitable for frequent polling; the original create and query endpoints are unchanged.

The create endpoint optionally accepts `storage_mode` (legacy/temporary/delete_after_read) and `retention_seconds`; if omitted, the legacy logic is kept. Example for 2-hour temporary storage: `"storage_mode":"temporary","retention_seconds":7200`. See [Image result storage policy](IMAGE_RESULT_STORAGE.md) for details, including expiry time, consumption endpoints, and safe retry semantics.

The compact query also returns HTTP 200 while the job is queued or running, with `assets` as an empty array; after success, download the image files through `job.assets[].proxy_url`, with relative paths resolved against the service address. The failure reason is in `error_message`, and partial-success notices are in `warning`. This endpoint ignores `include_cache` and never includes the image cache. Jobs that do not exist or do not belong to the current key all return 404.

```bash
curl 'https://your-host/v1/images/jobs/42/result' \
  --header 'Authorization: Bearer YOUR_API_KEY'
```

Successful response example (the signed URL is a placeholder):

```json
{"job":{"id":42,"status":"succeeded","assets":[{"id":123,"proxy_url":"/p/img/123?exp=...&sig=...","mime_type":"image/png","bytes":1500000,"width":1024,"height":1536,"model":"gpt-image-2","output_format":"png"}],"error_message":"","duration_ms":45000,"created_at":"2026-09-19T06:50:40Z"}}
```

Request example:

```json
{"model":"gpt-image-2.5-flare","prompt":"A small orange cat","size":"1024x1024","quality":"high","n":1}
```

Edit mode passes `input_images` (an array of image URLs / data URLs) to the same create endpoint. `prompt` is required and can be up to 8000 characters. Poll `job.id` and read the state from `job.status` (`queued/running/succeeded/failed`); on success, download the images from `job.assets[].proxy_url`, with relative paths resolved against this service's address. The returned object is defined by `ImageGenerationJob`; error messages are in `job.error_message`.

### 4. List Models

**Endpoint:** `GET /v1/models`

**Description:** Get the list of supported models.

**Response example:**

```json
{
  "object": "list",
  "data": [
    { "id": "gpt-6-astra", "object": "model", "owned_by": "openai" },
    { "id": "gpt-5.6-sol", "object": "model", "owned_by": "openai" },
    { "id": "gpt-5.6-terra", "object": "model", "owned_by": "openai" },
    { "id": "gpt-5.6-luna", "object": "model", "owned_by": "openai" },
    { "id": "gpt-5.5", "object": "model", "owned_by": "openai" },
    { "id": "gpt-5.3-codex-spark", "object": "model", "owned_by": "openai" },
    { "id": "gpt-image-2", "object": "model", "owned_by": "openai" }
  ]
}
```

### 5. Health Check

**Endpoint:** `GET /health`

**Description:** Health check endpoint that returns the service status.

**Response example:**

```json
{
  "status": "ok",
  "available": 5,
  "total": 8
}
```

---

### Token Estimation and Context Compaction

`POST /v1/responses/input_tokens` accepts `input/instructions/tools` and returns a local estimate such as `{"input_tokens":32}`. It is a rough estimate based on the JSON character count; it does not call the upstream and does not consume account quota, and it must not be used as an accurate tokenizer or as the basis for final billing.

`POST /v1/responses/compact` accepts `model` and the complete required `input` history, and is used to compact the context. Keep the returned `output` compaction items and their opaque fields for later requests; availability depends on the final model and channel. For an interactive example, see `/admin/docs#api-compact`.

## Admin API

All admin APIs require the `X-Admin-Key` request header for authentication.

### Statistics

#### GET /api/admin/stats

Get dashboard statistics.

**Response:**

```json
{
  "total": 10,
  "available": 8,
  "error": 2,
  "today_requests": 1234
}
```

#### GET /api/admin/health

Extended system health check.

**Response:**

```json
{
  "status": "ok",
  "available": 8,
  "total": 10
}
```

### Intelligence Check

The admin page is `/admin/intelligence-check`. The endpoints use Admin Secret authentication; for the full configuration, the judgment rules and automatic actions are described in [Intelligence Check](INTELLIGENCE_CHECK.md).

| Method | Path | Request and response |
| --- | --- | --- |
| GET | `/api/admin/intelligence-check/settings` | Returns the current settings and question bank, including `revision` |
| PUT | `/api/admin/intelligence-check/settings` | Submits the full settings together with the `revision` that was read; returns the new settings. A version conflict returns 409 |
| GET | `/api/admin/intelligence-check/accounts` | Paginated account status; supports filtering by `account_id`, `search`, `channel`, and `status` |
| PATCH | `/api/admin/intelligence-check/accounts` | Body `{ "account_ids": [1], "enabled": true }`; returns `{ "updated": 1 }` |
| POST | `/api/admin/intelligence-check/runs` | Body `{ "account_ids": [1] }`; returns 202 and `{ "runs": [...] }`; reuses the active run when the account already has one |
| GET | `/api/admin/intelligence-check/runs` | Paginated history; supports the same filter parameters as the account list |
| GET | `/api/admin/intelligence-check/runs/:id` | Returns `{ "run": {...} }`; 404 if it does not exist |
| GET | `/api/admin/intelligence-check/bars?account_ids=1,2` | Returns `{ "bars": { "1": {...}, "2": {...} } }` |

Pagination uses `page` (default 1) and `page_size` (default 20, maximum 100). The response is `{ "items": [...], "total": 0, "page": 1, "page_size": 20 }`. Account toggles and manual checks submit at most 200 accounts per request; the status bar reads at most 500 accounts per request. `channel` is `codex`. The run `status` is `queued`, `running`, `normal`, `degraded`, `error`, or `interrupted`; the `latest_status` of an account that has not been checked is `unknown`.

Run details include the config version, the question snapshot, the requested and response models, the reasoning effort, the judgment mode, the final output, elapsed time, token usage, errors, the trigger source, and the automatic actions. `source` is `manual` or `automatic`, and times use RFC3339. The status bar returns only the 20 most recent terminal run summaries, plus `enabled`, `quality_paused`, and `action`; when a run is in progress or a queued run can execute now, it also includes `active_run_id` and `active_status`. When automatic checks are turned off, queued automatic runs that cannot execute do not count as the active state of the button, and manual checks can still be submitted.

### Account Management

#### HTML Animation Intelligence Check

In the admin panel sidebar, "Intelligence Check" is at `/admin/quality-test`; you can switch between "Check Workbench", "Prompt Presets" (`?view=presets`), and "Check Records" (`?view=history`). The default question is a 2D animation of a pelican riding a bicycle drawn in SVG; you can edit the prompt and choose the account, model, and reasoning effort. The account dropdown shows a color badge for each subscription type.

There are two kinds of prompt presets. Built-in presets ship with the frontend (7 sets in total: pelican riding a bicycle, analog clock, solar system orbits, bouncing-ball physics, gear drive, night-city parallax, and Chinese character stroke order); they are not stored in the database and cannot be edited, but you can "copy as custom preset" and then modify them. Custom presets are saved in the database table `quality_test_prompts`. The "Prompt Presets" dropdown in the workbench can select either kind; if no preset is selected or the default is restored, the built-in pelican question is used. When the current prompt exactly matches a preset, that preset counts as selected; after an edit it counts as "Custom", and it can be saved as a new preset or used to update the original preset with one click from the workbench.

Checks run as server-side background jobs; switching pages, refreshing, or closing the tab does not cancel the job. The account identity and subscription snapshot, model, reasoning effort, prompt, check time, status, usage, and generated content are saved in the database. Records are shown with pagination; clicking a result previews it again and downloads the HTML; `?job=<id>` opens a result directly.

Previews use an isolated iframe that allows only inline scripts, styles, and data resources, and does not grant same-origin access to the admin panel. `GET /api/quality-test/preview` is a static preview container with no credentials and no user data; it receives HTML through parent-page messages. It does not accept persistent writes, and its separate CSP does not relax the admin panel's script restrictions.

"Download HTML" keeps the model-generated full animation and interactions. "Export SVG static snapshot" requires the preview to be open first; it saves the largest visible SVG graphic, including the paths generated by scripts, the current transforms, and the graphic styles, but not the HTML title, control buttons, or animation scripts. The export is completed through the isolated preview's message channel and does not open same-origin access; replaying, switching records, or leaving the preview cancels an unfinished export. Canvas, graphics containing `foreignObject`, or graphics that depend on external resources should be downloaded as HTML. SVG export handles at most 10000 elements and 8 MiB of content; if the limit is exceeded or the export times out, the user is prompted to retry or download the HTML.

- `GET /api/admin/accounts/:id/quality-test/options`: returns the `models` and `reasoning_efforts` of the selected runtime account. An empty string means the model default.
- `POST /api/admin/accounts/:id/quality-test`: creates a background check job for the specified account and returns `202 {"job": {...}}`; it does not switch to another account.
- `GET /api/admin/quality-tests?page=1&page_size=20`: returns `jobs`, `total`, `active_jobs`, and `concurrency_limit`. The list does not include the full prompt or HTML; at most 50 items per page.
- `GET /api/admin/quality-tests/:id`: returns `{"job": {...}}`, including the full prompt, the current generated content, and statistics; can be polled while running.
- `POST /api/admin/quality-tests/:id/cancel`: marks the running job as `cancelling`; after the executor receives the stop request, it cancels the upstream and saves a `stopped` result.
- `GET /api/admin/quality-test-prompts`: returns `{"prompts": [...]}`, sorted by update time in descending order; each item includes `id`, `name`, `prompt`, `usage_count`, `last_used_at`, `created_at`, and `updated_at`.
- `POST /api/admin/quality-test-prompts`: creates a preset; request body `{"name": "...", "prompt": "..."}`. `prompt` is required and must not exceed 16000 bytes; if `name` is left empty, the first 24 characters of the prompt are used, with a maximum of 100 characters. Returns `{"prompt": {...}}`.
- `PATCH /api/admin/quality-test-prompts/:id`: partial update; only send the fields to change; returns `404` if it does not exist.
- `DELETE /api/admin/quality-test-prompts/:id`: deletes the preset; check records already started are not affected.
- When creating a check job, you can include `prompt_id`; it is used only to accumulate the preset's `usage_count` and `last_used_at`, and is silently ignored if the preset has been deleted.

Create request example:

```json
{
  "model": "gpt-5.5",
  "reasoning_effort": "high",
  "prompt": "Create an HTML page with a 2D animation of a pelican riding a bicycle, drawn with SVG. You do not need any tests."
}
```

All the `/api/admin/*` endpoints above require `X-Admin-Key`. The model and prompt are required; the prompt can be at most 16000 bytes, a single job can run for at most 10 minutes, and the generated content can be at most 1 MiB. A unique database slot limits the running jobs across all admins, tabs, and shared database instances to 3 in total, and only 1 active job is allowed per account; when the slots are full or the account is duplicated, `409` is returned without queuing. A job that is being stopped still holds its slot and releases it when the executor exits.

Job statuses are `running`, `cancelling`, `completed`, `error`, `stopped`, and `interrupted`. A normal service shutdown cancels active jobs and saves an interrupted result; jobs left behind by a process crash are cleaned up after the original 10-minute deadline plus a 30-second grace period, and are not retried upstream automatically. The new table `quality_test_jobs` is created automatically and is compatible with PostgreSQL and SQLite. Results that previously existed only in page memory cannot be migrated retroactively.

Reasoning effort uses `reasoning.effort`; if a specific model does not support that level, an upstream error is returned. The test reuses the proxy, credential, usage, and cooldown handling of the account connection test. It consumes upstream quota and does not go through the API key scheduling, billing, or request-body rewrite rules of the public `/v1/*`.

The background executor consumes the complete upstream stream, keeping the pure model text and the final statistics that arrive after the completion event; the frontend reads progress through the job API. Elapsed time, time to first output segment, and the token data provided by the upstream are used only as aids for comparison; no automatic "intelligence score" is returned. One animation result cannot prove that model quality has declined.

#### GET /api/admin/accounts

Get the account list.

**Response:**

```json
{
  "accounts": [
    {
      "id": 1,
      "name": "account-1",
      "email": "user@example.com",
      "token_workspace_id": "personal-workspace-id",
      "workspace_id_override": "team-workspace-id",
      "effective_workspace_id": "team-workspace-id",
      "plan_type": "pro",
      "status": "ready",
      "health_tier": "healthy",
      "scheduler_score": 100,
      "dispatch_score": 150,
      "score_bias_override": null,
      "score_bias_effective": 50,
      "base_concurrency_override": null,
      "base_concurrency_effective": 2,
      "skip_warm_tier": false,
      "dynamic_concurrency_limit": 2,
      "allowed_api_key_ids": [1, 3],
      "proxy_url": "http://proxy.example.com:8080",
      "created_at": "2024-01-01T00:00:00Z",
      "updated_at": "2024-01-01T12:00:00Z",
      "active_requests": 0,
      "total_requests": 100,
      "last_used_at": "2024-01-01T11:00:00Z",
      "success_requests": 95,
      "error_requests": 5,
      "credit_enabled": false,
      "credit_skip_usage_window": false,
      "billed_5h": 0.25,
      "billed_7d": 3.50,
      "usage_percent_7d": 45.2,
      "usage_percent_5h": 12.5,
      "reset_5h_at": "2024-01-01T17:00:00Z",
      "reset_7d_at": "2024-01-08T00:00:00Z",
      "scheduler_breakdown": {
        "unauthorized_penalty": 0,
        "rate_limit_penalty": 0,
        "timeout_penalty": 0,
        "server_penalty": 0,
        "failure_penalty": 0,
        "success_bonus": 12,
        "usage_penalty_7d": -5,
        "usage_urgency_bonus_5h": 0,
        "latency_penalty": 0,
        "success_rate_penalty": 0
      }
    }
  ]
}
```

Additional field notes:

| Field                      | Type         | Description |
| -------------------------- | ------------ | ----------------------------------------------------------------- |
| scheduler_score            | number       | Raw health score; reflects only the dynamic scheduling health state |
| dispatch_score             | number       | Final score used for scheduling order; the runtime snapshot is read first |
| score_bias_override        | integer/null | Manually configured total weighted-score override; `null` means follow the plan default |
| score_bias_effective       | integer      | Weighted score currently in effect |
| base_concurrency_override  | integer/null | Manually configured base concurrency override; `null` means follow the global `max_concurrency` |
| base_concurrency_effective | integer      | Base concurrency value currently in effect |
| skip_warm_tier             | bool         | Whether to skip the warm tier; it only promotes warm to healthy and does not override risky/banned |
| allowed_api_key_ids        | integer[]    | List of API key IDs allowed to call this account; an empty array means all API keys can call it |
| token_workspace_id         | string       | Default workspace ID recognized in the token |
| workspace_id_override      | string       | Target workspace specified by `Chatgpt-Account-Id`; empty when not specified |
| effective_workspace_id     | string       | Workspace actually routed to; uses the override value first, otherwise the token's default workspace |
| credit_enabled             | bool         | Whether the account uses credit billing mode |
| credit_skip_usage_window   | bool         | Whether to skip the 7-day / 5-hour usage window penalty |
| billed_5h                  | number/null  | Accumulated billed amount (USD) within the past 5-hour window |
| billed_7d                  | number/null  | Accumulated billed amount (USD) within the past 7-day window |

#### GET /api/admin/accounts/live

Returns the runtime concurrency counts and usage projection for the accounts on the current page, for the account page's lightweight poll of about once per second. It reads only in-memory state; it does not query the database or rebuild the scheduling snapshot.

**Request parameters:**

| Parameter | Type | Description |
| ---- | ------ | ---------------------------------- |
| ids | string | Comma-separated account IDs, up to 500 |

**Response:**

```json
{
  "accounts": {
    "1": {
      "active_requests": 1,
      "occupied_requests": 1,
      "dynamic_concurrency_limit": 2,
      "base_concurrency_effective": 2,
      "status": "quota_paused",
      "using_credits": false,
      "plan_type": "pro",
      "credits_valid": true,
      "credits_balance": "12.5",
      "credits_has_credits": true,
      "credits_unlimited": false,
      "credits_overage_limit_reached": false,
      "credits_spend_control_reached": null,
      "credits_rate_limit_reached_type": "",
      "usage_percent_5h": 91.2,
      "reset_5h_at": "2026-10-18T18:00:00+08:00",
      "codex_5h_usage_updated_at": "2026-10-18T15:10:00+08:00",
      "usage_percent_7d": 42,
      "reset_7d_at": "2026-10-25T07:00:00+08:00",
      "codex_usage_updated_at": "2026-10-18T15:10:00+08:00",
      "codex_usage_frame_at": "2026-10-18T15:10:00+08:00",
      "codex_usage_frame_kind": "codex.rate_limits"
    }
  },
  "session_slot_buffer_enabled": false
}
```

**Field notes:**

| Field | Type | Description |
| ---- | ---- | ---- |
| status / using_credits | string / bool | Runtime status; older backends omit this key, and the frontend keeps the list value |
| plan_type | string | Latest runtime plan, using the account list field; kept from the list when an older backend omits it |
| credits_valid / credits_balance | bool / string/null | Whether the current credits snapshot has been observed, and the balance; an unknown or hidden balance is `null`, which clears the old displayed value |
| credits_has_credits / credits_unlimited / credits_overage_limit_reached | bool/null | Credits business flags carried over from the account list; `null` when no snapshot has been observed |
| credits_spend_control_reached / credits_rate_limit_reached_type | bool/null / string | Spend limit reached and workspace hard stop; an unknown spend limit is `null`, and no hard stop is `""` |
| usage_percent_5h / usage_percent_7d | number/null | Runtime usage; an explicit `null` means the window has expired, and the frontend clears the list value |
| reset_5h_at / reset_7d_at | string/null | Window reset time (RFC3339) |
| codex_5h_usage_updated_at / codex_usage_updated_at | string/null | Time of the last usage refresh; each WS quota frame per turn also refreshes it |
| codex_usage_frame_at / codex_usage_frame_kind | string | Time and event type of the most recently applied WS quota frame; omitted before the first frame; can be used to confirm that frame ingestion is working |
| dynamic_concurrency_limit / base_concurrency_effective | integer | Concurrency limit currently in effect in the scheduler, and the configured value |
| session_slot_buffer_enabled | bool | Global session slot buffer switch |

A missing key means an older backend, and the frontend keeps the list value; an explicit `null` in a usage field means the window has expired. The live plan and credits fields are projected only to rows that use the Codex runtime fields.

Connection-level quota frames that arrive after the terminal state also update the fields above. Only frames that pass the observation ordering gate advance the frame time, credits, and usage. When there is contention on the concurrent observation lock, frames are queued and applied in their original order without waiting for the database. Database writes for usage, credits, plan, and cooldown are merged and run in the background, so live results can become visible before the persisted results.

#### PATCH /api/admin/accounts/:id/scheduler

Update the account scheduling configuration.

**Request:**

```json
{
  "score_bias_override": 80,
  "base_concurrency_override": 6,
  "skip_warm_tier": true,
  "allowed_api_key_ids": [1, 3]
}
```

Each field can be set to `null` to restore its automatic value:

```json
{
  "score_bias_override": null,
  "base_concurrency_override": null,
  "allowed_api_key_ids": null
}
```

**Parameters:**

| Parameter                 | Type           | Required | Description |
| ------------------------- | -------------- | ---- | ---------------------------------------------------------------------------------------------------------- |
| score_bias_override       | integer/null   | No       | Total weighted-score override, range `-200..200`; `null` restores the plan default |
| base_concurrency_override | integer/null   | No       | Base concurrency override, `≥1` with no upper limit; `null` restores the global default |
| skip_warm_tier            | boolean/null   | No       | Whether to skip the warm tier; `null` is the same as `false`; when the field is omitted, the current value is kept |
| allowed_api_key_ids       | integer[]/null | No       | List of API key IDs allowed to call this account, saved deduplicated and in ascending order; when the field is omitted, the current value is kept; passing `null` or `[]` restores it to all callers |

**Response:**

```json
{
  "message": "Account scheduling configuration updated"
}
```

#### PATCH /api/admin/accounts/:id/credit

Update the account credit settings.

**Request:**

```json
{
  "credit_enabled": true,
  "credit_skip_usage_window": true
}
```

**Parameters:**

| Parameter                  | Type  | Required | Description                              |
| -------------------------- | ----- | ---- | ---------------------------------------- |
| credit_enabled             | bool  | No       | Marks the account as credit billing mode; kept unchanged when omitted |
| credit_skip_usage_window   | bool  | No       | Skips the 7-day / 5-hour usage window penalty (effective only when `credit_enabled=true`); kept unchanged when omitted |

**Response:**

```json
{
  "message": "Credit settings updated",
  "credit_enabled": true,
  "credit_skip_usage_window": true
}
```

#### POST /api/admin/accounts/batch-update

Batch-update account enabled state, lock state, tags, groups, and scheduling metadata. Omitted fields keep their current values; `ids` is deduplicated automatically, and deleted or nonexistent accounts are counted as `failed`.

At least one field to update must be provided; submitting only `ids` returns `400 Bad Request`:

```json
{
  "error": "Please provide the fields to update"
}
```

**Request:**

```json
{
  "ids": [1, 2, 3],
  "enabled": false,
  "locked": true,
  "score_bias_override": 25,
  "base_concurrency_override": 4,
  "scheduler_priority": 10,
  "tags": ["ops", "paid"],
  "group_ids": [1],
  "auto_pause_5h_threshold": 0.8,
  "auto_pause_7d_disabled": true
}
```

Common batch scheduling fields:

| Field | Type | Range / semantics |
|------|------|-----------|
| `score_bias_override` | integer/null | `-200..200`; `null` restores the plan default score bias |
| `base_concurrency_override` | integer/null | `≥1` with no upper limit; `null` restores the inherited group or global value |
| `scheduler_priority` | integer/null | `-100..100`; `null` restores the default priority `0` |
| `tags` | string[] | Replaces the account tags; an empty array clears them |
| `group_ids` | integer[] | Replaces the account groups; an empty array clears them |
| `timezone` | string | Binds an IANA time zone (for example `America/New_York`); an empty string clears it. Official Codex accounts use it to rewrite `<timezone>` and `<current_date>` in the `environment_context` of the outgoing request body (the date is shifted as a whole by the same-day difference between the account time zone and the client time zone); empty means the client value is passed through |

**Response:**

```json
{
  "message": "Updated 3 accounts, 0 failed",
  "success": 3,
  "failed": 0
}
```

#### POST /api/admin/accounts/batch-refresh-usage

Batch-refresh usage for all Codex accounts that support WHAM in the current running pool, corresponding to the "Manage → Refresh all usage" button on the account management page. The target scope covers all pages and is independent of the current search, filters, and selection; accounts without an access token and Agent Identity accounts are not included.

This operation only queries `/backend-api/wham/usage` and updates the 5-hour / weekly usage snapshot, without refreshing the login token, and it does not fall back to the `/responses` probe that consumes tokens. A failed query keeps the previous usage, and a WHAM 401 on its own does not mark the account as having invalid credentials.

No request parameters are needed. By default it returns `type: "complete"`, `total`, `current`, `success`, and `failed`; appending `?stream=true` returns `start` / `progress` / `complete` SSE events, with `action` fixed to `batch_usage_refresh`. Per-account progress includes the account identifier, status, and error message.

Concurrency follows `usage_probe_concurrency`, and a single query takes at most 15 seconds. If the same instance is already running this batch operation, a further call returns 409; when the client disconnects, the queries and pending tasks are canceled. Before the completion event is sent, the account list and analytics cache are invalidated, so a subsequent read updates the usage progress bars.

#### POST /api/admin/accounts

Add Refresh Token accounts (batch supported).

**Request:**

```json
{
  "name": "my-account",
  "refresh_token": "rt_xxxxxxxxxxxx",
  "proxy_url": "http://proxy.example.com:8080",
  "custom_headers": {
    "Chatgpt-Account-Id": "team-workspace-id"
  }
}
```

**Parameters:**

| Parameter     | Type   | Required | Description |
| ------------- | ------ | ---- | ------------------------------------------------------ |
| name          | string | No       | Account name; a sequence number is appended automatically in batch mode; default `account-{n}` |
| refresh_token | string | Yes      | Refresh Token; separate multiple tokens with `\n` (up to 100 per request) |
| proxy_url     | string | No       | Proxy URL |
| custom_headers | object | No   | Custom upstream request headers; `Chatgpt-Account-Id` specifies the target workspace |

The same RT can join the account pool through separate routes: the "default workspace" and several `Chatgpt-Account-Id` values. Quota, cooldown, scheduling and statistics are kept separately for each account record; the same target workspace under one login identity is still deduplicated.

Batch add (separated by newlines):

```json
{
  "name": "batch",
  "refresh_token": "rt_xxx1\nrt_xxx2\nrt_xxx3",
  "proxy_url": ""
}
```

**Response:**

```json
{
  "message": "Added 3 accounts",
  "success": 3,
  "failed": 0
}
```

**curl example:**

Single add:

```bash
curl -X POST http://localhost:8080/api/admin/accounts \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-account", "refresh_token": "rt_xxxxxxxxxxxx", "proxy_url": ""}'
```

Batch add (newline-separated):

```bash
curl -X POST http://localhost:8080/api/admin/accounts \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "batch", "refresh_token": "rt_xxx1\nrt_xxx2\nrt_xxx3"}'
```

> After adding, the system refreshes the Access Token automatically in the background. No manual trigger is needed.

#### POST /api/admin/accounts/at

Add AT-only accounts with an Access Token (batch supported). Use this when you have an AT but no RT.

**Request:**

```json
{
  "name": "my-at-account",
  "access_token": "eyJhbGciOiJSUzI1NiIs...",
  "proxy_url": "http://proxy.example.com:8080",
  "custom_headers": {
    "Chatgpt-Account-Id": "team-workspace-id"
  }
}
```

**Parameters:**

| Parameter    | Type   | Required | Description |
| ------------ | ------ | ---- | ----------------------------------------------------- |
| name         | string | No   | Account name; batch adds append a sequence number automatically, default `at-account-{n}` |
| access_token | string | Yes  | Access Token; separate multiple tokens with `\n` (up to 100 per request) |
| proxy_url    | string | No   | Proxy URL |
| custom_headers | object | No | Custom upstream request headers; `Chatgpt-Account-Id` specifies the target workspace |

The same AT can be saved as several independent routes, one per target workspace. When the login identity cannot be parsed from the AT, the system at least prevents the same route from being written twice, keyed on "raw AT + target workspace".

Batch add:

```json
{
  "name": "batch-at",
  "access_token": "eyJtoken1...\neyJtoken2...\neyJtoken3...",
  "proxy_url": ""
}
```

**Response:**

```json
{
  "message": "Added 3 AT accounts",
  "success": 3,
  "failed": 0
}
```

**curl example:**

```bash
curl -X POST http://localhost:8080/api/admin/accounts/at \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-at", "access_token": "eyJhbGciOiJSUzI1NiIs..."}'
```

> AT-only accounts cannot be refreshed automatically; add them again after they expire. The system parses the JWT automatically to extract information such as email and plan_type.

#### DELETE /api/admin/accounts/:id

Delete an account (soft delete, marked as deleted).

**Response:**

```json
{
  "message": "Account deleted"
}
```

#### POST /api/admin/accounts/:id/refresh

Manually refresh the account's Access Token.

**Response:**

```json
{
  "message": "Account refreshed successfully"
}
```

#### GET /api/admin/accounts/:id/test

Test the account connection.

**Response:** `text/event-stream`. Example events for a successful connection test:

```text
data: {"type":"test_start","model":"gpt-5.5"}

data: {"type":"content","text":"pong"}

data: {"type":"test_complete","success":true}

data: {"type":"diagnostics","codex_diagnostics":{"model":"gpt-5.5","http_status":200,"duration_ms":523}}
```

`diagnostics` events carry a `codex_diagnostics` field; failures are returned through an `error` event. After the upstream response headers arrive, one diagnostics frame containing only the status code and response header information is sent first. When the stream ends, a final frame carrying `duration_ms` is sent; the final frame may come after `test_complete`/`error`, so the client should wait until the SSE stream closes before refreshing the account snapshot. If the request fails before response headers arrive (DNS/proxy/timeout), no separate `diagnostics` event is sent; the diagnostics object is attached directly to the `error` event and contains only `model` and `duration_ms`.

For WebSocket diagnostics, the legacy handshake headers from the connection pool are not used: `headers_ms` is left empty, and `first_frame_ms` is the wait time for the first upstream frame of this request. `request_id`, `cf_ray` and the response headers come from this request's metadata frame; for headers with the same name, the latest frame wins. Fields that were not received are left empty.

The `codex_diagnostics` object of a Codex connection test contains:

- `http_status`, `headers_ms` (time to receive the HTTP response headers), `first_frame_ms` (time to the first WS frame), `first_content_ms` (time to the first text segment), `duration_ms` (total time), in milliseconds; fields that were not observed are omitted, not reported as zero.
- `model` (requested model), `response_model` (upstream `response.model`), `transport` (`http` / `websocket`; in forced WS mode the usage window comes from the `codex.rate_limits` frame instead of the response headers).
- `request_id` (prefers the account's custom `upstream_request_id_header`; otherwise tries `x-request-id`, `request-id`, `x-openai-request-id`, `x-oai-request-id`, `x-goog-request-id` in that order), `response_id` (`response.id`), `cf_ray`, `plan_type` (`x-codex-plan-type`).
- `safety_buffering_enabled` / `safety_buffering_faster_model`: upstream `x-codex-safety-buffering-*` headers (HTTP response headers, or the `codex.response.metadata` frame over WS). `enabled` only means that the model has "safety buffering" capability on (the upstream may hold back output for extra review), while `faster_model` is the switch target of the official CLI's "Retry with a faster model" option, not the model that answered this request. Only `safety_buffered=true` means that this turn's events included `safety_buffering: true`.
- `response_status` (the last observed `response.status`; normal terminal states are `completed` / `failed` / `incomplete`; if the stream is interrupted before a terminal state, it stays at an intermediate state such as `in_progress`), `incomplete_reason`, `error_type`, `error_code` (from an in-stream `error` event, `response.error`, `response.status_details.error`, or the JSON body of a non-200 response).
- `primary_window` / `secondary_window`: the `x-codex-primary-*` / `x-codex-secondary-*` triplets, projected as-is into `{used_percent, window_minutes, reset_after_seconds}`; `window_minutes` decides whether a window is a 5h (≥ 60) or 7d (≥ 1440) window.
- `usage`: terminal-state `input_tokens`, `output_tokens`, `total_tokens`, `cached_input_tokens` (`input_tokens_details.cached_tokens`), `reasoning_output_tokens` (`output_tokens_details.reasoning_tokens`); each value is overwritten by the latest terminal state.
- `response_headers`: diagnostic response headers filtered by an allowlist (`x-codex-*`, `x-ratelimit-*`, `openai-*`, request IDs, `retry-after`, `cf-ray`, etc.; cookies and auth headers are excluded); `response_body`: the JSON/SSE body that was read, as a redacted preview (Access Tokens, API keys and proxy credentials are replaced), up to 64 KiB; when truncated, `body_truncated=true`.

```text
data: {"type":"test_start","model":"gpt-5.5"}

data: {"type":"diagnostics","codex_diagnostics":{"model":"gpt-5.5","http_status":200,"headers_ms":412,"transport":"http","request_id":"req_x","plan_type":"plus","primary_window":{"used_percent":12.5,"window_minutes":300,"reset_after_seconds":1800}}}

data: {"type":"content","text":"pong"}

data: {"type":"test_complete","success":true}

data: {"type":"diagnostics","codex_diagnostics":{"model":"gpt-5.5","http_status":200,"headers_ms":412,"first_content_ms":980,"duration_ms":1210,"response_id":"resp_x","response_status":"completed","usage":{"input_tokens":20,"output_tokens":3,"total_tokens":23}}}
```

#### POST /api/admin/accounts/:id/models/probe

Read-only: concurrently probes the text models visible to the account, and returns `available` and a per-model `outcome` (`available`, `unsupported`, `throttled`, `error`). Model probes do not write cooldown, error or scheduling state for the account; append `?stream=true` to receive SSE progress.

#### POST /api/admin/accounts/:id/models/sync-upstream

Read-only: fetches the upstream model catalog of the given account without overwriting its allowlist. After you confirm, save it with the PATCH endpoint below.

#### PATCH /api/admin/accounts/:id/models

Sets the account-level model allowlist. An empty array clears the allowlist and allows all models; with a non-empty array, the scheduler only routes requests for allowlisted models to this account. The server validates the format of each model name, and returns `400` if there are more than 200.

```json
{
  "models": ["gpt-5.5"]
}
```

#### POST /api/admin/accounts/:id/usage/refresh

Synchronously refreshes the usage snapshot of one account, preferring the zero-cost wham endpoint. Returns `refreshed: true` and the account's latest 5-hour / 7-day usage fields.

#### GET /api/admin/accounts/:id/usage

Get usage statistics for one account.

**Response:**

```json
{
  "id": 1,
  "name": "account-1",
  "total_requests": 100,
  "total_tokens": 5000,
  "last_7d_requests": 500,
  "last_7d_tokens": 25000
}
```

#### POST /api/admin/accounts/import

Batch import accounts (TXT, JSON and AT-TXT formats supported).

**Request:**

- Method: POST
- Content-Type: multipart/form-data

**Form fields:**

| Field        | Type   | Required | Description |
| ------------ | ------ | ---- | ------------------------------------------ |
| file         | file   | Yes  | Uploaded file (max 20 MB; JSON format supports multiple files) |
| format       | string | No   | File format: `txt` (default), `json`, `at_txt` |
| proxy_url    | string | No   | Proxy URL |
| import_proxy | bool   | No   | Use the proxies carried in the file and register them in the proxy pool |

**import_proxy notes:**

With `true`, the `proxy_url` carried by each account in the JSON file takes effect (taking precedence over the form's `proxy_url`), these proxies are written to the proxy table and synced into the in-memory proxy pool first, and only then are the accounts written: the order matters, because an account bound first to a managed proxy that is not yet in the pool is judged to have no usable egress and cannot be scheduled. The TXT format carries one token per line and cannot carry a proxy, so this switch has no effect on it.

Details:

- At most 500 proxies are registered per import; if the limit is exceeded, none are registered and all accounts fall back to the form proxy;
- Proxy entries with an invalid format are skipped, and their accounts fall back to the form proxy; an unregistered URL is never bound;
- An existing proxy with the same URL is skipped via `ON CONFLICT DO NOTHING`; it is **not** revived or relabeled, so if it is disabled or in a failed-test state locally, the accounts bound to it are not scheduled, and the response includes a warning;
- Proxies marked disabled at the source are always imported as enabled, with a warning in the response;
- Newly registered proxies are tagged `imported-<YYYYMMDD-HHmm>`, so each batch can be filtered and cleaned up later;
- When an existing account matches, the file's proxy does **not** overwrite that account's existing binding (it only fills empty bindings); a `proxy_url` entered in the form keeps the existing overwrite behavior.

The SSE `complete` event includes `proxies_imported` / `proxies_skipped` / `warning`.

For the rules above (write order, 500-entry limit, fallback for invalid entries, no revival of existing proxies, no overwrite of existing bindings), see [proxy_pool.md](proxy_pool.md#migrating-proxy-bindings-with-account-export-and-import).

**format details:**

- **`txt`** — one Refresh Token per line:

  ```text
  rt_xxxxxx1
  rt_xxxxxx2
  rt_xxxxxx3
  ```

- **`json`** — CLIProxyAPI credential JSON format (an array or a single object is supported):

  ```json
  [
    { "refresh_token": "rt_xxx1", "email": "user1@example.com" },
    { "refresh_token": "rt_xxx2", "email": "user2@example.com" }
  ]
  ```

- **`at_txt`** — one Access Token per line (AT-only mode):

  ```text
  eyJhbGciOiJSUzI1NiIs...token1
  eyJhbGciOiJSUzI1NiIs...token2
  ```

> All formats are deduplicated automatically within the file and against the database; tokens that already exist are counted as `duplicate` and are not imported again.

**curl example:**

Import RTs (TXT format):

```bash
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@tokens.txt" \
  -F "format=txt" \
  -F "proxy_url=http://proxy.example.com:8080"
```

Import RTs (JSON format):

```bash
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@credentials.json" \
  -F "format=json"
```

Import ATs (AT-TXT format):

```bash
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@access_tokens.txt" \
  -F "format=at_txt"
```

**Response:** SSE streaming progress

```text
data: {"type":"progress","current":5,"total":10,"success":3,"duplicate":1,"failed":1}

data: {"type":"complete","current":10,"total":10,"success":8,"duplicate":1,"failed":1}
```

If all tokens already exist, a plain JSON response (not SSE) is returned:

```json
{
  "message": "All 10 RTs already exist; nothing to import",
  "success": 0,
  "duplicate": 10,
  "failed": 0,
  "total": 10
}
```

#### POST /api/admin/accounts/batch-test

Batch test account connections.

**Request:**

```json
{
  "ids": [1, 2, 3],
  "concurrency": 5
}
```

**Response:** SSE streaming progress

```
data: {"type":"progress","current":3,"total":3,"success":2,"failed":1}

data: {"type":"complete","current":3,"total":3,"success":2,"failed":1}
```

#### POST /api/admin/accounts/clean-banned

Clean up Unauthorized (401) accounts.

**Response:**

```json
{
  "message": "Cleaned up 5 accounts",
  "cleaned": 5
}
```

#### POST /api/admin/accounts/clean-rate-limited

Clean up Rate Limited (429) accounts.

#### POST /api/admin/accounts/clean-error

Clean up accounts in Error status.

#### GET /api/admin/accounts/export

Export accounts (standard JSON format).

**Query parameters:**

- `filter`: healthy (export healthy accounts only)
- `ids`: 1,2,3 (list of IDs)
- `remote`: true (remote migration mode)
- `include_proxy`: 1 (also export each account's bound proxy; off by default)

**Response:**

```json
[
  {
    "type": "codex",
    "email": "user@example.com",
    "expired": "2024-12-31T23:59:59Z",
    "id_token": "id_xxx",
    "account_id": "acc_xxx",
    "access_token": "at_xxx",
    "last_refresh": "2024-01-01T12:00:00Z",
    "refresh_token": "rt_xxx"
  }
]
```

**include_proxy notes:**

When enabled, each entry gets three extra fields, `proxy_url` / `proxy_label` / `proxy_enabled`; together with the importing side's `import_proxy`, this moves the whole "account pool + proxy bindings" over. The three fields only appear when the account is actually bound to a proxy; accounts without a binding do not get empty fields.

> ⚠️ Proxy URLs often embed a username and password, and the export file already contains plaintext `refresh_token` values, so sensitivity rises when this is enabled;
> handle the file as a confidential file.

`/accounts/recycle-bin/export` also supports this parameter.

Remote migration mode with `remote=true` also omits proxies by default: the target machine may not be able to reach the source machine's proxy network, and silently inheriting them would bind the whole batch of accounts to an unreachable egress.

A proxy bound only to a group and delivered by that group is not the account's own binding, so account exports do not carry it; the target side needs to create a group with the same name first.

#### POST /api/admin/accounts/migrate

Migrate accounts from a remote codex2api instance.

**Request:**

```json
{
  "url": "http://remote-instance:8080",
  "admin_key": "remote-admin-secret"
}
```

**Response:** SSE streaming progress

#### GET /api/admin/accounts/event-trend

Get the account add/remove trend.

**Query parameters:**

- `start`: start time in RFC3339 format
- `end`: end time in RFC3339 format
- `bucket_minutes`: aggregation bucket size (default 60)

**Response:**

```json
{
  "trend": [{ "timestamp": "2024-01-01T00:00:00Z", "added": 5, "deleted": 0 }]
}
```

### Usage statistics

#### GET /api/admin/usage/stats

Get usage statistics.

**Response:**

```json
{
  "total_requests": 10000,
  "total_tokens": 500000,
  "today_requests": 500,
  "today_tokens": 25000,
  "rpm": 50,
  "tpm": 2500,
  "error_rate": 0.02
}
```

#### GET /api/admin/usage/logs

Get usage logs.

The `X-Codex2API-Request-ID` header on HTTP `/v1/*` responses maps to the `request_id` that can be searched below; browsers can read it through CORS. The existing `X-Request-ID` is the request-context / access-log ID and may echo a value the client sent; it is independent of this gateway trace ID. To troubleshoot usage, use `X-Codex2API-Request-ID`. For usage from automatic compaction, `parent_request_id` prefers the gateway trace ID of the parent request; `/v1/live` settlement reuses the trace information of the request that opened the connection.

**Query parameters:**

- `start`: start time in RFC3339
- `end`: end time in RFC3339
- `page`: page number
- `page_size`: items per page (max 200)
- `email`: filter by account email
- `model`: filter by model
- `endpoint`: filter by endpoint
- `api_key_id`: filter by API key ID
- `request_id`: gateway trace ID, exact match
- `upstream_request_id`: upstream request ID, exact match
- `q`: fuzzy search across gateway and upstream request IDs
- `fast`: true/false (whether the request used the fast service)
- `stream`: true/false (whether streaming)

**Response:**

```json
{
  "logs": [
    {
      "id": 1,
      "account_id": 1,
      "request_id": "019-example-gateway-id",
      "upstream_request_id": "req_example",
      "upstream_proxy_id": 1,
      "upstream_proxy_name": "local-egress",
      "injected_turn_state": "",
      "upstream_turn_state": "gAAAAABo...",
      "account_email": "user@example.com",
      "api_key_id": 3,
      "api_key_name": "Team A",
      "api_key_masked": "sk-t****...****1234",
      "endpoint": "/v1/chat/completions",
      "model": "gpt-5.5",
      "status_code": 200,
      "duration_ms": 523,
      "first_token_ms": 150,
      "prompt_tokens": 25,
      "completion_tokens": 15,
      "total_tokens": 40,
      "created_at": "2024-01-01T12:00:00Z"
    }
  ],
  "total": 1000
}
```

`upstream_turn_state` is the `X-Codex-Turn-State` returned by the upstream response (taken from the response headers over HTTP, or from the in-stream metadata frame over WebSocket); an empty string means none. `injected_turn_state` and `turn_state_overridden` are kept only for usage records written by earlier versions.

`first_token_ms` uses a loose definition for Responses upstreams: from the start of this upstream attempt to the first non-terminal response event other than `response.created` / `response.in_progress`, including structural frames with no visible text. It is not the time to the first body character, and it excludes the inbound processing and scheduling wait before the attempt started, so it cannot be compared directly with a time-to-first-token inferred from client session logs.

`ws_acquire_ms` covers the queueing, liveness probe and handshake time to acquire a WebSocket connection. It is counted in `first_token_ms` by default; with `first_token_excludes_ws_acquire` enabled, adding the two columns recovers the first-response time including connection acquisition. `output_tokens` includes invisible output such as reasoning; when comparing visible text speed, also check `reasoning_tokens`, the reasoning effort, and the time of the first actual content frame. Do not attribute differences to average first-token values from different sources alone.

#### GET /api/admin/usage/chart-data

Get aggregated chart data.

**Query parameters:**

- `start`: RFC3339 start time
- `end`: RFC3339 end time
- `bucket_minutes`: aggregation bucket size (default 5)

**Response:**

```json
{
  "buckets": [
    {
      "time": "2024-01-01T12:00:00Z",
      "requests": 50,
      "tokens": 2500,
      "latency_ms": 500
    }
  ],
  "total_requests": 1000,
  "total_tokens": 50000
}
```

#### DELETE /api/admin/usage/logs

Clear usage logs.

**Response:**

```json
{
  "message": "Logs cleared"
}
```

### Model pricing sync

`GET /api/admin/model-pricing` returns the reference source addresses `litellm_url`, `models_dev_url`, `default_sync_url` and the official HTML page address. The `url` for `POST /api/admin/model-pricing/sync` supports the LiteLLM, models.dev and the project's flat JSON formats; manually set prices are not overwritten.

`POST /api/admin/model-pricing/official-sync` validates the HTML prices of the selected official source. If a fetch fails or a price is missing, it falls back per model in order to LiteLLM, models.dev and the project default JSON, and finally restores the built-in default prices. The response's `sources` lists the sources that supplied a valid price, `fallbacks` maps each model to its reference source URL, `missing` lists the models for which no source provided a valid price, `warnings` contains only fetch, parse and format-validation errors, and `notices` contains ordinary messages for successful fallbacks and for using default prices. Channels that are not selected do not take part in the sync, and manually set administrator prices take priority.

### Model image-generation billing settings

`PUT /api/admin/model-pricing` (authenticated with `X-Admin-Key`) configures the user billing mode for image models:

```json
{
  "model": "gpt-image-2",
  "pricing": {
    "user_billing_mode": "per_image",
    "image_unit_price": 0.05
  }
}
```

`user_billing_mode` is `token` (default) or `per_image`; the per-image mode only accepts image models, and `image_unit_price` must be a USD amount greater than 0. `GET /api/admin/model-pricing` returns the settings currently in effect. On save, `pricing` replaces the model's entire manual override; to keep custom token cost prices, submit the original fields together. `{"model":"gpt-image-2","reset":true}` clears the manual override.

In per-image mode, the number of successful images × the unit price is written to `user_billed`, while the upstream token cost is still written to `account_billed`. The usage log and the API key public usage records gain `user_billing_mode`, `image_unit_price` and `billed_image_count`. Failed or undelivered images cost 0, and historical records are not re-billed when prices change. The `image_pricing` field of the public workspace quota response only provides the user billing mode and unit price, not the upstream cost rate. For the full behavior, see [per-image image generation billing](CONFIGURATION.md#per-image-billing-for-image-generation).

### API key management

#### GET /api/admin/keys

Get all API keys. Admin endpoints require `X-Admin-Key`; these endpoints are not part of the external `/v1/*` client API. This endpoint returns the full key in `raw_key`, so use it only from a trusted admin panel.

**Response:**

```json
{
  "keys": [
    {
      "id": 1,
      "name": "Codex CLI",
      "key": "sk-****...abcd",
      "raw_key": "sk-live-full-key",
      "quota_limit": 10,
      "quota_used": 1.25,
      "expires_at": "2026-06-01T00:00:00Z",
      "allowed_group_ids": [1],
      "status": "active",
      "created_at": "2024-01-01T00:00:00Z"
    }
  ]
}
```

#### POST /api/admin/keys

Create a new API key.

When creating a key, `limits.allow_live` defaults to `true`; set it explicitly to `false` to turn off ChatGPT Live. The admin panel can adjust this switch both when creating and when editing a key; the settings of existing keys do not change automatically.

**Request:**

```json
{
  "name": "production",
  "key": "sk-custom-key",
  "quota_limit": 10,
  "expires_in_days": 30,
  "allowed_group_ids": [1]
}
```

| Field               | Type      | Required | Description |
| ------------------- | --------- | ---- | -------------------------------------- |
| name                | string    | Yes      | Display name |
| key                 | string    | No       | Custom key; if omitted, one is generated automatically |
| quota_limit / quota | number    | No       | Quota limit; 0 or omitted means no limit |
| expires_at          | string    | No       | RFC3339 or local date-time |
| expires_in_days     | number    | No       | Expires after N days; 0 means no expiry |
| allowed_group_ids   | integer[] | No       | Account groups allowed for scheduling; an empty array means all groups |

**Response:**

```json
{
  "id": 2,
  "key": "sk-xxxxxxxxxxxxxxxxxxxxxxxx",
  "name": "production",
  "quota_limit": 10,
  "quota_used": 0,
  "expires_at": "2026-06-12T00:00:00Z",
  "allowed_group_ids": [1]
}
```

#### PATCH /api/admin/keys/:id

Edit the API key name, quota, expiry time and allowed account groups. Omitted fields keep their current values.

**Request:**

```json
{
  "name": "Cherry Studio",
  "quota_limit": 25,
  "expires_at": null,
  "allowed_group_ids": []
}
```

| Field               | Type        | Description |
| ------------------- | ----------- | -------------------------------------- |
| name                | string      | New display name |
| quota_limit / quota | number/null | New quota limit; 0 or null clears the quota limit |
| expires_at          | string/null | New expiry time; null clears the expiry time |
| expires_in_days     | number      | Expires after N days; 0 clears the expiry time |
| allowed_group_ids   | integer[]   | Account groups allowed for scheduling; an empty array means all groups |

**Response:**

```json
{
  "message": "API Key updated"
}
```

#### DELETE /api/admin/keys/:id

Delete the API key.

**Response:**

```json
{
  "message": "Deleted"
}
```

### Account group management

Account groups split the account pool into multiple schedulable sets. An API key's `allowed_group_ids` can limit a downstream key to specific groups; an account's own `allowed_api_key_ids` can also restrict, in reverse, which API keys may schedule that account. A group can also supply a base concurrency value to its member accounts through `base_concurrency_override`: an account-level override takes precedence; when an account belongs to several groups, the smallest effective group value is used; when none is set, it falls back to the global `max_concurrency`.

#### GET /api/admin/account-groups

Get account groups.

**Response:**

```json
{
  "groups": [
    {
      "id": 1,
      "name": "Team",
      "description": "Paid team accounts",
      "color": "#2563eb",
      "sort_order": 0,
      "base_concurrency_override": 4,
      "member_count": 8,
      "created_at": "2026-05-13T00:00:00Z",
      "updated_at": "2026-05-13T00:00:00Z"
    }
  ]
}
```

#### POST /api/admin/account-groups

Create an account group.

**Request:**

```json
{
  "name": "Team",
  "description": "Paid team accounts",
  "color": "#2563eb",
  "sort_order": 0,
  "base_concurrency_override": 4
}
```

**Response:**

```json
{
  "id": 1,
  "message": "Group created"
}
```

#### PATCH /api/admin/account-groups/:id

Edit an account group.

**Request:**

```json
{
  "name": "Team Plus",
  "description": "High-priority accounts",
  "color": "#16a34a",
  "sort_order": 10,
  "base_concurrency_override": 2
}
```

`base_concurrency_override` has a minimum of `1` and no upper limit. On create, omitting it or passing `null` means no group override is set; on PATCH, passing `null` clears the existing value and restores inheritance. This value only sets the base concurrency; health tiers, usage protection and smart pacing can still lower the effective concurrency.

**Response:**

```json
{
  "message": "Group updated"
}
```

#### DELETE /api/admin/account-groups/:id

Delete an account group. If the group still has members, `?force=true` is required. After deletion, the ID is removed from account relations and, where possible, cleaned up from API keys' allowed groups. If an API key is bound only to this group, it is kept in a missing-group state so that its permissions are not accidentally widened.

```bash
curl -X DELETE "http://localhost:8080/api/admin/account-groups/1?force=true" \
  -H "X-Admin-Key: your-secret"
```

**Response:**

```json
{
  "message": "Group deleted"
}
```

### System settings

#### GET /api/admin/settings

Get system settings.

**Response:**

```json
{
  "max_concurrency": 2,
  "global_rpm": 0,
  "test_model": "gpt-5.5",
  "test_content": "hi",
  "test_concurrency": 50,
  "proxy_url": "",
  "pg_max_conns": 50,
  "redis_pool_size": 30,
  "auto_clean_unauthorized": false,
  "auto_clean_rate_limited": false,
  "auto_clean_full_usage": false,
  "auto_clean_error": false,
  "auto_activate_5h_window_enabled": false,
  "proxy_pool_enabled": false,
  "fast_scheduler_enabled": false,
  "max_retries": 2,
  "max_rate_limit_retries": 1,
  "retry_interval_ms": 0,
  "transport_retry_policy": "rotate",
  "continuous_retry_enabled": false,
  "continuous_retry_catch_all": false,
  "continuous_retry_categories": ["transport", "http_429", "http_5xx", "stream_error"],
  "continuous_retry_status_codes": [],
  "continuous_retry_error_codes": [],
  "continuous_retry_max_duration_seconds": 600,
  "codex_fingerprint_default_mode": "off",
  "scheduler_mode": "round_robin",
  "allow_remote_migration": false,
  "database_driver": "postgres",
  "database_label": "PostgreSQL",
  "cache_driver": "redis",
  "cache_label": "Redis",
  "response_cache_local_max_bytes": 67108864,
  "response_cache_local_max_entry_bytes": 8388608,
  "response_cache_reconstruct_max_bytes": 67108864,
  "response_cache_config_generation": 1,
  "admin_secret": "",
  "admin_auth_source": "env"
}
```

#### PUT /api/admin/settings

Update system settings.

**Request:**

```json
{
  "max_concurrency": 4,
  "global_rpm": 100,
  "test_model": "gpt-5.5",
  "test_content": "say pong",
  "test_concurrency": 50,
  "proxy_url": "http://proxy.example.com:8080",
  "auto_clean_unauthorized": true,
  "auto_clean_rate_limited": false,
  "fast_scheduler_enabled": true,
  "scheduler_mode": "remaining_quota",
  "max_rate_limit_retries": 2,
  "retry_interval_ms": 500,
  "transport_retry_policy": "sticky",
  "codex_fingerprint_default_mode": "session",
  "response_cache_local_max_bytes": 134217728,
  "response_cache_local_max_entry_bytes": 8388608,
  "response_cache_reconstruct_max_bytes": 134217728
}
```

**Response:** the full settings object after the update

`codex_fingerprint_default_mode` (`off` / `device` / `session` / `full`, default `off`) is the device fingerprint convergence level applied by default to newly imported or newly created Codex accounts. It only affects accounts added afterwards; existing accounts keep their level, and it can still be adjusted per account after import. Invalid values return HTTP 400.

By default, Codex maintenance requests share client identity resolution and the account's custom identity headers with conversation requests; there is no separate switch in the settings. The model list version stays consistent with the resolved client version. If identity resolution fails, an error is returned instead of falling back to a fixed identity. Maintenance requests set only the headers each endpoint requires, and do not add extra session identifiers or attestation. The browser protocol fallback for subscription sync stays independent. When compatibility mode lets official clients pass through, background requests use the configured identity and do not track or reuse the live identity of any downstream client.

`max_retries`, `max_rate_limit_retries` and `codex_ws_silent_max_retries` are the existing finite retry budgets; the admin UI and API both accept `0` to `10` (`0` disables the corresponding budget). For continuous retry, use the separate `continuous_retry_enabled` switch; it does not change the meaning of these finite budgets. When the switch is on, `continuous_retry_categories` can select `transport`, `http_429`, `http_4xx`, `http_5xx`, `stream_error`, `response_failed` and `context_error`, and `continuous_retry_status_codes` (for example `[403,404,501]`) or `continuous_retry_error_codes` (for example `["rate_limited","context_length_exceeded"]`) can be added for precise matching. A hit on any category, status code or error code is enough to enter continuous retry. 403, 404, context errors and "all `response.failed`" are not selected by default (501 is already covered by the default `http_5xx` category). Ordinary opt-in mode does not upgrade structured safety-policy refusals into infinite retries. Permanent quota or balance errors (such as `insufficient_quota`, `quota_exceeded`, `billing_hard_limit`, `billing_limit_reached`, `spend_limit`, `credit_balance`, `insufficient_balance`, `usage_limited`) do not enter an endless loop because of a generic category; they enter continuous retry only when the administrator explicitly selects the matching quota error code, or explicitly enables the catch-all switch described below.

`continuous_retry_catch_all` is a super switch that is off by default. When a request submits both `continuous_retry_enabled=true` and `continuous_retry_catch_all=true`, then, except for an explicit upstream `cyber_policy`, every HTTP status, transport/read failure, `error` frame, `response.failed`, and any unknown failure that appears later and that the proxy recognizes as a real upstream failure enters continuous retry, without relying on the current list of known error codes. Permanent quota, balance, authentication, invalid-request and other structured safety-policy errors are included as well. An explicit upstream `cyber_policy` always ends the current request, with no account switch and no replay. For text reasoning requests, only an upstream HTTP `200` and a normal protocol terminal state count as a committable result; 201, 202, 204, redirects, `response.failed`, standalone `error` frames and EOF without a terminal state all discard the entire attempt and retry. Stateless requests rotate through available accounts; a request that holds encrypted account-bound state switches accounts only when it can be safely expanded into a self-contained request, otherwise it keeps the binding semantics of its protocol. `continuous_retry_enabled=false` normalizes to `continuous_retry_catch_all=false`.

When continuous retry is enabled, streaming upstream attempts are first fully buffered, and only a normal terminal state is replayed to the client in one go. So partial text, tool calls and error frames from a failed attempt do not leak, but the client also no longer receives that result token by token in real time. On the SSE path, comment heartbeats are written and flushed while backing off, waiting for an account, waiting for response headers, and reading the buffered stream; Responses WebSocket uses Ping. Non-streaming JSON (excluding Codex/GPT Images), once in infinite retry, sends a standard HTTP `102 Processing` informational response while backing off, waiting for an account, waiting for response headers and reading the response body; it does not commit the final JSON status code or body, but intermediate proxies may drop 1xx responses, so wall-clock limits and client timeouts are still needed. Non-streaming generation and editing for Codex/GPT Images, even in infinite retry, only waits for the final JSON and sends no 102; its retry deadline and cancellation behavior stay unchanged. `retry_interval_ms` is the local minimum wait; the actual wait is the largest of this value, exponential backoff with jitter (starting at 250 ms, capped at 30 seconds), and the effective `Retry-After` (up to 5 minutes).

`continuous_retry_max_duration_seconds` (default 600, range 1-900) is the wall-clock cap on the infinite budget. It starts when the request first enters `retryLimit=-1`, and later attempts do not reset it. When it expires, the in-flight upstream request is cancelled and the most recent real upstream failure is returned: a committed SSE stream converts that failure into a protocol error event, and Responses WebSocket uses an error frame and closes with 1013. Only when no upstream failure is available to return does it fall back to 504 `upstream_timeout`. During infinite retry, non-streaming JSON is kept alive with HTTP `102 Processing`, and SSE comments are not mixed into the final JSON. This informational response is best-effort, and intermediate proxies may still filter it.

Each streaming attempt buffers at most 64 MiB: the first 8 MiB are kept in memory, and beyond that it spills to a mode-0600 temporary file that is unlinked immediately. Reaching the per-attempt limit or a local storage failure ends the request at once, without calling the upstream again. There is currently no process-wide buffering budget across requests, so high-concurrency deployments still need to limit concurrency themselves and monitor memory and temporary disk. For Responses HTTP, `X-Codex-Turn-State` can be forwarded only from the final successful account, and only if no response headers were committed by a heartbeat yet; if an SSE heartbeat was already sent during the wait, this header is omitted and cannot be replaced with the value from a failed account. Account-bound continuations keep their original account semantics when they cannot be safely expanded into a self-contained request.

Only real upstream failures can trigger continuous retry. Client cancellation, reaching the continuous-retry deadline, downstream write failures, entry validation, account pool or concurrency scheduling, local prompt or output policy rejections, buffering resource failures, and replay failures of a successful result all end the request immediately; they are not disguised as upstream errors that keep consuming accounts. The deadline guarantees that waiting API key and scope concurrency slots are eventually released.

Ordinary image requests are still capped at 5 total attempts. Once an error is selected by the continuous-retry policy (including super mode), it goes past the ordinary cap until success, client cancellation or the deadline. Because the upstream may not provide a reliable idempotency key, image creation may generate duplicates and be charged more than once. Any entry point that receives an explicit upstream `cyber_policy` stops immediately and keeps the local audit, signed decision, conversation lock and verified-user cooldown; super mode cannot override this safety terminal state. Local scheduling failures, such as an empty initial account pool, no eligible account for the model, or a scope/concurrency budget rejection, are not "upstream errors"; they still return an explicit local error.

Responses context cache fields use raw byte counts:

| Field | Type | Default | Valid range |
| --- | --- | --- | --- |
| `response_cache_local_max_bytes` | integer | 67,108,864 (64 MiB) | 8 MiB-4 GiB |
| `response_cache_local_max_entry_bytes` | integer | 8,388,608 (8 MiB) | 1-256 MiB, and cannot exceed the local total |
| `response_cache_reconstruct_max_bytes` | integer | 67,108,864 (64 MiB) | 8-512 MiB |
| `response_cache_config_generation` | integer | 1 | Read-only; any explicit write, including `null`, returns HTTP 400 |

A PUT can submit only some of the writable budgets; the server merges them with the current values inside a database transaction and validates the result. The admin panel takes integer MiB inputs and sends the three writable fields as one atomic update. After a successful change, the generation increments; this instance applies it immediately, and other instances sync every 5 seconds.

### Proxy pool management

#### GET /api/admin/proxies

Get the proxy list.

**Response:**

```json
{
  "proxies": [
    {
      "id": 1,
      "url": "http://proxy1.example.com:8080",
      "label": "US Proxy",
      "enabled": true,
      "created_at": "2024-01-01T12:00:00Z",
      "test_ip": "1.2.3.4",
      "test_location": "United States·California·Los Angeles",
      "test_latency_ms": 150,
      "test_status": "success"
    }
  ]
}
```

#### POST /api/admin/proxies

Add proxies (batch supported).

**Request:**

```json
{
  "urls": ["http://proxy1.example.com:8080", "http://proxy2.example.com:8080"],
  "label": "Batch Add"
}
```

Or a single proxy:

```json
{
  "url": "http://proxy.example.com:8080",
  "label": "US Proxy"
}
```

#### DELETE /api/admin/proxies/:id

Delete proxies and clear the account bindings that still reference the URL. The proxy is removed from the running process's in-memory proxy pool right after submission; if reloading the database snapshot fails, the endpoint returns HTTP `500` with the completed `deleted` / `unbound` counts, but it does not put the deleted proxy back into scheduling.

#### PATCH /api/admin/proxies/:id

Update a proxy. Disabling removes the URL from the runtime proxy pool immediately, but keeps the `proxy_url` bindings on accounts: these accounts do not switch to another proxy and do not connect directly until the proxy is re-enabled. When the URL is changed, account bindings that still point to the old URL are rewritten to the new URL.

**Request:**

```json
{
  "label": "New Label",
  "enabled": false
}
```

#### POST /api/admin/proxies/batch-delete

Batch delete proxies and unbind the accounts that still reference these URLs. When a reload fails, the semantics are the same as for a single delete.

**Request:**

```json
{
  "ids": [1, 2, 3]
}
```

#### POST /api/admin/proxies/test

Test proxy connectivity. When `id` is passed, `test_status` is persisted. Failure states attributable to the proxy (including proxy-side TCP connection failure/timeout, HTTPS/SOCKS negotiation failure/timeout, and HTTP `407` proxy authentication failure) are stored as `error`; after a successful retest it returns to `success`. When the caller cancels, when a transport error happens after the proxy connection was made, when the target site is unreachable or times out after a SOCKS proxy starts connecting to it, or when the probe service returns 429/5xx or an invalid response, `conclusive` in the response is `false`, and the original test status is kept unchanged.

**Request:**

```json
{
  "url": "http://proxy.example.com:8080",
  "id": 1,
  "lang": "zh-CN"
}
```

`id` is optional. If omitted, the request only tests and does not persist the result.

When `id` is passed, the server first reads the URL currently saved for that ID and starts the test only if it matches the request URL. When writing the result, it compares the ID and the original URL again; if the URL was changed during the test, it returns HTTP `409`, so an old result cannot overwrite a new configuration. Leading and trailing whitespace is removed only when dialing, which keeps non-normalized URLs from historical data working.

**Response:**

```json
{
  "success": true,
  "conclusive": true,
  "ip": "1.2.3.4",
  "country": "United States",
  "region": "California",
  "city": "Los Angeles",
  "isp": "Example ISP",
  "latency_ms": 150,
  "location": "United States·California·Los Angeles"
}
```

#### POST /api/admin/proxies/test-all

The server tests the specified proxies with at most 4 concurrent checks and returns progress item by item over SSE. The request passes only proxy IDs, and the server uses the current URL from the database. After all test results are saved, the runtime proxy pool is reloaded only once. The proxy pool refresh runs in the background finalization path and does not depend on the client continuing to read the SSE stream.

**Request:**

```json
{
  "ids": [1, 2, 3],
  "lang": "zh-CN"
}
```

`ids` is required, must be positive integers, and at most 100 per request. Empty arrays, unknown request fields and over-limit requests all return HTTP `400`. When there are more than 100 proxies, the admin panel automatically splits them into several sequential batches. A single service instance runs only one batch proxy test at a time; if a job is already running, it returns HTTP `409`. Example SSE `progress` event:

```text
data: {"type":"progress","proxy_id":1,"current":1,"total":3,"success":1,"result":{"success":true,"conclusive":true,"ip":"1.2.3.4","latency_ms":150}}
```

When the stream ends, a `complete` event is sent. If the database results were already saved but the runtime proxy pool refresh failed, the `error` field of the event describes the failure. If the client disconnects, unfinished probes are cancelled; results already written to the database still trigger one proxy pool refresh.

#### POST /api/admin/proxies/clean-error

Fixes the set of proxies with `test_status=error` at the start of this operation, deletes those proxies, and clears the account bindings that actually reference them. Proxies that become `error` during the cleanup are left for the next cleanup. After the commit, the URLs actually deleted are removed from the runtime proxy pool of the current process right away. If reloading the database snapshot fails, the endpoint returns HTTP `500` with the completed `cleaned` / `unbound` counts, but it does not put the deleted proxies back into scheduling.

**Response:**

```json
{
  "message": "Cleaned up 2 error proxies and unbound 3 accounts",
  "cleaned": 2,
  "unbound": 3
}
```

### Operations monitoring

#### GET /api/admin/ops/overview

Gets the system operations overview.

**Response:**

```json
{
  "updated_at": "2024-01-01T12:00:00Z",
  "uptime_seconds": 86400,
  "database_driver": "postgres",
  "database_label": "PostgreSQL",
  "cache_driver": "redis",
  "cache_label": "Redis",
  "cpu": {
    "percent": 25.5,
    "cores": 8
  },
  "memory": {
    "percent": 60.2,
    "used_bytes": 6442450944,
    "total_bytes": 10737418240,
    "process_bytes": 268435456,
    "heap_alloc_bytes": 100663296,
    "heap_inuse_bytes": 117440512,
    "heap_released_bytes": 33554432,
    "num_gc": 421
  },
  "response_cache": {
    "effective_config": {
      "generation": 3,
      "local_max_bytes": 67108864,
      "local_max_entry_bytes": 8388608,
      "reconstruct_max_bytes": 67108864
    },
    "applied_config": {
      "generation": 3,
      "local_max_bytes": 67108864,
      "local_max_entry_bytes": 8388608,
      "reconstruct_max_bytes": 67108864
    },
    "entries": 120,
    "max_entries": 2000,
    "current_bytes": 33554432,
    "max_bytes": 67108864,
    "high_water_bytes": 50331648,
    "largest_entry_bytes": 7340032,
    "local_hits": 920,
    "local_misses": 80,
    "remote_hits": 60,
    "remote_misses": 20,
    "expirations": 12,
    "count_evictions": 2,
    "byte_evictions": 8,
    "oversize_bypasses": 4,
    "oversize_rejections": 0,
    "known_unavailable_errors": 1,
    "last_config_sync_at": "2024-01-01T11:59:58Z",
    "last_config_sync_error": ""
  },
  "runtime": {
    "goroutines": 50,
    "available_accounts": 8,
    "total_accounts": 10
  },
  "requests": {
    "active": 5,
    "total": 10000
  },
  "postgres": {
    "healthy": true,
    "open": 10,
    "in_use": 5,
    "idle": 5,
    "max_open": 50,
    "wait_count": 0,
    "usage_percent": 20
  },
  "redis": {
    "healthy": true,
    "total_conns": 10,
    "idle_conns": 5,
    "stale_conns": 0,
    "pool_size": 30,
    "usage_percent": 33.3
  },
  "traffic": {
    "qps": 10.5,
    "qps_peak": 50.0,
    "tps": 500.0,
    "tps_peak": 2000.0,
    "rpm": 600,
    "tpm": 30000,
    "error_rate": 0.02,
    "today_requests": 5000,
    "today_tokens": 250000,
    "rpm_limit": 0
  }
}
```

`response_cache.current_bytes`, `max_bytes`, `high_water_bytes` and `largest_entry_bytes` are all logical bytes of the JSON payload, not RSS. `local_*` counts L1 lookups. `remote_*` counts effective hits and definite misses on the shared backend. `oversize_bypasses` means a shared-backend hit could be served but was not promoted to L1. `oversize_rejections` counts only writes that Memory mode could not keep within the byte budget. `known_unavailable_errors` counts only the final HTTP 409 responses for unavailable context.

`memory.process_bytes` uses the process RSS on Linux/Docker, and falls back to Go `Sys` on non-Linux systems where `/proc` cannot be read. `heap_alloc_bytes`, `heap_inuse_bytes`, `heap_released_bytes` and `num_gc` come from the same Go runtime sample. The logical cache budget does not include Go object or allocator overhead, so it must not be treated as a hard process memory limit.

During a rolling upgrade, responses from older backends may not include `response_cache` or the new heap/GC fields. The new admin frontend shows a compatibility waiting state or hides the missing heap rows.

### Model management

#### GET /api/admin/models

Gets the list of currently enabled models and returns model registry metadata.

**Response:**

```json
{
  "models": [
    "gpt-6-astra",
    "gpt-5.6-sol",
    "gpt-5.6-terra",
    "gpt-5.6-luna",
    "gpt-5.5",
    "gpt-5.3-codex-spark",
    "gpt-image-2"
  ],
  "items": [
    {
      "id": "gpt-5.3-codex-spark",
      "enabled": true,
      "category": "codex",
      "source": "official_codex_docs",
      "pro_only": true,
      "api_key_auth_available": true
    }
  ],
  "last_synced_at": "2026-04-24T00:00:00Z",
  "source_url": "https://developers.openai.com/codex/models"
}
```

#### POST /api/admin/models/sync

Syncs the model registry from the official OpenAI Codex models page. Sync only adds or updates model metadata and never deletes local models automatically. `gpt-image-2` is always kept as a built-in image model.

**Response:**

```json
{
  "added": 0,
  "updated": 2,
  "unchanged": 5,
  "skipped": ["gpt-5.2-codex"],
  "models": [
    "gpt-6-astra",
    "gpt-5.6-sol",
    "gpt-5.6-terra",
    "gpt-5.6-luna",
    "gpt-5.5",
    "gpt-5.3-codex-spark",
    "gpt-image-2"
  ],
  "last_synced_at": "2026-04-24T00:00:00Z",
  "source_url": "https://developers.openai.com/codex/models"
}
```

### Image generation workspace

API of the image generation workspace in the admin panel. It supports text-to-image (`/images/jobs`) and image-to-image (`/images/edit-jobs`) jobs, and gallery management.

#### POST /api/admin/images/jobs

Creates a text-to-image job.

**Request:**

```json
{
  "prompt": "A small orange cat sitting on a cloud",
  "model": "gpt-image-2",
  "size": "1024x1024",
  "quality": "high",
  "output_format": "png",
  "style": "photorealistic",
  "api_key_id": 0
}
```

**Parameters:**

| Parameter     | Type   | Required | Description                   |
| ------------- | ------ | ---- | ----------------------------- |
| prompt        | string | yes      | Prompt, up to 8000 characters |
| model         | string | no       | Model, default `gpt-image-2`  |
| size          | string | no       | Output size                   |
| quality       | string | no       | Quality level                 |
| output_format | string | no       | Output format, default `png`  |
| style         | string | no       | Style description             |
| upscale       | string | no       | Upscale option                |
| api_key_id    | int    | no       | Specify API Key ID            |

**Response:**

```json
{
  "id": 1,
  "status": "pending"
}
```

#### POST /api/admin/images/edit-jobs

Creates an image-to-image job. Its parameters are similar to text-to-image, but it requires an additional reference image.

**Request:**

```json
{
  "prompt": "Replace the background with a sunset scene",
  "model": "gpt-image-2",
  "size": "1024x1024",
  "output_format": "png",
  "input_images": [
    "https://example.com/source.png",
    "data:image/png;base64,iVBORw0KGgo..."
  ]
}
```

**Parameters:**

| Parameter     | Type     | Required | Description                          |
| ------------- | -------- | ---- | ------------------------------------ |
| prompt        | string   | yes      | Edit prompt, up to 8000 characters   |
| model         | string   | no       | Model, default `gpt-image-2`         |
| input_images  | string[] | yes      | List of reference image URLs or data URIs |
| size          | string   | no       | Output size                          |
| quality       | string   | no       | Quality level                        |
| output_format | string   | no       | Output format, default `png`         |
| api_key_id    | int      | no       | Specify API Key ID                   |

#### GET /api/admin/images/jobs

Lists image jobs. Supports pagination and status filtering.

**Response:**

```json
{
  "jobs": [
    {
      "id": 1,
      "prompt": "A small orange cat",
      "model": "gpt-image-2",
      "status": "completed",
      "created_at": "2024-01-01T12:00:00Z"
    }
  ],
  "total": 50
}
```

#### GET /api/admin/images/jobs/:id

Gets the details and result of a single image job.

#### DELETE /api/admin/images/jobs/:id

Deletes an image job and all gallery assets linked to it.

```bash
curl -X DELETE http://localhost:8080/api/admin/images/jobs/1 \
  -H "X-Admin-Key: your-secret"
```

#### GET /api/admin/images/assets

Lists gallery assets.

#### GET /api/admin/images/assets/:id/file

Gets a single gallery asset file (returns the image binary or a signed URL).

#### DELETE /api/admin/images/assets/:id

Deletes a single gallery asset.

### OAuth authorization

Gets a refresh token for a Codex account through the OAuth PKCE flow. Use it when the RT cannot be obtained manually.

**Flow:** generate the authorization URL → the user completes authorization in the browser → exchange the authorization code for a token and write it into the system

#### POST /api/admin/oauth/generate-auth-url

Generates an OAuth authorization URL (PKCE mode).

**Request:**

```json
{
  "proxy_url": "http://proxy.example.com:8080",
  "redirect_uri": "https://example.com/callback"
}
```

**Parameters:**

| Parameter    | Type   | Required | Description                          |
| ------------ | ------ | ---- | ---------------------------- |
| proxy_url    | string | no       | Proxy URL the account uses           |
| redirect_uri | string | no       | Callback address, default is the built-in system address |

**Response:**

```json
{
  "auth_url": "https://auth.openai.com/authorize?response_type=code&client_id=...&state=...",
  "session_id": "a1b2c3d4e5f6..."
}
```

> Open `auth_url` in the browser. After authorization, get the `code` and `state` parameters from the callback URL. `session_id` is valid for 30 minutes.

#### POST /api/admin/oauth/exchange-code

Exchanges the authorization code for a token, automatically creates a new account, and adds it to the account pool.

**Request:**

```json
{
  "session_id": "a1b2c3d4e5f6...",
  "code": "auth_code_from_callback",
  "state": "state_from_callback",
  "name": "my-oauth-account",
  "proxy_url": "http://proxy.example.com:8080"
}
```

**Parameters:**

| Parameter  | Type   | Required | Description                              |
| ---------- | ------ | ---- | ---------------------------------------- |
| session_id | string | yes      | session_id returned by `generate-auth-url`    |
| code       | string | yes      | The `code` parameter in the authorization callback URL |
| state      | string | yes      | The `state` parameter in the authorization callback URL |
| name       | string | no       | Account name; defaults to the email or `oauth-account` |
| proxy_url  | string | no       | Proxy URL; overrides the setting used when the URL was generated |

**Response:**

```json
{
  "message": "OAuth account user@example.com added successfully",
  "id": 42,
  "email": "user@example.com",
  "plan_type": "pro"
}
```

---

## Supported models

| Model                      | Description                                                                               |
| -------------------------- | ----------------------------------------------------------------------------------------- |
| gpt-6-astra                | Most powerful flagship model                                                              |
| gpt-5.6-sol / terra / luna | gpt-5.6 series (luna is the faster tier)                                                  |
| gpt-5.5                    | Flagship model. Billing: $5.00/M input / $30.00/M output (standard); priority is $12.50/M / $75.00/M respectively |
| gpt-5.3-codex-spark        | Codex Spark model, callable only by Pro subscription accounts                             |
| gpt-image-2                | GPT Image 2 image generation model                                                        |

> Note: The models actually supported are whatever the `/v1/models` endpoint returns. This document may not be updated in time.

---

## Error codes

### HTTP status codes

| Status code | Description              |
| ------ | ------------------------ |
| 200    | Request succeeded                 |
| 400    | Invalid request parameters        |
| 401    | Authentication failed             |
| 403    | Insufficient permissions          |
| 404    | Resource not found                |
| 409    | Resource conflict, or the context of the previous response is unavailable |
| 429    | Too many requests (rate limited)  |
| 499    | Client disconnected               |
| 500    | Internal server error             |
| 502    | Gateway error (upstream service abnormal) |
| 503    | Service unavailable (account pool exhausted, or the shared context backend it depends on is temporarily down) |
| 598    | Upstream stream interrupted       |

499 means the client cancelled or the connection closed early. The original request log and the recorded token usage are kept. The Requests (7D) failure count, retry failure count and error-code distribution in the account list all exclude 499, so client cancellations are not counted as account failures. The existing health rate, usage and billing summary rules are unchanged.

### Error response format

```json
{
  "error": {
    "message": "Error description",
    "type": "Error type",
    "code": "Error code"
  }
}
```

### Common error codes

| Code                             | Description      | Suggested action                 |
| -------------------------------- | ---------------- | -------------------------------- |
| missing_api_key                  | Missing API key  | Add the Authorization request header |
| invalid_api_key                  | Invalid API key  | Check that the key is correct    |
| authentication_error             | Authentication error | Check the Admin Secret or API key |
| invalid_request_error            | Invalid request parameters | Check the request body format  |
| server_error                     | Server error     | Check the logs to find the problem |
| upstream_error                   | Upstream service error | Check the Codex service status |
| no_available_account             | No schedulable account available | Retry later, enable an account, or add an available account |
| account_pool_concurrency_saturated | Concurrency window of the matching accounts is full | Retry later, or raise the per-account concurrency limit |
| account_pool_usage_limit_reached | Account pool quota exhausted | Wait for cooldown or add a new account |
| rate_limit_exceeded              | Rate limit triggered | Reduce the request rate          |
| previous_response_not_found      | The context required by `previous_response_id` is unavailable | Resend the full context or start a new response chain |
| service_unavailable              | The shared context backend it depends on is temporarily unavailable | Back off, retry, and check the Redis status |

### Responses context unavailable

HTTP 409 is returned in the following cases:

- In Memory mode, a known over-limit or evicted entry is hit, or a required context it depends on is simply missing or already expired.
- In Redis mode, a corrupted value is read, or the logical context exceeds the rebuild limit.

```json
{
  "error": {
    "code": "previous_response_not_found",
    "message": "Previous response not found; resend the full conversation input without previous_response_id",
    "type": "invalid_request_error",
    "details": {
      "field": "previous_response_id",
      "message": "local_context_evicted"
    }
  }
}
```

When a given `previous_response_id` is confirmed unavailable, do not retry unconditionally. Instead, resend the full required context, start a new response chain, or adjust the cache budget for later requests. If the failure is only a shared-backend transport fault, requests that depend on context return HTTP 503 with error code `service_unavailable`; back off, retry, and check Redis.

---

## Rate limiting

### Global RPM rate limit

Limits the global number of requests per minute through the `global_rpm` setting.

- `global_rpm = 0`: no rate limit
- `global_rpm > 0`: RPM rate limiting enabled

### API key window limits

`limits.rpm`, `limits.rpd`, and the cost/token windows are checked before each request is forwarded. There is also an atomic admission counter within the same process: RPM uses one-second buckets, and RPD uses a sliding 24-hour count. RPD admissions within the same minute are merged and expire 24 hours after the last admission in that bucket, so at most a bit under one extra minute is kept, conservatively, and quota is never released early. In multi-instance deployments, counts are kept separately per replica, and in-process records are lost on restart; persistent history checks still depend on usage logs. The admission table keeps at most 8,192 keys. When the table is full and no record can expire, new keys get `503`; active counts are never evicted to reset quota.

Batch image generation checks only the bounds when jobs are enqueued and does not reserve RPM/RPD; each actual image call takes one admission. `202` only means the job was accepted. It can still fail at execution time because of insufficient quota, so clients must check the job's final status.

For Responses (including direct `/v1/responses/compact`), when `input` exceeds 20,000 items, `400` is returned before parsing and rewriting. For Chat Completions, `messages` keeps its existing limit of 4,096 items, plus a guard before parsing. For either field, duplicate top-level keys, case conflicts or non-canonical spellings (such as `Messages`) all return `400`, so the up-front checks and the later JSON decoding cannot pick different fields.

For the admin, portal and external job entry points of Image Studio, an explicit `WIDTHxHEIGHT` is limited to 16,384 pixels per side and 40 million total pixels. Invalid or over-limit targets return `400` before enqueueing, external link downloads and upstream calls, and exact-size scaling is never silently cancelled.

The admin side records authentication failures per client IP: after 5 failures within 10 minutes, the IP is blocked for 10 minutes and gets `429` with `Retry-After`. The block runs before the key comparison, so during the block even the correct key gets `429`. Admins behind a shared egress must wait for the block to expire. Only an explicitly configured trusted reverse proxy may supply the trusted client address; never treat forwarding headers sent by a client as identity.

The authentication failure table records at most 4,096 sources and keeps valid failure counts and blocks. When the table is full and has no expired entries, a new untracked source receives `429` before the key comparison, and `Retry-After` points to the earliest entry that can be released. Tracked sources keep their own counts and blocks, and a successful authentication can still clear that source's record.

OAuth sessions are claimed once before exchange, and the public callback, manual exchange and account update share this state. Concurrent or repeated exchanges are rejected. After a failure, authorization must be started again, and the code that may already have been consumed upstream is not reused. While callback processing is running, polling returns `processing`; once it finishes, polling returns `completed` and the session is cleared. The callback page and polling results show only generic errors, and redacted diagnostics stay in the server logs.

For self-service account submissions, the credentials, disabled state, contact notes, the `self-service` tag and the credential family are saved in the same database transaction. If saving fails, no half-finished account is left behind and the account identity is not claimed. Authorization can be started again and the submission retried. Accounts pending review do not enter the scheduling pool until an admin approves them.

Upstream HTTP error handling for Responses, Chat, compact, Images and downstream Responses WS reads at most a 1 MiB prefix. A successful JSON aggregated directly by Responses/compact is read up to 64 MiB; a successful JSON over the limit fails with an explicit error and is never returned as truncated JSON. The per-line limit on the new SSE read path is 8 MiB, and over-limit errors are propagated to the caller. These are local resource boundaries. They do not mean that a larger Base64 image or tool output is necessarily invalid under the upstream protocol, and they do not mean that other HTTP clients such as OAuth reuse this budget.

### API key model weekly request budget

The API key's `limits.model_request_limits` can cap the number of requests per fixed calendar week by final mapped model. Exact model names and the `*` wildcard are supported. The matching models of one rule share one budget, and when several rules match, all of them apply. For the configuration fields, counting rules and update rules, see the [Configuration guide](CONFIGURATION.md#api-key-model-weekly-request-budget).

Both `POST /api/admin/keys` (create) and `PATCH /api/admin/keys/:id` (update) in the admin API accept this field. New rules omit `id`, and the server generates it. Read the `limits` returned by `GET /api/admin/keys` to get the IDs of saved rules. When updating an existing rule, keep its ID; only the request cap or its order can be changed. To change the model or the reset schedule, delete the old rule and add a new one. Invalid configuration or an unknown rule ID returns `400`.

Admins can query current-week usage with admin authentication:

```http
GET /api/admin/keys/123/model-request-usage
X-Admin-Key: YOUR_ADMIN_SECRET
```

```json
{
  "model_request_usage": [
    {
      "rule_id": "mr_example",
      "model": "gpt-6*",
      "window": "week",
      "limit": 50,
      "used": 12,
      "remaining": 38,
      "window_start": "2026-08-30T16:00:00Z",
      "reset_at": "2026-09-06T16:00:00Z",
      "timezone": "Asia/Shanghai"
    }
  ]
}
```

The public self-service endpoint `GET /api/key-usage/summary`, and its alias `GET /api/key-usage/me`, add a top-level `model_request_usage` with the same structure, in addition to the existing `key`, `range` and `usage`. Pass `Authorization: Bearer YOUR_API_KEY`. It returns only the budget of this key, and other keys cannot be read through query parameters. When the public usage page is turned off, it keeps returning `404`. When nothing is configured, this field is `[]`. This field always reflects the current fixed week and is independent of the report's `range` parameter.

The request logs of this endpoint (`usage.recent_logs`) support the following optional filter parameters. They affect only the log list and `recent_logs_total`, not `summary`, `windows`, or the model/endpoint rankings:

- `model`: matches both the requested model and the actual effective model;
- `endpoint`: matches the inbound endpoint (same basis as `endpoint` in the logs);
- `status`: `success` (< 400), `error` (≥ 400), `4xx`, `5xx`, `429`;
- `stream`: `stream` or `sync`;
- `channel`: upstream channel, currently `codex`.

Invalid `status` / `stream` / `channel` values, or `model` / `endpoint` values longer than 128 characters, return `400`. The `usage.log_models` and `usage.log_endpoints` in the response list the request models and inbound endpoints that appeared in the current `range`, sorted by request count in descending order, up to 50 each, and unaffected by filters. They can be used to build filter dropdowns. Each log also carries an extra `channel` field (`codex` or an empty string).

When the budget is used up, HTTP returns `429` with error code `rate_limit_reached`, and `Retry-After` gives the number of seconds until that rule resets. `error.details` contains a usage snapshot of the exhausted rule:

```json
{
  "error": {
    "type": "rate_limit_error",
    "code": "rate_limit_reached",
    "message": "API key weekly model request limit reached for \"gpt-6*\" (50/50)",
    "details": {
      "rule_id": "mr_example",
      "model": "gpt-6*",
      "window": "week",
      "limit": 50,
      "used": 50,
      "remaining": 0,
      "window_start": "2026-08-30T16:00:00Z",
      "reset_at": "2026-09-06T16:00:00Z",
      "timezone": "Asia/Shanghai"
    }
  }
}
```

After the Responses WebSocket upgrade, rejections are returned as the matching error frame, and each `response.create` is counted separately. HTTP streaming requests are checked before being sent upstream. If an earlier upstream attempt has already started an HTTP event stream, and a later retry maps to another model and uses up its budget, an error event containing the same `error.details` is sent on the existing stream. This local budget error does not trigger an upstream account-switch retry. When the counting dependency is temporarily unavailable, `503` is returned rather than silently allowing the request. After the quota is used up, wait for the rule to reset, or call a model that does not match this rule and meets its other limits.

### Account-level rate limiting

The system automatically rate limits based on account state:

- **Healthy**: normal concurrency
- **Warm**: concurrency halved
- **Risky**: fixed at 1 concurrent request
- **Banned**: 0 concurrency, excluded from scheduling

### Response when no account is schedulable

When no account in the account pool can be scheduled, the endpoint returns `503 Service Unavailable`:

```json
{
  "error": {
    "message": "No accounts available, please retry later",
    "type": "server_error",
    "code": "no_available_account"
  }
}
```

When the matching accounts all exist but their concurrency slots are already full, the endpoint still returns `503`, but the error code changes to `account_pool_concurrency_saturated`, with `Retry-After: 1`. This is not the same failure as "no schedulable account in the pool": raise the concurrency limit, or retry after in-flight requests release their slots.

```json
{
  "error": {
    "message": "Account concurrency window is full, please retry later or raise the concurrency limit",
    "type": "server_error",
    "code": "account_pool_concurrency_saturated"
  }
}
```

### Response when the account pool quota is exhausted

When upstream returns a `429` indicating that account quota is exhausted, the system rewrites it externally to `503 Service Unavailable` and keeps the `Retry-After` header:

```http
HTTP/1.1 503 Service Unavailable
Retry-After: 3600

{
  "error": {
    "message": "Account pool quota is exhausted, please retry later",
    "type": "server_error",
    "code": "account_pool_usage_limit_reached",
    "plan_type": "free",
    "resets_at": 1712345678,
    "resets_in_seconds": 3600
  }
}
```

### Recommendations

1. Monitor the `X-RateLimit-*` response headers (if present)
2. Implement an exponential backoff retry strategy
3. Handle 429/503 status codes, and wait for the `Retry-After` interval before retrying
4. Avoid sending large numbers of requests in a short period


### Optional persistent image job queue

Setting `IMAGE_JOB_WORKERS=2` enables a database job queue with fixed concurrency. Once enabled, the create endpoint returns only `job.id` and `job.status`, and reference images are read when the job runs. For database incremental migrations, memory limits, restart recovery and compatibility, see [Image job queue](IMAGE_JOB_QUEUE.md).

### Adaptive intelligence-degradation check configuration

`PUT /api/admin/intelligence-check/settings` supports `adaptive_enabled`, `pause_initial_seconds`, `pause_max_seconds`, `recovery_initial_seconds` and `recovery_min_seconds`. You must still submit the current `revision` and the full settings. For field semantics, bounds and compatibility rules, see [Adaptive intelligence-degradation check](INTELLIGENCE_BACKOFF.md). The check bars add `probe_phase`, `next_interval_seconds`, `passed_streak` and `next_due_at`, which show backoff or recovery-confirmation progress.
