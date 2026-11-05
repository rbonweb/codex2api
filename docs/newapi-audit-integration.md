# Optional NewAPI identity and execution adapter

NewAPI is an optional external adapter, not a prerequisite for Prompt Filter, local blocking, external model review, or risk profiling. Deployments that do not use NewAPI can ignore this document.

Codex2API only accepts a NewAPI signed identity that is bound one-to-one to the Codex2API key the request actually uses:

- When a request uses a bound key, the identity is verified only with that binding's enabled state and binding secret.
- A key without a binding does not accept a NewAPI signed identity.
- The legacy process-level secret configuration no longer takes part in signature verification.

The binding does not control the Prompt blocking mode or the review tier. All requests are governed by the Codex2API GuardPipeline. The `mode` and `profile` that NewAPI sends are accepted only as compatibility metadata and do not override the global security policy.

Prompt filtering does not depend on the NewAPI audit integration. Violation strikes, account restrictions, and IP restrictions are controlled by NewAPI's separate switches, all off by default.

## Configuration on both sides

In the admin page, create a binding for the Codex2API key that the caller actually uses. The system generates and stores a binding secret of at least 32 bytes. The binding secret must not be written to logs or to business response bodies.

NewAPI:

```env
CODEX2API_POLICY_ENABLED=false
CODEX2API_POLICY_IDENTITY_FORWARD_ENABLED=true
CODEX2API_POLICY_BINDINGS=[{"platform_id":"primary-newapi","target":"http://127.0.0.1:18095","codex_key_fingerprint":"<SHA-256 fingerprint of the Codex2API key>","secret":"<binding secret for this key, at least 32 characters>","enabled":true}]
CODEX2API_POLICY_AUDIT_ENABLED=true
CODEX2API_POLICY_STRIKE_ENABLED=false
CODEX2API_POLICY_ACCOUNT_BAN_ENABLED=false
CODEX2API_POLICY_IP_BLOCK_ENABLED=false
CODEX2API_POLICY_BAN_AFTER=2
CODEX2API_POLICY_WINDOW_SECONDS=86400
```

On first deployment, keep `CODEX2API_POLICY_ENABLED=false`, enable it as needed after connectivity and signature tests pass, and keep the penalty sub-switches off.

Each entry in `CODEX2API_POLICY_BINDINGS` must match both the actual outbound target address and the fingerprint of the Codex2API key actually used. The raw Codex2API key must not be written to this config; the fingerprint is the lowercase hex SHA-256 value of the raw key. The binding config saved on the admin page has the same structure as this environment variable; when the environment variable is set, it takes precedence.

## Request signing protocol

NewAPI generates a unique request ID and a Unix timestamp in seconds, and builds the signing string:

```text
v1\n<timestamp>\n<request_id>\n<user_id>\n<client_ip>\n<http_method>\n<request_path>\n<body_sha256>
```

Compute HMAC-SHA256 with the binding secret of the current Codex2API key, and write the lowercase hex result to the following request headers:

```text
X-NewAPI-User-ID
X-NewAPI-Client-IP
X-NewAPI-Request-ID
X-NewAPI-Timestamp
X-NewAPI-Method
X-NewAPI-Path
X-NewAPI-Body-SHA256
X-NewAPI-Signature-Version: 1
X-NewAPI-Signature
```

The original endpoint, protocol, and model provider go into `X-NewAPI-Policy-Meta`, signed with a separate `policy-meta-v1` HMAC. For compatibility with existing NewAPI deployments, Codex2API can still parse its mode and tier fields, but does not use them as blocking config. Extended metadata does not change the V1 identity signature format.

## Recommendations for implementing an external adapter

1. Sign when creating upstream request headers on an OpenAI-compatible channel, and do not accept same-named headers submitted by the client.
2. Set a target address allowlist, and send identity headers only to the Codex2API host.
3. When a signed HTTP 400 policy decision or a WebSocket policy event arrives, verify the decision signature and the event signature first, and only then store the user, IP, request ID, model, endpoint, and length-limited Prompt evidence.
4. Audit, violation strikes, account bans, and IP restrictions must be independent settings with penalties off by default; enabling audit must not trigger automatic bans.
5. Admins and super admins should keep automatic-penalty protection, so a false positive cannot lock them out of the admin panel.
6. The audit page must use admin authentication; Prompt evidence should have a length limit and a data retention period.

The file layout and persistence implementation of external projects are not part of this repository's contract. A compatible implementation only needs to follow the signing, target restriction, penalties-off-by-default, and privacy boundaries above.
