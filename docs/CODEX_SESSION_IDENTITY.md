# Session identity convergence

Accounts' "Device fingerprint convergence" setting has a new optional mode: **Session identity convergence**. It differs from "device + session" in that it does not merge all sessions of the same account into one session. The device is still pinned per account, but different conversations and sub-threads each keep a stable mapping.

For example, the downstream client sends:

```text
Parent task: session=root, thread=root
Child task: session=root, thread=child, parent_thread_id=root
Another conversation: session=other, thread=other
```

Under the same account, after mapping, they become:

```text
Parent task: session=A, thread=A
Child task: session=A, thread=B, parent_thread_id=A
Another conversation: session=C, thread=C
```

Here, session, thread and parent-thread references share the same mapping space. A parent reference is not counted as a different ID just because its field name differs The mapping also does not depend on current concurrency, access tokens or request order.

## Usage

In account editing, quick configuration or bulk settings, choose "Session identity convergence". The settings page can also make it the default mode for new accounts; it only affects accounts added later and does not bulk-change existing accounts.

The API / credential value is `single_machine_multi_window`. The display name is changed to "Session identity convergence", the stored value and ID derivation seed keep the values of the original experimental implementation, so existing configuration does not break and identities do not change. The behavior of the old `off`, `device`, `session` and `full` modes is unchanged, and the default is still `off`. It applies only to Codex accounts.

## Requests and connections

- Handles session headers for HTTP, compact and WebSocket, plus existing `client_metadata`. If the request body carries the identity, it is first extracted from a per-request copy of the headers, then rewritten uniformly; the caller's headers are not modified.
- The WS handshake identity is fixed when the connection is established, so the connection pool stays partitioned within the existing API key / model partitions, and is further partitioned by the mapped session/thread. The same thread can still reuse its connection, and different threads never share a frozen identity. The existing `previous_response_id` connection affinity logic is kept.
- When session/thread is missing, the identity is not guessed, and it is not derived from user content or `prompt_cache_key`. Metadata that is absent from the request is not made up.
- Does not rewrite `prompt_cache_key`, `previous_response_id`, `turn_id` or turn timing. Real subagent metadata is kept; no subagents are invented, and conversations are not squeezed into a fixed number of windows.
- The final override priority of account custom headers is kept. An explicit `CODEX_SESSION_HEADER_MODE=legacy` still uses the old header names, and the corresponding values are aligned only in the new mode.

This is not a network egress or quota setting, and it cannot be used to guarantee upstream caching, account availability or model quality. The gateway's own prompt-cache partitioning is unchanged; it has not been verified whether the upstream still uses other identity fields in its cache.

## Storage and verification

The mode value exceeds the original 20 characters, so the PostgreSQL column for the default mode of new accounts is widened to `VARCHAR(64)`; existing values are unchanged, and the SQLite TEXT column needs no migration. Before downgrading to a version that does not recognize this mode, first switch the selected accounts in this mode and the new-account default setting back to the old mode; the column does not need to be narrowed.

Local tests cover parent/child references, concurrency stability, missing or request-body identity, actual HTTP/compact outbound traffic, WS handshake and frame alignment, connection pool partitioning and reuse, admin-side save/bulk edit, and persistence of the SQLite default value. Tests use synthetic data and local HTTP/WS servers, and do not call real models. The PostgreSQL column widening has not yet been verified on a real instance.

The design was compared with `DeanZFC/sub2api-custom` `backend/internal/service/openai_codex_fingerprint.go` at commit `08ef70d1750e665ede7c2fb2306ef48599428c66`. This does not port its fixed-window slot algorithm; instead it reuses this project's ID derivation helper to map the relationships the client already has.
