# Documentation Index

## API Documentation

The full API documentation is in [`docs/API.md`](../docs/API.md), covering all public and admin APIs.

### OpenAPI/Swagger Specification

The complete OpenAPI 3.0.3 specification is available in [`openapi.yaml`](./openapi.yaml).

### Error Codes Reference

See [`errors.md`](./docs/errors.md) for detailed error code documentation.

### API Versioning

This API uses URL path versioning (e.g., `/v1/`). The current version is v1.0.0.

### Authentication

**Public API** (`/v1/*`) uses API key authentication:

```
Authorization: Bearer sk-...
```

**Admin API** (`/api/admin/*`) uses admin secret authentication:

```
X-Admin-Key: your-admin-secret
```

or

```
Authorization: Bearer your-admin-secret
```

### Response Formats

#### Success Response

```json
{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "created": 1234567890,
  "model": "gpt-5.4",
  "choices": [...],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 20,
    "total_tokens": 30
  }
}
```

#### Error Response

```json
{
  "error": {
    "code": "invalid_request",
    "message": "Invalid request",
    "type": "invalid_request_error",
    "details": {
      "field": "model",
      "message": "Model 'invalid-model' is not supported"
    }
  }
}
```

### Rate Limiting

Rate limits are returned in response headers:

- `X-RateLimit-Limit`: Maximum requests allowed
- `X-RateLimit-Remaining`: Remaining requests
- `X-RateLimit-Reset`: Unix timestamp when the limit resets

### Public API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/models` | GET | List available models |
| `/v1/chat/completions` | POST | Create chat completion |
| `/v1/responses` | POST | Create response (Codex native) |
| `/v1/images/generations` | POST | Create image generation |
| `/v1/images/edits` | POST | Create image edit |
| `/health` | GET | Health check |

### Admin API Endpoints

> The table below is a quick reference. For full request/response formats, see [`docs/API.md`](../docs/API.md).

**Token upload & account management:**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/admin/accounts` | GET | List accounts |
| `/api/admin/accounts` | POST | Add RT accounts (batch supported) |
| `/api/admin/accounts/at` | POST | Add AT-only accounts (batch supported) |
| `/api/admin/accounts/import` | POST | Batch import from a file (TXT/JSON/AT-TXT) |
| `/api/admin/accounts/:id` | DELETE | Delete account |
| `/api/admin/accounts/:id/refresh` | POST | Manually refresh AT |
| `/api/admin/accounts/:id/test` | GET | Test account connection |
| `/api/admin/accounts/:id/usage` | GET | View account usage |
| `/api/admin/accounts/:id/models/sync-upstream` | POST | Read-only preview of the account's upstream model catalog |
| `/api/admin/accounts/batch-test` | POST | Batch test connections (SSE) |
| `/api/admin/accounts/export` | GET | Export accounts |
| `/api/admin/accounts/migrate` | POST | Migrate accounts from a remote instance (SSE) |
| `/api/admin/accounts/event-trend` | GET | Account add/remove trend |
| `/api/admin/accounts/clean-banned` | POST | Clean up 401 accounts |
| `/api/admin/accounts/clean-rate-limited` | POST | Clean up 429 accounts |
| `/api/admin/accounts/clean-error` | POST | Clean up error accounts |


**OAuth authorization:**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/admin/oauth/generate-auth-url` | POST | Generate an OAuth authorization URL |
| `/api/admin/oauth/exchange-code` | POST | Exchange an authorization code for a token |

**API key management:**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/admin/keys` | GET | List keys |
| `/api/admin/keys` | POST | Create a key |
| `/api/admin/keys/:id` | DELETE | Delete a key |

**System settings & operations:**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/admin/stats` | GET | Dashboard statistics |
| `/api/admin/health` | GET | Health check |
| `/api/admin/ops/overview` | GET | Operations monitoring overview |
| `/api/admin/settings` | GET | Get system settings |
| `/api/admin/settings` | PUT | Update system settings |
| `/api/admin/models` | GET | List supported models |
| `/api/admin/models/sync` | POST | Sync the model registry from OpenAI's official Codex models page |

**Usage statistics:**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/admin/usage/stats` | GET | Usage statistics |
| `/api/admin/usage/logs` | GET | Usage logs (paginated) |
| `/api/admin/usage/chart-data` | GET | Aggregated chart data |
| `/api/admin/usage/logs` | DELETE | Clear logs |

**Proxy pool management:**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/admin/proxies` | GET | List proxies |
| `/api/admin/proxies` | POST | Add proxies (batch supported) |
| `/api/admin/proxies/:id` | DELETE | Delete a proxy |
| `/api/admin/proxies/:id` | PATCH | Update a proxy |
| `/api/admin/proxies/batch-delete` | POST | Batch delete |
| `/api/admin/proxies/test` | POST | Test proxy connectivity |

### Model Support

Built-in fallback models include:
- `gpt-5.5`
- `gpt-5.4`
- `gpt-5.4-mini`
- `gpt-5.3-codex`
- `gpt-5.3-codex-spark` (requires a Pro subscription account)
- `gpt-5.2`
- `gpt-image-2`

Use `/api/admin/models/sync` to merge newer official Codex models into the runtime registry. The sync never removes local models and always keeps `gpt-image-2`.
