# Responses compatibility and acceptance boundaries

Codex2API's Responses entry serves Codex clients, the OpenAI SDK and different upstreams at the same time. The degree of compatibility depends on the account actually selected, the protocol and the upstream capability; a model name, model catalog or HTTP 200 cannot prove that all official features are available.

## Request fields and strict mode

Responses requests keep the native `reasoning.effort`, `text.format`, JSON Schema constraints and large integers; native JSON mode does not automatically rewrite the user's prompt. Compatibility aliases such as `reasoning_effort` and `response_format` still have explicit conversion; native fields take precedence. HTTP outbound removes the top-level `type` and `stream_id`, which belong only to the WebSocket envelope. The gateway uses a named stream identifier internally for routing, and WS frames finally sent upstream do not carry the gateway `stream_id` either; same-named fields in nested input or schema are kept. The built-in `tool_search` is not automatically given a `description` that its upstream protocol does not accept; explicit user fields are validated by the upstream.

The ChatGPT/Codex upstream needs extra normalization, and some Responses fields or schema constraints cannot be kept as they are. Clients can send:

```http
X-Codex2API-Strict-Compatibility: true
```

Alternatively, set the server environment variable `CODEX_STRICT_RESPONSES_COMPAT=true`. Strict mode checks against the **actually selected Codex account**. Default mode continues to use the original compatibility conversion.

Strict mode returns `400 unsupported_feature` while the HTTP response has not yet been committed; if the retry keepalive has already committed the SSE, it sends a `response.failed` event; WebSocket returns an error frame with `status:400`. `error.details.fields` lists the lossy fields, including output length, sampling parameters, metadata, context management, storage and cache control. The following semantic checks also apply:

- No-op values such as `background:false/null` and `conversation:null` are allowed; `background:true`, a non-empty `conversation` and `store:true` are explicitly rejected. A string `background` is still the original image background extension.
- Reasoning and format aliases pass only when the known conversion can be completed; an unknown effort or one that would be downgraded, and a discarded service tier, are rejected.
- Tool or structured-output schemas that Codex sanitization would change are rejected; more than 128 tools, tool declarations that would be deleted, and search configurations that would be rewritten also produce explicit errors. Tools in `additional_tools` are checked the same way, and the reserved tool namespace follows the existing pass-through-as-is rule. The check is limited by the cumulative tool count and error budgets; schema numbers that the native Codex path would lose precision are also rejected.

This is a gate for known lossy conversions, **not a full schema validator or a proof that every field is lossless**. Account mapping, administrator Payload Rules, upstream model limits and unknown nested extensions can still affect the final behavior. Turning on strict mode may cause requests that were previously repaired automatically to return errors; correct the requests according to the field list, or switch to an upstream that supports that semantic.

## Recovery requirements for HTTP continuation

When processing raw JSON, HTTP Responses/Compact rejects duplicate top-level `input` or `previous_response_id` with `400 invalid_request`, so that different parsers do not pick different values; a same-named key after JSON escaping also counts as a duplicate. Numeric values in nested fields and parent references are not rewritten by this check.

Native Codex HTTP outbound must expand and delete `previous_response_id`. Every non-empty parent response reference must be proven in the cache owned by the current request; a missing or expired cache returns 409, and a backend failure returns 503. It never drops the reference and goes on to return a successful response with no history. A new tool call still needs the parent snapshot concatenated, and this does not allow inferring that the client resent all ancestors. The duplicate concatenation is skipped only when the overlapping `call_id` and the complete parent prefix match; the comparison ignores only the defined top-level item ID and internal metadata. Partial replay, conflicting call content or a duplicated parent call definition returns `409 previous_response_not_found`; the client can resend the full history and omit the parent ID. Identical message text by itself does not trigger deduplication.

The current HTTP cache mainly stores tool context and does not add session storage for ordinary messages. Follow-up HTTP requests for an ordinary text parent turn should send the full history and omit `previous_response_id`; when incremental continuation is needed, use the verified WS path.

## WebSocket streams and continuation

`GET /v1/responses` supports `response.create`:

- The default stream and the same `stream_id` are FIFO; different streams can run in parallel, and a slow independent stream does not block other independent streams.
- Each connection allows at most 16 active turns and 32 distinct named streams; the default stream does not use up the named-stream quota. `stream_id` is 1–256 bytes of ASCII letters, digits, `_`, `-` or `.`.
- Response events and request errors for a named stream carry back the matching `stream_id`. A request-level failure or preemption on a named stream does not close other healthy streams; the default stream keeps its original protocol-error close behavior. Authorization failure, a dropped connection or exhausted connection resources can still end the whole connection.
- Per connection: at most 16 pending requests and 64 pending requests in total, still subject to the read queue, byte budget, global memory, and account/key concurrency limits. A disconnect cancels active turns and releases the queue and leases.
- Each turn uses its own audit, quota and request state, and the upstream connection pool is isolated per stream. The gateway may use different upstream connections to run in parallel, and it does not promise that all streams reuse one upstream connection; continuations pointing to the same parent response may still wait for the original upstream connection lease.

`stream_id` decides the ordering, and `previous_response_id` decides the history source. For the `store:false` native WS cache, the same client connection can fork from a response **already committed** on another stream, and the new response is written to its own stream cache. The per-connection index keeps at most 256 response identifiers, and a match still requires the full context and must satisfy account/key, routing group, credential and cache limits. Another client connection cannot use this index to read cross-stream history it has not observed; after index eviction, cache expiry or a restart, the client should provide the full history. For the recovery rules for Memory and Redis, see [Responses context](API.md#responses-context-unavailable).

`response.append` and the official GPT-6 `response.steer` currently return a WebSocket error frame with `status:400` and error code `unsupported_feature`. This change does not implement redirecting during generation; sending a new `response.create` after the current turn terminates is a separate turn. Real steering would also need to cover accepted/pending/failed, the original response being incomplete, automatic successor responses, tool results, and quota and commit state; it cannot claim success by merely forwarding one control frame.

Codex's interrupt control event is different from `response.steer`:

```json
{"type":"response.interrupt","response_id":"resp_current","mode":"discard_partial_items"}
```

An interrupt bypasses the generation queue and locates the upstream lease by the `response_id` on the current downstream connection; the optional `stream_id` further limits the target stream, and this gateway routing field is stripped when sent upstream. A Codex account sends the control frame to the current connection, without reconnecting, replaying or opening a new generation request. Upstream `response.interrupt.accepted` / `response.interrupt.failed`, `response.output_item.interrupted` and `response.incomplete` are kept as they are, and named streams still carry back `stream_id`. A completion frame may arrive before the interrupt confirmation; the gateway keeps the original order of events and `sequence_number`, and returns the lease only after the terminal state and the matching confirmation have both been consumed.

A late interrupt for a finished turn is acknowledged locally, and a repeated interrupt is not forwarded again; the connection remembers at most the latest 256 finished turns, and an old ID cannot cancel the next turn or a request on another connection. The interrupt confirmation wait is capped at 30 seconds; on timeout the corresponding upstream connection is destroyed, so a leftover confirmation does not enter a later lease. An unknown target returns `response.interrupt.failed` with the same `response_id`; an upstream without a WebSocket control channel returns `interrupt_not_supported`. A missing or wrongly typed `response_id`, an unsupported `mode`, invalid JSON, a non-object payload, and duplicated routing/control fields return request errors.

The `interrupted`, `steered`, `content_filter` and `max_messages` reasons of `response.incomplete` keep their original terminal state even when the output is zero, and do not enter empty-reply repair or automatic retry. Completed output and usage continue to follow the original continuation and billing path; whether unfinished output is discarded is decided by the upstream `discard_partial_items` semantics.

## Capability catalog and tool boundaries

Account probing stores a bounded array of strings in `experimental_supported_tools`, and the modality accepts only `text/image/audio`. The Codex `models/slug` model list takes the intersection of capabilities across accounts that match the current key scope, channel, and the catalog and model filter conditions, and does not guarantee that an account has a free concurrency slot at this moment. The ordinary OpenAI `data/id` model list does not add capability fields for this reason. An unknown snapshot, a credential generation change, or unreadable capability information does not inherit advanced declarations, and an old snapshot cannot restore capabilities that were revoked in the upstream list obtained this time. The upstream list and the local fallback both apply this rule; the ETag corresponds to the final catalog, and a client 304 cannot bypass capability narrowing.

| Feature | Gateway contract | Acceptance limits |
| --- | --- | --- |
| function / custom tools | Keeps call and result pairing, and restores required history when continuing | Protocol verified locally with simulation; does not guarantee the model always generates according to the schema |
| strict / parallel tools | Keeps passable fields; strict mode rejects known lossy schemas; different WS streams can run tool turns independently | A parallel tool declaration in the catalog does not mean steering is supported |
| compact / previous_response_id | Kept or converted according to the actual upstream; by default, an explicit error is raised when full snapshot recovery fails; explicitly enabling `CODEX_WS_CONTINUATION_FAIL_OPEN=true` turns on lossy degradation | Does not fake the official Conversations or storage query lifecycle |
| hosted tools / MCP / files / images / audio | Supported native requests are handed to the matching upstream for execution and validation | Authorization, resource ownership, model and protocol limits are still decided by the upstream; official hosted capabilities are never synthesized from local tool declarations |
| background / conversation | Validated by the upstream; Codex strict mode rejects unsupported semantics | No complete query, cancel, Conversations or background-task endpoints have been added |

## Local verification and live acceptance

Automated regression uses fake accounts and a loopback upstream, and checks the fields, events, tool returns, parallelism and FIFO, cross-stream history, release on disconnect, cache isolation and error handling after HTTP/WS dispatch. Shared-cache tests cover the backend interface contract and are not equivalent to integration acceptance against a standalone Redis service. These automated regressions do not access real accounts or incur model call costs; real-account acceptance is run separately, and records the models and tools actually used.

Live acceptance should still run a differential between a direct official SDK connection and the gateway against the same upstream that supports this feature, comparing event sequences, terminal states, tool pairing, constraints, resource ownership and errors; re-verify after a model upgrade, credential change, or cache/protocol switch. Local automated regression does not mean the actual deployment or all upstream capabilities have been verified.

Official contract references: [WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode), [Steering](https://developers.openai.com/api/docs/guides/steering), [Codex model description structure](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/openai_models.rs).

The interrupt protocol was checked against the Codex source: [send and acknowledgement](https://github.com/openai/codex/blob/970111f6cb62cc02fe36a82ea7d71b37a2889c97/codex-rs/codex-api/src/endpoint/responses_websocket.rs), [interrupt terminal state and usage](https://github.com/openai/codex/blob/970111f6cb62cc02fe36a82ea7d71b37a2889c97/codex-rs/codex-api/src/sse/responses.rs). The public [Responses event definitions](https://developers.openai.com/api/reference/resources/responses/streaming-events) and the Steering documentation cannot replace the Codex `response.interrupt` contract.
