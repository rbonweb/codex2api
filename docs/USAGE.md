# Usage guide

The admin panel's `/admin/docs` provides client configuration, search, parameter reference, and copyable examples. Content is grouped into quick start, development guide, API reference, troubleshooting, and administration and advanced topics; section URLs can be bookmarked directly. The page's "Copy as Markdown" includes the same guide and API data, with keys replaced by placeholders; the copy button in the client configuration area uses the currently selected real key.

## Complete your first integration

1. Enable at least one account that has the capabilities of the target model.
2. Create a client-specific key under API keys, and check the channel, model, and group scope.
3. In the quick start section of the docs, choose Codex CLI, CC Switch, or Cherry Studio, and set the service address and model. Reasoning effort and service tier are under Advanced options.
4. Write the config file or import into the client as prompted, then run the cURL command the page provides.
5. In usage statistics, check the request status, final model, duration, and token count.

A successful `GET /health` only means the service is online. `GET /v1/models` verifies the address and authentication; only a complete generation request verifies the model, account, and upstream path.

## Addresses and keys

Assume the service address is `https://gateway.example.com`:

| Purpose | Address or credential |
| --- | --- |
| Codex CLI | This gateway is compatible with the Responses path without `/v1` under `https://gateway.example.com` |
| OpenAI SDK | `base_url` / `baseURL` is `https://gateway.example.com/v1` |
| Regular API | `Authorization: Bearer YOUR_API_KEY` |
| Admin API | `X-Admin-Key: YOUR_ADMIN_SECRET`, no downstream API key is needed on top |
| Outbound proxy | Account `proxy_url`, used by the gateway to reach upstream; configured separately from the client Base URL |

The public API requires authentication by default. Regular public endpoints allow anonymous access only when no API key is configured and `CODEX_ALLOW_ANONYMOUS=true` is explicitly enabled; async image jobs always require an API key created in the admin panel.

## Development guide

- `/admin/docs#sdk-examples`: minimal requests for the Python and Node.js SDKs. The SDK key is read from a server-side environment variable.
- `/admin/docs#streaming`: `stream=false` returns JSON; `stream=true` returns SSE. cURL uses `-N`. The client checks for the terminal event and must not treat a dropped connection as complete.
- `/admin/docs#conversation-tools`: keep the original input, all response.output items, and tool results; use `call_id` to pair tool calls with their outputs.
- `/admin/docs#structured-output`: Responses uses `text.format`, Chat uses `response_format`; supported constraints depend on the final model, channel, and transport.
- `/admin/docs#protocols`: protocols, model mapping, reasoning effort, and service tier.

`previous_response_id` depends on context that can be recovered within the same key scope. The cache is limited by capacity, TTL, and write policy. When the Memory cache is paired with a SQLite file primary database, it has a disk persistence layer by default; without that layer, its contents are lost on restart after a shutdown. On `409 previous_response_not_found`, resend the full required history and start a new chain. See [Responses context](API.md#responses-context-unavailable) for details.

The basic calls and streaming events in the SDK examples follow the [OpenAI SDK docs](https://developers.openai.com/api/docs/libraries) and the [streaming response docs](https://developers.openai.com/api/docs/guides/streaming-responses); model names, routes, and compatibility boundaries are defined by this gateway.

## Image and advanced endpoints

| Capability | Entry point | How to use |
| --- | --- | --- |
| Sync image generation / edit | `/v1/images/generations`, `/v1/images/edits` | Returns images; edit accepts JSON and multipart; masks are supported only by the gpt-image series |
| Async image generation / edit | `POST /v1/images/jobs`, `GET /v1/images/jobs/:id` | Returns `202 {job: ...}`; query `job.id` with the same API key; after success, download the image from `job.assets[].proxy_url` |
| Local token estimate | `/v1/responses/input_tokens` | Does not call upstream; used for compatibility probing, not as an accurate tokenizer or a billing basis |
| Context compaction | `POST /v1/responses/compact` | Submit the full history; keep the returned compaction items and opaque fields |
| Responses WebSocket | `GET /v1/responses` | After connecting with the auth header, send `response.create`; the same `stream_id` keeps order, and different independent streams run in parallel (up to 16 active turns and 32 named streams); `response.interrupt` can be sent to the current WebSocket upstream lease; `response.append` and `response.steer` return `unsupported_feature`, so use `response.create` with `previous_response_id` on the next turn instead |

Flare and Sunburst in GPT Image 2.5 support `auto/low/medium/high/xhigh/max`; omitting the model still defaults to `gpt-image-2`. `-2k/-4k` are gateway size tier aliases. When generating images directly through Responses, the top-level `model` drives the text, and `tools[].model` is the image model. For full parameters, see [Images](API.md#3-images).

## Errors and quota

| Symptom | Next step |
| --- | --- |
| 401 / 403 | Check credentials, validity period, key channel, model, and group authorization |
| 429 `rate_limit_reached` | Check `error.details` and `Retry-After` for the model's weekly budget |
| 503 `no_available_account` | Check account enabled state, cooldown, concurrency, and available models |
| 503 `account_pool_concurrency_saturated` | The account exists, but its concurrency window is full; retry later or raise the concurrency limit |
| 503 `account_pool_usage_limit_reached` | The account pool's quota is used up; wait for the reset indicated by `Retry-After`, or add accounts |
| 409 `previous_response_not_found` | Resend the full context and start a new response chain |
| 503 `service_unavailable` | Check the shared context or budget store, back off, and retry |
| 404 | Check for a duplicated `/v1`, the server version, and the full path; job queries must also check the owning key and its validity period |

Monetary quota, the weekly model request count, and upstream account quota are independent limits. The weekly count matches the final model and can specify a time zone and reset time. See [model weekly budget](CONFIGURATION.md#api-key-model-weekly-request-budget) for details.

For more, see the [API reference](API.md), [configuration](CONFIGURATION.md), [deployment docs](DEPLOYMENT.md), and [troubleshooting](TROUBLESHOOTING.md).
