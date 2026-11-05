# Codex Multi-Agent v2 third-party sub-agent compatibility

Last updated: 2026-10-18

This document describes the optional plaintext compatibility layer for Codex Multi-Agent v2 in Codex2API. It addresses this case: when a GPT parent agent hands a task to a third-party sub-model (for example DeepSeek) under v2, the sub-agent can be created and can reply to messages, but it repeatedly says "did not receive a specific task".

## Conclusion and boundaries

This is an experimental compatibility layer that is off by default and must be explicitly enabled in the deployment environment.

When enabled, matching v2 collaboration calls switch to plaintext parameters. The gateway cannot tell whether a sub-agent is native OpenAI or a third-party model: as long as the parent request matches, the task messages from parent to child are generated and transmitted in plaintext. When native OpenAI sub-agent encryption must be kept, do not enable it, or enable it only on isolated instances and account pools.

The compatibility layer does not decrypt ciphertext that was already generated, and does not modify the Codex binary. What it changes is the schema and parameter generation of subsequent requests.

It does not change authentication, account selection, model routing, billing, rate limiting or User-Agent, and does not affect v1, ordinary Responses, Chat Completions, images, reasoning, compaction, other tools or non-Codex clients.

The User-Agent check is a compatibility heuristic, not a security boundary. A caller holding an API key can forge the official UA; the rewrite only applies to that caller's own requests, but deployers should not treat "official clients only" as strong access control.

## Risks and applicable accounts

- Upstream sees a non-standard `collaboration-optimize` schema. Upstream may reject it as a reserved/unknown schema, or may log it or trigger account risk control. The compatibility layer has no automatic fallback: when upstream rejects, the request returns the upstream error.
- The body of a sub-task travels in plaintext to the third-party model and its logs, caches or forwarding path. Do not hand sensitive tasks to untrusted third parties.
- The switch is a process-level environment variable and cannot be excluded per account or per model. When multiple instances handle the same traffic, all instances must stay consistent.
- Roll out first on a non-primary account or an isolated instance, verify spawn, followup, send_message and real tool calls, watch upstream 4xx responses and account status, and only then widen the scope.
- If account risk control is not acceptable, the safe alternative is to wait for upstream to offer an official plaintext configuration, or to let the sub-agent use v1. Changing the schema at the gateway always changes the request that upstream sees.

## Why this layer is needed

In Codex Multi-Agent v2, `spawn_agent`, `send_message` and `followup_task` mark the `message` parameter as encrypted. The official OpenAI upstream generates ciphertext when it receives this marker, and the Codex client stores it as `encrypted_content` of `agent_message`, leaving only the `Payload:` header in plaintext. The third-party sub-model cannot read the body, so it replies "no task".

The Codex client takes the `DirectPlaintextMessage` path only when all of the following conditions are met:

1. `namespace` is exactly `collaboration`;
2. The tool name is `spawn_agent`, `send_message` or `followup_task`;
3. `encrypted_function_args` is an explicit empty array `[]`. Missing or `null` is not equivalent.

Encryption happens while the parent model generates the arguments, and the gateway cannot decrypt it afterwards. The compatibility point must be in the parent request's tool schema, that is, before the parent model generates the arguments.

Upstream requires the reserved `collaboration.*` tools to match the schema verbatim; directly editing or deleting `message.encrypted` is rejected by upstream as a reserved schema. The compatibility layer therefore uses a temporary namespace `collaboration-optimize` and restores it before the response returns to the client.

Source basis:

- `codex-rs/core/src/tools/router.rs`: the decision conditions of `ToolCall::direct_source()`.
- `codex-rs/core/src/tools/handlers/multi_agents_v2.rs`: a non-plaintext source becomes `AgentMessage::Encrypted`.
- `codex-rs/core/src/tools/handlers/multi_agents_spec.rs`: the `message` of the three collaboration tools carries `.with_encrypted()`.

## Relationship to the similar CLIProxyAPI implementation

CLIProxyAPI already implements the same capability (MIT), and this approach borrows its core idea:

- Source: [optimize_multi_agent_v2.go](https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/internal/client/codex/optimize-multi-agent-v2/optimize_multi_agent_v2.go)
  - [`OptimizeCodexMultiAgentV2Request`](https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/internal/client/codex/optimize-multi-agent-v2/optimize_multi_agent_v2.go#L138-L155): removes the encryption marker and temporarily renames the namespace.
  - [`PrepareCodexMultiAgentV2Tools`](https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/internal/client/codex/optimize-multi-agent-v2/optimize_multi_agent_v2.go#L112-L136): prepares the collaboration tools at the Responses boundary.
  - [`optimizeCodexCollaborationNamespace`](https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/internal/client/codex/optimize-multi-agent-v2/optimize_multi_agent_v2.go#L695-L716): changes `collaboration` to `collaboration-optimize`.
  - [`RestoreCodexMultiAgentV2Response`](https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/internal/client/codex/optimize-multi-agent-v2/optimize_multi_agent_v2.go#L718-L739): restores the namespace in the response.
  - [`rewriteCodexAgentMessageContent`](https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/internal/client/codex/optimize-multi-agent-v2/optimize_multi_agent_v2.go#L834-L874): converts `agent_message` for the sub-agent side.
- Config: [`client.codex.optimize-multi-agent-v2`](https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/config.example.yaml#L327), default `false`, applies to both OAuth and API-key credentials.
- License: [MIT License](https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/LICENSE).

This implementation keeps the core transformation. The differences: it takes effect only in the native Codex executor; on conflicts, invalid JSON, or nesting or count overflow it fails closed; it does not relabel `encrypted_content` as `input_text`; and it handles only the collaboration tool family, scoped by namespace.

Related issues:

- [CLIProxyAPI #4746](https://github.com/router-for-me/CLIProxyAPI/issues/4746): `followup_task` still stays encrypted.
- [CLIProxyAPI #4801](https://github.com/router-for-me/CLIProxyAPI/issues/4801): third-party Responses providers do not support `agent_message`.
- [CLIProxyAPI #6233](https://github.com/router-for-me/CLIProxyAPI/issues/6233): handling of `author` / `recipient` in compat mode.

## Enablement and gradual rollout

```bash
CODEX_MULTI_AGENT_V2_COMPAT=true
```

Default `false`; accepts `1`, `true`, `yes` and `on`. The variable is read per process, and a restart is needed after changing it.

Rollout steps:

1. Pick one isolated instance or a separate account pool, and do not let the load balancer randomly send the same client to instances with the switch on and off.
2. After enabling, have one real Codex session run `spawn_agent`, `followup_task`, `send_message` and one read-only tool call.
3. The success criterion is that the sub-agent returns a task result; `spawn_agent` returning an id only shows that the sub-thread was created.
4. Check that the sub-request's `agent_message` contains only `input_text`, with no `encrypted_content`.
5. Watch upstream 4xx responses, account status and compatibility-layer logs. If anything is abnormal, turn the switch off and restart all related instances.

## Exact conditions for taking effect

The compatibility layer checks all of the following:

- `CODEX_MULTI_AGENT_V2_COMPAT` is true;
- The downstream `User-Agent` is an official Codex client (`Codex Desktop/`, `codex-tui/`, `codex_cli_rs`, `codex_exec/`);
- A Codex account was actually selected;
- The request contains `spawn_agent`, `send_message` or `followup_task` under the top-level `collaboration` namespace, or the flat `collaboration.<tool>` / `collaboration__<tool>` at the top level;
- At least one of these tools has `parameters.properties.message.encrypted`;
- The request has no conflict with an existing `collaboration-optimize` namespace;
- Tool nesting does not exceed 8 levels and no more than 16 tools are to be rewritten; when exceeded, the original request is kept and a skip is logged.

If any condition is not met, nothing is rewritten.

Validated client version: Codex CLI `0.162.0-alpha.17.2`. Codex Desktop and other versions have not been validated separately; re-run acceptance after an upgrade.

## Protocol handling

Request side:

- Scan the top-level `tools[]` and `input[].additional_tools[].tools[]`;
- Recognize only the three functions under the top-level `collaboration` namespace, and the flat top-level `collaboration.<tool>` / `collaboration__<tool>`; same-named tools under other namespaces are not rewritten;
- Delete `parameters.properties.message.encrypted` from these three tools;
- Rename the `collaboration` namespace to `collaboration-optimize`; flat tools are renamed to `collaboration-optimize.<tool>` or `collaboration-optimize__<tool>`;
- Rewrite the `tool_choice` pointing at these tools in the same way, including the `function`, `custom` and `allowed_tools` forms;
- Keep other tools and unrelated fields unchanged.

Response side:

- Restore the temporary namespace in `item`, `output`, `response.output`, `tools`, `response.tools`, `tool_choice` and `response.tool_choice`;
- Tool calls are restored to `namespace: collaboration` + `name: <tool>`; the namespace declaration, echoed tool definitions and `tool_choice` get their original prefix back;
- When upstream omits `encrypted_function_args` or returns `null`, and `arguments` is valid JSON, `message` is a string and does not look like known ciphertext, fill in `encrypted_function_args: []`;
- Accept `collaboration-optimize.<tool>` and `collaboration-optimize__<tool>`;
- Flat tool definitions echoed in the response are restored to `collaboration.<tool>`;
- Opaque fields such as `arguments`, `input` and `output` are not rewritten;
- Streaming SSE, non-streaming JSON, HTTP and native Responses WebSocket share the same restore logic;
- Numbers keep their original precision.

Upstream error bodies are outside the restore scope. If the upstream 4xx text contains `collaboration-optimize`, the client may see this temporary name; it is an internal compatibility detail and does not contain task text.

## Safe failure policy

- Do not parse the task body, do not decrypt, and do not relabel `encrypted_content` as `input_text`.
- Before filling in `encrypted_function_args: []`, check that `arguments` is a string and `message` is non-empty; missing fields, invalid JSON, empty text, non-string values and known ciphertext shapes are all kept as they are. Ciphertext detection is a heuristic (`gAAAA`, JWE `eyJ`, `enc_`, long base64url tokens) and is not a complete guarantee.
- If the request has a `collaboration-optimize` conflict, invalid JSON, or nesting or count overflow, keep the original request and log a skip; do not do a half-conversion.
- The response restore takes effect only when this request actually performed the rename; a request that was not renamed triggers no response rewrite.
- Logs record the account, model, tool name, whether it took effect, whether it looks like ciphertext, and the skip reason; they do not record task bodies, auth information or raw SSE.
- When upstream rejects the rewritten schema there is no automatic fallback, and it appears as 4xx. When the effective log and a 4xx appear together, turn the switch off first and keep a redacted request sample.

## Relationship to typical integration methods

The default path only handles the schema of the parent request and the restore of the response namespace; it does not handle `agent_message`. Sub-requests are usually forwarded directly by the upstream gateway to the third-party model.

Whether the sub-side works depends on that gateway: this repo's built-in Responses→Chat conversion does not currently recognize `agent_message` and returns `cannot be represented by Chat Completions`; the sub-gateway must natively support fully plaintext `agent_message`, or perform an equivalent conversion. The NewAPI used in early validation ignored that item; the later sub-side normalization fix was merged in [NewAPI fork PR #259](https://github.com/DeliciousBuding/new-api/pull/259), and together with this compatibility layer it completed end-to-end acceptance. Other sub-gateways still need to be checked for their own handling.

The sub-side conversion can only handle fully plaintext `input_text`; `encrypted_content` cannot be treated as task text. This PR covers only the parent side and does not include an unwired sub-side adapter.

## Validation

As of 2026-10-18: unit tests and integration tests pass; the parent side has also passed validation on the real production path.

With the compatibility layer enabled, the parent model's `spawn_agent` call is filled in as `encrypted_function_args: []`, and the sub-request's `agent_message` contains only plaintext `input_text`, whose body matches the task the parent model issued.

The sub-side is an independent dependency: third-party sub-agent requests usually do not pass through this repo. Early validation confirmed that without converting `agent_message` the model cannot read the task; after the later plaintext normalization was connected, the initial dispatch from GPT v2 parent to DeepSeek child, the `followup_task` to the same sub-agent, and two read-only tool calls passed, and the native GPT sub-agent regression passed. The parent call's `encrypted_function_args=[]`, and the sub-message contains only `input_text`. This acceptance did not separately cover `send_message`, nor does it cover forcing v2 on a third-party parent model.

Unit and integration test coverage:

- Top-level and `additional_tools`, nested `collaboration` namespace, and the flat `collaboration.*` / `collaboration__*` tool forms;
- All three collaboration tools are covered, and same-named tools under other namespaces are unaffected;
- Requests without `spawn_agent` that have only `send_message` / `followup_task`;
- With the switch off, from a non-Codex client, with a namespace conflict, with invalid JSON, or with nesting or count overflow, the request is kept as it is;
- The `function`, `custom` and `allowed_tools` forms of `tool_choice` are rewritten in sync;
- Restore for SSE, non-streaming JSON, HTTP and WebSocket;
- Restore for flat tool definitions, double underscore and dot forms;
- For `encrypted_function_args` omitted or returned as `null` by upstream: a plaintext string is filled in as `[]`, while ciphertext, invalid arguments and missing fields are kept as they are;
- `arguments` is not rewritten, numeric precision is kept, and the operation is idempotent;

The client decision rules and the gateway restore are covered by the Go tests above; real upstream results are below.

First real upstream validation (2026-10-18, before the sub-side fix):

- The parent request had already switched to plaintext collaboration tools, and the call returned by upstream `spawn_agent` was filled in with `encrypted_function_args: []`;
- The sub-request's `agent_message` contains only `input_text`, and its body matches the task the parent model issued;
- The sub-side gateway (NewAPI) did not turn `agent_message` into an ordinary user message, so the model still could not read the task; the same request changed to `message` + `role=user` returned the correct result;
- So at that time the parent-side boundary was validated, and the sub-side conversion was a separate blocking item.

Later end-to-end acceptance (2026-10-18, after the sub-side fix):

- The `spawn_agent` from GPT v2 parent to DeepSeek child and the `followup_task` to the same sub-agent both returned the correct task result;
- The sub-agent completed two read-only tool calls, and the native GPT sub-agent regression passed;
- The parent's two collaboration calls had `encrypted_function_args=[]`, and both of the child's `agent_message` entries contained only `input_text`;
- The results above depend on the parent-side compatibility and sub-side normalization taking effect together; they do not mean that every sub-gateway is compatible.

Additional acceptance checklist:

- An independent real call of `send_message`, and other client/sub-gateway combinations;
- After upgrading upstream or the client, re-verify the task body, follow-up dispatch, tool calls and upstream errors.

## Known limitations

- Depends on the Codex upstream honoring the modified schema and generating plaintext `arguments`. When upstream omits `encrypted_function_args`, the gateway fills it in; if `message` still looks like ciphertext, it is not filled in and the diagnostic is kept.
- Ciphertext detection is a heuristic and not a complete guarantee; the current native Codex upstream uses the `gAAAA` prefix, and unknown formats may be misjudged or missed. When it hits, the choice is not to fill in, keeping the native encryption behavior.
- Native OpenAI and third-party sub-agents cannot be told apart; once enabled, all matching v2 collaboration calls use plaintext.
- When upstream rejects, there is no automatic fallback, and it appears as 4xx.
- The temporary namespace is an internal compatibility detail; if the upstream reserved schema or the client decision rules change, it needs to be re-evaluated.
- Covers only the v2 collaboration tools of the official Codex client; v1, ordinary tools and other protocols are not affected.
- Upstream error bodies are not restored; the client may see the temporary namespace.
- The `function_call` records in the request history keep their original namespace; the real account has accepted this history, and it needs re-verification if upstream policy changes.
- User-Agent checks are not a security boundary.
- The sub-side gateway must support fully plaintext `agent_message` or perform an equivalent conversion. The fixed NewAPI fork combination passed acceptance; other combinations must be verified separately.
- Forcing v2 on a third-party parent model is outside the scope of this compatibility layer; the accepted choice is third-party parent models on v1 and GPT parent models on v2.

## Rollback

Set `CODEX_MULTI_AGENT_V2_COMPAT` to `false` or delete the variable, then restart all instances handling related traffic. There is no database migration or persistent state. A restart disconnects in-flight streaming requests and WebSocket connections, and clients need to reconnect.

## References

Codex client `0.162.0-alpha.17.2`:

- `codex-rs/core/src/tools/router.rs`
- `codex-rs/core/src/tools/handlers/multi_agents_v2.rs`
- `codex-rs/core/src/tools/handlers/multi_agents_spec.rs`

OpenAI Codex:

- [PR #26210](https://github.com/openai/codex/pull/26210): Encrypt multi-agent v2 message payloads
- [PR #35845](https://github.com/openai/codex/pull/35845): Support plaintext collaboration tool messages
- [Issue #36376](https://github.com/openai/codex/issues/36376): OpenAI parent still sends encrypted V2 task to non-OpenAI child
- [Issue #36586](https://github.com/openai/codex/issues/36586): Subagent task payload invisible to custom non-OpenAI providers
- [Issue #46939](https://github.com/openai/codex/issues/46939): Add a config option to select plaintext delivery

codex2api:

- Upstream issue #342: reserved `collaboration.spawn_agent` schema error
- Upstream issue #741 / PR #742: Codex client tool and history compatibility

CLIProxyAPI (MIT):

- [optimize_multi_agent_v2.go](https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/internal/client/codex/optimize-multi-agent-v2/optimize_multi_agent_v2.go)
- [Config option `client.codex.optimize-multi-agent-v2`](https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/config.example.yaml#L327), default `false`, applies to both OAuth and API-key credentials
- [MIT License](https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/LICENSE)
- [Issue #4746](https://github.com/router-for-me/CLIProxyAPI/issues/4746): `followup_task` still stays encrypted
- [Issue #4801](https://github.com/router-for-me/CLIProxyAPI/issues/4801): third-party Responses providers do not support `agent_message`
- [Issue #6233](https://github.com/router-for-me/CLIProxyAPI/issues/6233): handling of `author` / `recipient` in compat mode
