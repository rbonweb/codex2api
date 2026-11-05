import { resolveTemplate, type QuickTool } from "./quickStartTools";
import { addEndpointDetails, buildExtraEndpoints } from "./endpointDetails";
import { buildGuides, guideToMarkdown } from "./docsGuides";

export type ParameterSpec = {
  name: string;
  type: string;
  required: boolean;
  description: string;
  defaultValue?: string;
};
export type ResponseExample = {
  code: number;
  body: string;
  label?: string;
  lang?: string;
};

export type EndpointSpec = {
  id: string;
  method: string;
  path: string;
  title: string;
  description: string;
  curl: string;
  defaultBody?: string;
  responses: ResponseExample[];
  category?: "text" | "media" | "advanced";
  transport?: "websocket";
  parameters?: ParameterSpec[];
  requestExamples?: { label: string; lang: string; content: string }[];
};

export function buildEndpointSpecs(baseUrl: string): EndpointSpec[] {
  const endpoints: EndpointSpec[] = [
    {
      id: "api-responses",
      method: "POST",
      path: "/v1/responses",
      title: "Create Responses output",
      description:
        "Native Codex Responses endpoint with optional streaming, forwarded directly upstream.",
      defaultBody: `{
  "model": "gpt-5.5",
  "input": [{"role": "user", "content": [{"type": "input_text", "text": "Hello"}]}],
  "stream": false
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/v1/responses \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-5.5",
  "input": [
    {"role": "user", "content": [{"type": "input_text", "text": "Hello, what can you do?"}]}
  ],
  "stream": false,
  "reasoning": {"effort": "high"}
}'`,
      responses: [
        {
          code: 200,
          body: `{
  "id": "resp_abc123",
  "object": "response",
  "model": "gpt-5.5",
  "status": "completed",
  "output": [
    {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Hello!"}]}
  ],
  "usage": {"input_tokens": 12, "output_tokens": 45, "total_tokens": 57}
}`,
        },
        {
          code: 400,
          body: `{
  "error": {"code": "invalid_request", "message": "model is required", "type": "invalid_request_error"}
}`,
        },
        {
          code: 401,
          body: `{
  "error": {"code": "invalid_api_key", "message": "Invalid API key provided", "type": "authentication_error"}
}`,
        },
        {
          code: 503,
          body: `{
  "error": {"message": "No available account, please retry later", "type": "server_error", "code": "no_available_account"}
}`,
        },
        {
          code: 503,
          label: "Quota exhausted",
          body: `{
  "error": {"message": "Rate limit exceeded", "type": "server_error", "code": "account_pool_usage_limit_reached", "resets_in_seconds": 18000}
}`,
        },
      ],
    },
    {
      id: "api-chat",
      method: "POST",
      path: "/v1/chat/completions",
      title: "Create Chat Completions output",
      description:
        "OpenAI Chat Completions compatible endpoint that translates between OpenAI and Codex Responses formats.",
      defaultBody: `{
  "model": "gpt-5.5",
  "messages": [{"role": "user", "content": "Hello"}],
  "stream": false
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/v1/chat/completions \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-5.5",
  "messages": [
    {"role": "system", "content": "You are a helpful assistant."},
    {"role": "user", "content": "Hello!"}
  ],
  "stream": false,
  "reasoning_effort": "high"
}'`,
      responses: [
        {
          code: 200,
          body: `{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "model": "gpt-5.5",
  "choices": [
    {"index": 0, "message": {"role": "assistant", "content": "Hello! How can I help you today?"}, "finish_reason": "stop"}
  ],
  "usage": {"prompt_tokens": 18, "completion_tokens": 9, "total_tokens": 27}
}`,
        },
        {
          code: 400,
          body: `{
  "error": {"code": "invalid_request", "message": "Request validation failed", "type": "invalid_request_error"}
}`,
        },
        {
          code: 401,
          body: `{
  "error": {"code": "invalid_api_key", "message": "Invalid API key provided", "type": "authentication_error"}
}`,
        },
      ],
    },
    {
      id: "api-images-gen",
      method: "POST",
      path: "/v1/images/generations",
      title: "Generate images",
      description:
        "Supports GPT Image 2 / 2.5. response_format defaults to b64_json; with S3-compatible storage, url returns a 1-hour signed link, otherwise a base64 data URL.",
      defaultBody: `{
  "model": "gpt-image-2",
  "prompt": "Draw a small orange cat",
  "size": "1024x1024",
  "quality": "high"
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/v1/images/generations \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-image-2",
  "prompt": "Draw a small orange cat",
  "response_format": "b64_json"
}'`,
      responses: [
        {
          code: 200,
          label: "Base64",
          body: `{
  "created": 1710000000,
  "model": "gpt-image-2",
  "data": [{"b64_json": "..."}],
  "usage": {"images": 1}
}`,
        },
        {
          code: 200,
          label: "URL",
          body: `{
  "created": 1710000000,
  "model": "gpt-image-2",
  "data": [{"url": "https://<bucket-endpoint>/images/api/...png?X-Amz-Expires=3600&X-Amz-Signature=..."}],
  "usage": {"images": 1}
}`,
        },
      ],
    },
    {
      id: "api-images-edit",
      method: "POST",
      path: "/v1/images/edits",
      title: "Edit images",
      description:
        "OpenAI Images edit compatible endpoint supporting JSON image_url input and multipart uploads.",
      defaultBody: `{
  "model": "gpt-image-2",
  "prompt": "Replace the background with aurora lights",
  "images": [{"image_url": "https://example.com/source.png"}],
  "output_format": "png"
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/v1/images/edits \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-image-2",
  "prompt": "Replace the background with aurora lights",
  "images": [{"image_url": "https://example.com/source.png"}]
}'`,
      requestExamples: [
        {
          label: "multipart",
          lang: "bash",
          content: `curl '${baseUrl}/v1/images/edits' \\
  -H 'Authorization: Bearer YOUR_API_KEY' \\
  -F 'model=gpt-image-2.5-flare' \\
  -F 'prompt=Replace the background with aurora lights' \\
  -F 'image=@source.png' \\
  -F 'mask=@mask.png'`,
        },
      ],
      responses: [
        {
          code: 200,
          body: `{
  "created": 1710000000,
  "model": "gpt-image-2",
  "data": [{"b64_json": "..."}]
}`,
        },
      ],
    },
    {
      id: "api-models",
      method: "GET",
      path: "/v1/models",
      title: "List models",
      description: "List the models currently exposed by this proxy.",
      curl: `curl --request GET \\
  --url ${baseUrl}/v1/models \\
  --header 'Authorization: Bearer <token>'`,
      responses: [
        {
          code: 200,
          body: `{
  "object": "list",
  "data": [
    {"id": "gpt-6-astra", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.6-sol", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.6-terra", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.6-luna", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.5", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.3-codex-spark", "object": "model", "owned_by": "openai"},
    {"id": "gpt-image-2", "object": "model", "owned_by": "openai"}
  ]
}`,
        },
        {
          code: 401,
          body: `{
  "error": {"code": "invalid_api_key", "message": "Invalid API key provided", "type": "authentication_error"}
}`,
        },
      ],
    },
    {
      id: "api-health",
      method: "GET",
      path: "/health",
      title: "Health check",
      description:
        "Inspect service status and available account count; this endpoint does not require authentication.",
      curl: `curl --request GET \\
  --url ${baseUrl}/health`,
      responses: [
        {
          code: 200,
          body: `{
  "status": "ok",
  "available": 5,
  "total": 8
}`,
        },
      ],
    },
  ];
  return [...endpoints, ...buildExtraEndpoints(baseUrl)].map((endpoint) =>
    addEndpointDetails(endpoint),
  );
}

export function buildAdminSpecs(baseUrl: string): EndpointSpec[] {
  return [
    {
      id: "admin-add-rt",
      method: "POST",
      path: "/api/admin/accounts",
      title: "Add account (Refresh Token)",
      description:
        "Add an account via Refresh Token. The system refreshes its Access Token and adds it to the pool automatically. Set Chatgpt-Account-Id in custom_headers to keep separate workspace routes for the same login.",
      defaultBody: `{
  "name": "my-account",
  "refresh_token": "<refresh-token>",
  "proxy_url": "",
  "custom_headers": {
    "Chatgpt-Account-Id": "workspace-id"
  }
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/api/admin/accounts \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "name": "my-account",
  "refresh_token": "<refresh-token>\\n<another-refresh-token>",
  "proxy_url": "",
  "custom_headers": {
    "Chatgpt-Account-Id": "workspace-id"
  }
}'`,
      responses: [
        {
          code: 200,
          body: `{
  "message": "Added 1 account",
  "success": 1,
  "failed": 0
}`,
        },
        { code: 400, body: `{"error": "refresh_token is required"}` },
        { code: 401, body: `{"error": "Unauthorized"}` },
      ],
    },
    {
      id: "admin-add-at",
      method: "POST",
      path: "/api/admin/accounts/at",
      title: "Add account (Access Token)",
      description:
        "Add AT-only accounts; the access_token field can contain multiple tokens separated by newlines. Use custom_headers.Chatgpt-Account-Id to select the target workspace.",
      defaultBody: `{
  "name": "at-account",
  "access_token": "eyJhbGciOi...",
  "proxy_url": "",
  "custom_headers": {
    "Chatgpt-Account-Id": "workspace-id"
  }
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/at \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "name": "at-account",
  "access_token": "eyJhbGciOi...",
  "proxy_url": "",
  "custom_headers": {
    "Chatgpt-Account-Id": "workspace-id"
  }
}'`,
      responses: [
        {
          code: 200,
          body: `{
  "message": "Added 1 AT-only account",
  "success": 1,
  "failed": 0
}`,
        },
        { code: 400, body: `{"error": "access_token is required"}` },
      ],
    },
    {
      id: "admin-import",
      method: "POST",
      path: "/api/admin/accounts/import",
      title: "Bulk import accounts from file",
      description:
        "Bulk import accounts from file. Supports txt, CLIProxyAPI-exported json, and at_txt with one access token per line; max file size is 20MB.",
      curl: `# TXT — one Refresh Token per line
curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/import \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --form 'file=@tokens.txt' \\
  --form 'format=txt' \\
  --form 'proxy_url='

# JSON — CLIProxyAPI credential export
curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/import \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --form 'file=@credentials.json' \\
  --form 'format=json' \\
  --form 'proxy_url='

# AT TXT — one Access Token per line
curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/import \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --form 'file=@access_tokens.txt' \\
  --form 'format=at_txt' \\
  --form 'proxy_url='`,
      responses: [
        {
          code: 200,
          body: `{
  "message": "Import complete: 5 succeeded, 0 failed, 2 duplicates",
  "total": 7,
  "success": 5,
  "failed": 0,
  "duplicate": 2
}`,
        },
        { code: 400, body: `{"error": "Upload a file (field name: file)"}` },
      ],
    },
    {
      id: "admin-delete",
      method: "DELETE",
      path: "/api/admin/accounts/:id",
      title: "Delete account",
      description:
        "Delete an account by ID and remove it from the active pool.",
      curl: `curl --request DELETE \\
  --url ${baseUrl}/api/admin/accounts/1 \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [
        { code: 200, body: `{"message": "Account deleted"}` },
        { code: 404, body: `{"error": "Account not found"}` },
      ],
    },
    {
      id: "admin-list",
      method: "GET",
      path: "/api/admin/accounts",
      title: "List accounts",
      description:
        "List accounts with status, usage, tags, account groups, and basic metadata.",
      curl: `curl --request GET \\
  --url ${baseUrl}/api/admin/accounts \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [
        {
          code: 200,
          body: `{
  "accounts": [
    {
      "id": 1,
      "name": "my-account",
      "email": "user@example.com",
      "plan_type": "team",
      "status": "active",
      "proxy_url": "",
      "tags": ["team"],
      "group_ids": [1],
      "allowed_api_key_ids": [],
      "created_at": "2025-01-01T00:00:00Z",
      "total_requests": 128,
      "success_requests": 125
    }
  ]
}`,
        },
      ],
    },
    {
      id: "admin-update-scheduler",
      method: "PATCH",
      path: "/api/admin/accounts/:id/scheduler",
      title: "Update account scheduler config",
      description:
        "Update account proxy, tags, account groups, concurrency/score overrides, and reverse API key authorization. Omitted fields keep existing values; allowed_api_key_ids set to null or [] means unrestricted API key access.",
      defaultBody: `{
  "proxy_url": "",
  "tags": ["team", "paid"],
  "group_ids": [1, 2],
  "allowed_api_key_ids": [],
  "score_bias_override": null,
  "base_concurrency_override": null,
  "skip_warm_tier": true,
  "keep_concurrency_on_degrade": false
}`,
      curl: `curl --request PATCH \\
  --url ${baseUrl}/api/admin/accounts/1/scheduler \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "tags": ["team", "paid"],
  "group_ids": [1, 2],
  "allowed_api_key_ids": [],
  "skip_warm_tier": true
}'`,
      responses: [
        {
          code: 200,
          body: `{"message": "Account scheduling settings updated"}`,
        },
        {
          code: 400,
          body: `{"error": "allowed_api_key_ids contains an API key ID that does not exist: 99"}`,
        },
        { code: 404, body: `{"error": "Account not found"}` },
      ],
    },
    {
      id: "admin-list-keys",
      method: "GET",
      path: "/api/admin/keys",
      title: "List API keys",
      description:
        "List downstream API keys created in the admin panel, including quota, usage, expiration, status, and allowed account groups. The raw_key field contains the full secret and must only be used in trusted admin contexts.",
      curl: `curl --request GET \\
  --url ${baseUrl}/api/admin/keys \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [
        {
          code: 200,
          body: `{
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
      "created_at": "2026-05-13T00:00:00Z"
    }
  ]
}`,
        },
      ],
    },
    {
      id: "admin-create-key",
      method: "POST",
      path: "/api/admin/keys",
      title: "Create API key",
      description:
        "Create a downstream API key. The key can be generated automatically; quota_limit omitted or set to 0 means unlimited; empty allowed_group_ids means all account groups are allowed.",
      defaultBody: `{
  "name": "Codex CLI",
  "quota_limit": 10,
  "expires_in_days": 30,
  "allowed_group_ids": [1]
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/api/admin/keys \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "name": "Codex CLI",
  "quota_limit": 10,
  "expires_in_days": 30,
  "allowed_group_ids": [1]
}'`,
      responses: [
        {
          code: 200,
          body: `{
  "id": 2,
  "key": "sk-...",
  "name": "Codex CLI",
  "quota_limit": 10,
  "quota_used": 0,
  "expires_at": "2026-06-12T00:00:00Z",
  "allowed_group_ids": [1]
}`,
        },
        {
          code: 400,
          body: `{"error": "allowed_group_ids contains a group ID that does not exist: 99"}`,
        },
      ],
    },
    {
      id: "admin-update-key",
      method: "PATCH",
      path: "/api/admin/keys/:id",
      title: "Edit API key",
      description:
        "Edit API key name, quota, expiration, and allowed account groups. Omitted fields keep existing values; quota_limit set to 0/null clears the limit; expires_at set to null or expires_in_days set to 0 clears expiration.",
      defaultBody: `{
  "name": "Cherry Studio",
  "quota_limit": 25,
  "expires_at": null,
  "allowed_group_ids": []
}`,
      curl: `curl --request PATCH \\
  --url ${baseUrl}/api/admin/keys/2 \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "name": "Cherry Studio",
  "quota_limit": 25,
  "expires_at": null,
  "allowed_group_ids": []
}'`,
      responses: [
        { code: 200, body: `{"message": "API key updated"}` },
        { code: 400, body: `{"error": "Quota limit cannot be less than 0"}` },
        { code: 404, body: `{"error": "API key not found"}` },
      ],
    },
    {
      id: "admin-delete-key",
      method: "DELETE",
      path: "/api/admin/keys/:id",
      title: "Delete API key",
      description:
        "Delete an API key and immediately revoke client access using it.",
      curl: `curl --request DELETE \\
  --url ${baseUrl}/api/admin/keys/2 \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [{ code: 200, body: `{"message": "Deleted"}` }],
    },
    {
      id: "admin-list-groups",
      method: "GET",
      path: "/api/admin/account-groups",
      title: "List account groups",
      description:
        "List account groups with color, description, and member count.",
      curl: `curl --request GET \\
  --url ${baseUrl}/api/admin/account-groups \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [
        {
          code: 200,
          body: `{
  "groups": [
    {"id": 1, "name": "Team", "description": "Paid team accounts", "color": "#2563eb", "member_count": 8, "sort_order": 0}
  ]
}`,
        },
      ],
    },
    {
      id: "admin-create-group",
      method: "POST",
      path: "/api/admin/account-groups",
      title: "Create account group",
      description:
        "Create an account group. Accounts may belong to multiple groups, and API keys may be restricted to specific groups.",
      defaultBody: `{
  "name": "Team",
  "description": "Paid team accounts",
  "color": "#2563eb"
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/api/admin/account-groups \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{"name":"Team","description":"Paid team accounts","color":"#2563eb"}'`,
      responses: [
        { code: 200, body: `{"id": 1, "message": "Group created"}` },
        { code: 400, body: `{"error": "Group name cannot be empty"}` },
      ],
    },
    {
      id: "admin-update-group",
      method: "PATCH",
      path: "/api/admin/account-groups/:id",
      title: "Edit account group",
      description:
        "Edit account group name, description, color, and sort order. Account and API key relations remain bound by group ID after rename operations.",
      defaultBody: `{
  "name": "Team Plus",
  "description": "High-priority accounts",
  "color": "#16a34a",
  "sort_order": 10
}`,
      curl: `curl --request PATCH \\
  --url ${baseUrl}/api/admin/account-groups/1 \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{"name":"Team Plus","description":"High-priority accounts","color":"#16a34a","sort_order":10}'`,
      responses: [
        { code: 200, body: `{"message": "Group updated"}` },
        { code: 404, body: `{"error": "Group not found"}` },
      ],
    },
    {
      id: "admin-delete-group",
      method: "DELETE",
      path: "/api/admin/account-groups/:id",
      title: "Delete account group",
      description:
        "Delete an empty account group. If the group still has members, append ?force=true; the ID is removed from account memberships and pruned from API key scopes where safe. If an API key only referenced this group, the missing group ID is retained so access does not silently broaden.",
      curl: `curl --request DELETE \\
  --url '${baseUrl}/api/admin/account-groups/1?force=true' \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [
        { code: 200, body: `{"message": "Group deleted"}` },
        {
          code: 409,
          body: `{"error": "The group still has accounts; confirm to force delete"}`,
        },
      ],
    },
  ];
}

function endpointToMd(e: EndpointSpec): string {
  const responses = e.responses
    .map(
      (r) =>
        `**${r.code}${r.label ? ` · ${r.label}` : ""}**\n\n\`\`\`${r.lang || "json"}\n${r.body}\n\`\`\``,
    )
    .join("\n\n");
  const parameters =
    e.parameters
      ?.map(
        (p) =>
          `- \`${p.name}\` (${p.type}${p.required ? ", required" : ""})${p.defaultValue ? ` [${p.defaultValue}]` : ""}: ${p.description}`,
      )
      .join("\n") || "";
  const examples =
    e.requestExamples
      ?.map((e) => `**${e.label}**\n\n\`\`\`${e.lang}\n${e.content}\n\`\`\``)
      .join("\n\n") || "";
  return `<a id="${e.id}"></a>\n\n### ${e.method} ${e.path} — ${e.title}\n\n${e.description}\n\n${parameters}\n\n\`\`\`bash\n${e.curl}\n\`\`\`\n\n${examples}\n\n${responses}`;
}

export function buildDocsMarkdown(args: {
  baseUrl: string;
  quickTools: QuickTool[];
  apiKeyExample: string;
  clientConfigs?: { label: string; lang: string; content: string }[];
}): string {
  const { baseUrl, quickTools, apiKeyExample, clientConfigs } = args;
  const guides = buildGuides(baseUrl);
  const clients =
    clientConfigs ||
    quickTools
      .filter((tool) => tool.kind !== "protocol")
      .map((tool) => ({
        label: tool.name,
        lang: tool.templateLang || "text",
        content: resolveTemplate(tool, baseUrl, apiKeyExample),
      }));
  const block = (label: string, lang: string, content: string) =>
    `### ${label}\n\n\`\`\`${lang}\n${content}\n\`\`\``;
  return [
    `# Codex2API documentation`,
    `> Service URL: ${baseUrl}`,
    `## Quick start`,
    "1. Enable an eligible account.\n2. Create a client API key.\n3. Configure the client and send a minimal request.\n4. Check the result and usage in the dashboard.",
    ...clients.map((e) => block(e.label, e.lang, e.content)),
    `## Development`,
    ...guides.filter((g) => g.section === "development").map(guideToMarkdown),
    `## API reference`,
    ...guides.filter((g) => g.section === "model-api").map(guideToMarkdown),
    ...buildEndpointSpecs(baseUrl).map(endpointToMd),
    `## Troubleshooting`,
    ...guides
      .filter((g) => g.section === "troubleshooting")
      .map(guideToMarkdown),
    `## Administration`,
    ...guides.filter((g) => g.section === "admin-api").map(guideToMarkdown),
    ...buildAdminSpecs(baseUrl).map(endpointToMd),
  ].join("\n\n");
}
