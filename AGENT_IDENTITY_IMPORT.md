# Codex Agent Identity Import API

Import Codex **Agent Identity** accounts into the codex2api gateway. These accounts **do not store** OAuth access or refresh tokens. They store only an Ed25519 private key and an `agent_runtime_id`, and the gateway signs every upstream request on the fly (`Authorization: AgentAssertion <signed envelope>`).

---

## 1. General conventions

| Item | Value |
|---|---|
| Gateway address | `{BASE_URL}` (for example `http://127.0.0.1:2004`) |
| Admin auth header | `X-Admin-Key: {ADMIN_KEY}` |
| Admin API prefix | `/api/admin` |
| Content-Type | `application/json` (entry points A/B); `multipart/form-data` (entry point C) |

- Deduplication is by `agent_runtime_id`: an account that already exists is skipped and is not created twice.
- After an import, the gateway runs one signed `/responses` probe automatically to fill in the usage progress bar.
- **Do not import or delete the same `agent_runtime_id` repeatedly in a short time.** The upstream rate-limits task registration and returns 403 (temporary), which delays the first usage sample for that account.

---

## 2. auth.json format

One JSON file per account:

```json
{
  "auth_mode": "agent_identity",
  "agent_identity": {
    "agent_runtime_id": "agent-XXXXXXXXXXXXXXXXXXXXXX",
    "agent_private_key": "MC4CAQAwBQYDK2VwBCIEII...",
    "account_id": "9324fdb2-2a0c-4f39-b292-839afdb3b6ec",
    "chatgpt_user_id": "user-XXXXXXXXXXXXXXXXXXXX",
    "email": "someone@example.com",
    "plan_type": "free",
    "chatgpt_account_is_fedramp": false
  }
}
```

| Field | Required | Description |
|---|:---:|---|
| `agent_runtime_id` | Yes | Upstream agent runtime ID (`agent-` prefix) |
| `agent_private_key` | Yes | **Ed25519 private key in PKCS#8 base64** |
| `account_id` | Yes | ChatGPT account (workspace) ID |
| `chatgpt_user_id` | Yes | ChatGPT user ID (`user-` prefix) |
| `email` | No | Account email, used as the display name |
| `plan_type` | No | Plan (free/plus/pro/team/k12…), defaults to `free` |
| `chatgpt_account_is_fedramp` | No | Boolean, defaults to `false` |

- `auth_mode` accepts both `agent_identity` (snake case) and `agentIdentity` (camel case).
- An `agent_identity` object is enough to be recognized, so `auth_mode` can be omitted.

---

## 3. Three import entry points

### Entry A: single import

```
POST /api/admin/accounts/codex/agent-identity
```

Request body:

```json
{
  "auth_json": "<the raw JSON string of one auth.json>",
  "name": "optional custom name",
  "proxy_url": "optional, for example http://user:pass@host:port"
}
```

> Note: `auth_json` takes the whole auth.json **as a string** (escape it).

curl:

```bash
curl -s -X POST "{BASE_URL}/api/admin/accounts/codex/agent-identity" \
  -H "X-Admin-Key: {ADMIN_KEY}" -H "Content-Type: application/json" \
  -d "$(jq -Rs '{auth_json: .}' path/to/one.json)"
```

Response:

```json
{ "message": "Agent Identity account imported successfully", "id": 123, "email": "someone@example.com" }
```

Already exists: `409` with `{"error":"This Agent Identity account already exists"}`.
Missing field or invalid private key: `400`, with `error` giving the reason.

---

### Entry B: batch file import (recommended)

```
POST /api/admin/accounts/codex/agent-identity/import
```

Request body: a `files` array, where each item is **the raw JSON string of one auth.json**. The limit is **200 per batch**.

```json
{
  "files": ["<auth.json content 1>", "<auth.json content 2>"],
  "proxy_url": "optional"
}
```

curl (packs all `*.json` files in a directory into a `files` array):

```bash
jq -Rs . *.json | jq -s '{files: .}' | \
curl -s -X POST "{BASE_URL}/api/admin/accounts/codex/agent-identity/import" \
  -H "X-Admin-Key: {ADMIN_KEY}" -H "Content-Type: application/json" -d @-
```

Response:

```json
{
  "total": 2,
  "imported": 1,
  "failed": 1,
  "items": [
    { "email": "a@x.com", "id": 123, "ok": true },
    { "email": "b@x.com", "ok": false, "error": "Account already exists, skipped" }
  ]
}
```

---

### Entry C: generic import (auto-detected, multipart)

```
POST /api/admin/accounts/import
```

Send auth.json through the generic import as a plain JSON file. The gateway detects `agent_identity` automatically and creates the account as an Agent Identity account. The generic import also accepts CLIProxyAPI and Sub2Api JSON files, and folder scanning.

```bash
curl -s -X POST "{BASE_URL}/api/admin/accounts/import" \
  -H "X-Admin-Key: {ADMIN_KEY}" \
  -F "format=json" \
  -F "file=@one.json" \
  -F "file=@two.json"
```

Form fields: `format=json` (required), `file` (repeatable), `proxy_url` (optional), and `allow_duplicate=true` (optional, skips deduplication and forces a new account).
The response is either an SSE progress stream or a JSON summary with created, skipped and failed counts.

---

## 4. Python batch example

```python
import json, glob, requests

BASE_URL = "http://127.0.0.1:2004"
ADMIN_KEY = "your-admin-key"

files = [open(p, encoding="utf-8").read() for p in glob.glob("auth_jsons/*.json")]

# The batch limit is 200; split larger sets into batches
for i in range(0, len(files), 200):
    batch = files[i:i + 200]
    r = requests.post(
        f"{BASE_URL}/api/admin/accounts/codex/agent-identity/import",
        headers={"X-Admin-Key": ADMIN_KEY, "Content-Type": "application/json"},
        json={"files": batch},
        timeout=120,
    )
    out = r.json()
    print(f"Batch {i//200+1}: imported {out['imported']} / skipped or failed {out['failed']} / total {out['total']}")
    for it in out["items"]:
        if not it["ok"]:
            print("  failed:", it.get("email") or "?", "—", it.get("error"))
```

---

## 5. Notes

- The private key is a sensitive credential. **Do not print it to logs.**
- Imported accounts show a blue "Agent Identity" badge in the account list. These accounts use HTTP (not the long-lived WebSocket connection), and they skip the ChatGPT wham probe (there is no access token). Their usage is filled in from the `/responses` response headers.
- During large batch imports, concurrent task registration may hit upstream 403 rate limits. For some accounts, the usage progress bar is filled in later, at the next probe or on real traffic. This does not affect whether the account works.
