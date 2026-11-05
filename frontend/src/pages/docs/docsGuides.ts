export type DocsSection =
  "quick-start" | "development" | "model-api" | "troubleshooting" | "admin-api";
export type GuideSpec = {
  id: string;
  section: DocsSection;
  title: string;
  summary: string;
  paragraphs?: string[];
  table?: { headers: string[]; rows: string[][] };
  examples?: { label: string; lang: string; content: string }[];
  links?: { label: string; href: string }[];
};

export function buildGuides(baseUrl: string): GuideSpec[] {
  const base = baseUrl.replace(/\/+$/, "");
  return [
    {
      id: "authentication",
      section: "development",
      title: "URLs & authentication",
      summary: "Choose the destination and the correct credential first.",
      paragraphs: [
        "Clients connect to this service. An account's proxy_url controls the service's outbound proxy; it is not the client API Base URL.",
        "Public endpoints normally use an API key created in the dashboard. Admin endpoints use the Admin Secret without an additional downstream API key. Anonymous access to ordinary public endpoints requires both no configured keys and CODEX_ALLOW_ANONYMOUS=true. Async image jobs still require a dashboard-created API key.",
      ],
      table: {
        headers: ["Use", "URL / header", "Notes"],
        rows: [
          [
            "Codex CLI",
            base,
            "This gateway also accepts Responses paths without /v1.",
          ],
          [
            "OpenAI SDK",
            `${base}/v1`,
            "The SDK appends resource paths such as /responses.",
          ],
          [
            "Public API",
            "Authorization: Bearer YOUR_API_KEY",
            "All public endpoints use Bearer authentication.",
          ],
          [
            "Admin API",
            "X-Admin-Key: YOUR_ADMIN_SECRET",
            "For /api/admin/*; keep it out of ordinary client configuration.",
          ],
        ],
      },
      links: [
        {
          label: "Manage API keys",
          href: "/admin/api-keys",
        },
      ],
    },
    {
      id: "protocols",
      section: "development",
      title: "Protocols & models",
      summary: "Request format and model name determine the route.",
      table: {
        headers: ["Endpoint", "Use case", "Key fields"],
        rows: [
          [
            "Responses",
            "Codex, tools and response chains",
            "input · reasoning.effort · text.format",
          ],
          [
            "Chat Completions",
            "Existing OpenAI chat clients",
            "messages · reasoning_effort · response_format",
          ],
          [
            "Images",
            "Image generation, editing and async jobs",
            "prompt · model · size",
          ],
        ],
      },
      paragraphs: [
        "Use the current /v1/models catalog and your key's permissions. A listed model is not guaranteed to be available on every account. Groups, mappings and account readiness affect scheduling.",
        "The Codex path transforms or sanitizes some parameters. reasoning.effort and Chat's reasoning_effort are normalized for the final model; max is retained for supported newer models and becomes xhigh on older ones. fast maps to priority, ultrafast is retained, and auto/default do not force a tier. Actual latency and limits depend on the account and response.",
      ],
      links: [
        {
          label: "Model pricing & usage",
          href: "/admin/model-pricing",
        },
        { label: "Model mapping", href: "#client-mapping" },
      ],
    },
    {
      id: "sdk-examples",
      section: "development",
      title: "Your first SDK request",
      summary:
        "Set CODEX2API_KEY on your server and use this instance's /v1 URL.",
      paragraphs: [
        "Python: pip install openai. Node.js: npm install openai, then save the example as example.mjs. Replace the gateway model name with one available to your key.",
      ],
      examples: [
        {
          label: "Python · Responses",
          lang: "python",
          content: `import os\nfrom openai import OpenAI\n\nclient = OpenAI(\n    api_key=os.environ["CODEX2API_KEY"],\n    base_url=${JSON.stringify(`${base}/v1`)},\n)\nresponse = client.responses.create(\n    model="gpt-5.5", input="Say hello in one sentence.", stream=False,\n)\nprint(response.output_text)`,
        },
        {
          label: "Node.js · Responses",
          lang: "javascript",
          content: `import OpenAI from "openai";\n\nconst client = new OpenAI({\n  apiKey: process.env.CODEX2API_KEY,\n  baseURL: ${JSON.stringify(`${base}/v1`)},\n});\nconst response = await client.responses.create({\n  model: "gpt-5.5", input: "Say hello in one sentence.", stream: false,\n});\nconsole.log(response.output_text);`,
        },
      ],
      links: [
        {
          label: "Official OpenAI SDK documentation",
          href: "https://developers.openai.com/api/docs/libraries",
        },
      ],
    },
    {
      id: "streaming",
      section: "development",
      title: "Streaming & terminal states",
      summary: "stream=false returns JSON; stream=true returns SSE events.",
      paragraphs: [
        "Use curl -N to disable output buffering. For Responses, read response.output_text.delta and handle response.completed, response.incomplete, response.failed or error. A disconnected stream is not a successful completion. Chat uses choices[].delta and [DONE].",
      ],
      examples: [
        {
          label: "Node.js · SSE",
          lang: "javascript",
          content: `// Reuse the client from the SDK example.\nconst stream = await client.responses.create({\n  model: "gpt-5.5", input: "Say hello.", stream: true,\n});\nlet completed = false;\nfor await (const event of stream) {\n  if (event.type === "response.output_text.delta") process.stdout.write(event.delta);\n  if (event.type === "response.completed") completed = true;\n  if (["error", "response.failed", "response.incomplete"].includes(event.type)) {\n    throw new Error(JSON.stringify(event));\n  }\n}\nif (!completed) throw new Error("Stream ended before completion");`,
        },
      ],
      links: [
        {
          label: "Streaming event reference",
          href: "https://developers.openai.com/api/docs/guides/streaming-responses",
        },
      ],
    },
    {
      id: "conversation-tools",
      section: "development",
      title: "Conversation history & tools",
      summary:
        "Preserve history and call_id so the model can continue after a tool result.",
      paragraphs: [
        "Store the original input, all response.output items and tool results, then send them together on the next turn. Saving only output_text can lose reasoning and tool context. The example executes only the allowed tool and preserves call pairing.",
        "previous_response_id requires recoverable history in the same key scope. TTL, capacity and write policy apply, and Memory caching with a file-backed SQLite database enables a local on-disk store by default (without it, history is lost on restart); some store:false sessions do not populate replay history. On 409 previous_response_not_found, send full history and start a new chain instead of retrying the same unavailable ID.",
      ],
      examples: [
        {
          label: "Python · function_call_output",
          lang: "python",
          content: `import json\n\nhistory = [{"role": "user", "content": "What is 2 plus 3?"}]\ntools = [{\n    "type": "function", "name": "add", "description": "Add two numbers",\n    "parameters": {"type": "object", "properties": {\n        "a": {"type": "number"}, "b": {"type": "number"}\n    }, "required": ["a", "b"], "additionalProperties": False},\n    "strict": True,\n}]\nfor _ in range(5):\n    reply = client.responses.create(model="gpt-5.5", input=history, tools=tools)\n    history.extend(reply.output)\n    calls = [item for item in reply.output if item.type == "function_call"]\n    if not calls:\n        print(reply.output_text)\n        break\n    for call in calls:\n        if call.name != "add":\n            raise ValueError("Unknown tool")\n        args = json.loads(call.arguments)\n        history.append({"type": "function_call_output", "call_id": call.call_id,\n                        "output": str(args["a"] + args["b"])})\nelse:\n    raise RuntimeError("Tool round limit reached")`,
        },
      ],
    },
    {
      id: "structured-output",
      section: "development",
      title: "Structured output",
      summary: "Responses uses text.format; Chat uses response_format.",
      paragraphs: [
        "Start with a simple object and explicit required/additionalProperties. Some constraints depend on the final model, Lite capability and HTTP/WS transport. Tool parameter schemas and output schemas do not have identical support.",
      ],
      examples: [
        {
          label: "Responses · text.format",
          lang: "json",
          content: JSON.stringify(
            {
              model: "gpt-5.5",
              input: "Return a short greeting.",
              text: {
                format: {
                  type: "json_schema",
                  name: "greeting",
                  strict: true,
                  schema: {
                    type: "object",
                    properties: { answer: { type: "string" } },
                    required: ["answer"],
                    additionalProperties: false,
                  },
                },
              },
            },
            null,
            2,
          ),
        },
      ],
    },
    {
      id: "media-guide",
      section: "model-api",
      title: "Image guide",
      summary:
        "Synchronous images and persistent image jobs have different response shapes.",
      table: {
        headers: ["Capability", "How to use"],
        rows: [
          [
            "GPT Image 2.5",
            "Flare / Sunburst accept auto, low, medium, high, xhigh and max; an omitted model still defaults to gpt-image-2.",
          ],
          [
            "Size & format",
            "size is WIDTHxHEIGHT; -2k/-4k are gateway size aliases. output_format selects encoding; response_format selects b64_json or url.",
          ],
          [
            "Edits & masks",
            "JSON uses images[].image_url; multipart uses image/image[]. Masks are supported only for the gpt-image family.",
          ],
          [
            "Image jobs",
            "POST /v1/images/jobs returns 202 and job. Query job.id with the same API key and inspect job.status.",
          ],
        ],
      },
      paragraphs: [
        "For image generation through Responses, top-level model is the text driver and tools[].model is the image model. The Images endpoint selects its driver from dashboard settings. See model pricing and usage for cost and token details.",
      ],
      links: [
        {
          label: "Open Image Studio",
          href: "/admin/images/studio",
        },
        { label: "Usage", href: "/admin/usage" },
      ],
    },
    {
      id: "errors",
      section: "troubleshooting",
      title: "Find the cause by error",
      summary:
        "Check the HTTP status and error.code before changing configuration or retrying.",
      table: {
        headers: ["Symptom", "What to check"],
        rows: [
          [
            "401",
            "Check the downstream API key, header, key status and expiration. Use the Admin Secret for admin endpoints.",
          ],
          [
            "403",
            "Check key model/group permissions and upstream account capability.",
          ],
          [
            "429 · rate_limit_reached",
            "May indicate a weekly model budget. Inspect used, limit and reset_at in error.details, plus Retry-After.",
          ],
          [
            "503 · no_available_account",
            "Check enabled accounts, groups, cooldown, concurrency and available models.",
          ],
          [
            "503 · account_pool_concurrency_saturated",
            "Matching accounts exist, but their concurrency windows are full. Retry shortly or raise the per-account concurrency limit.",
          ],
          [
            "503 · account_pool_usage_limit_reached",
            "Upstream account quota is exhausted. Respect Retry-After, wait for reset or add eligible accounts.",
          ],
          [
            "409 · previous_response_not_found",
            "Previous context is unavailable. Resend the full required history and start a new chain.",
          ],
          [
            "503 · service_unavailable",
            "Context or quota storage may be temporarily unavailable. Check operations and retry with backoff.",
          ],
          [
            "Stream ends early / stalls",
            "Check terminal events, client timeouts, reverse-proxy buffering and connection logs. Retrying after partial output can produce output and usage again.",
          ],
        ],
      },
      links: [
        { label: "Request logs", href: "/admin/usage" },
        { label: "Operations", href: "/admin/ops/overview" },
      ],
    },
    {
      id: "connection-check",
      section: "troubleshooting",
      title: "From connectivity to a successful response",
      summary:
        "Health, model listing and generation validate different parts of the chain.",
      paragraphs: [
        "1. GET /health only verifies service health. 2. Request /v1/models with the client key to check URL and authentication. 3. Send a minimal generation request to a model with eligible accounts. 4. Inspect the final model, status, duration and tokens in Usage.",
        "For 404, check duplicated /v1, the complete path and whether the server version supports the endpoint. Image job queries also require the creating API key.",
      ],
      examples: [
        {
          label: "cURL · models",
          lang: "bash",
          content: `curl "${base}/v1/models" \\\n  -H "Authorization: Bearer YOUR_API_KEY"`,
        },
      ],
    },
    {
      id: "key-limits",
      section: "admin-api",
      title: "Keys, budgets & groups",
      summary:
        "Monetary budgets, weekly request counts and upstream quotas are separate limits.",
      paragraphs: [
        "Monetary budgets limit a key's cumulative cost. Weekly rules match the final mapped model, and all models matching one rule share its count. Timezone, weekday and reset time are configured separately from upstream plan quotas.",
        "Accounts can belong to multiple groups, while API keys can restrict groups. If accounts exist but requests fail, check both key scope and account membership. Exhausted weekly model budgets return 429 with the rule's reset time.",
      ],
      examples: [
        {
          label: "API Key · limits",
          lang: "json",
          content: JSON.stringify(
            {
              limits: {
                model_request_limits: [
                  {
                    model: "gpt-6*",
                    window: "week",
                    max_requests: 50,
                    timezone: "Asia/Shanghai",
                    reset_weekday: 1,
                    reset_time: "00:00",
                  },
                ],
              },
            },
            null,
            2,
          ),
        },
      ],
      links: [
        {
          label: "Configure key limits",
          href: "/admin/api-keys",
        },
      ],
    },
    {
      id: "client-mapping",
      section: "admin-api",
      title: "Model mapping & operations",
      summary: "The requested model can differ from the final upstream model.",
      paragraphs: [
        "Codex redirects support wildcards such as gpt-*, *-mini and *codex*. Check mappings in Settings and the final model in request logs rather than relying only on the client's displayed name.",
        "The repository API reference covers account refresh, batch testing, proxy pools, usage, settings and image assets. The common account, key and group endpoints below expand on demand.",
      ],
      links: [
        { label: "Settings", href: "/admin/settings" },
        {
          label: "Full API reference",
          href: "https://github.com/rbonweb/codex2api/blob/main/docs/API.md",
        },
        {
          label: "Deployment & troubleshooting",
          href: "https://github.com/rbonweb/codex2api/blob/main/docs/TROUBLESHOOTING.md",
        },
      ],
    },
  ];
}

export function guideToMarkdown(guide: GuideSpec): string {
  const table = guide.table
    ? [
        guide.table.headers,
        guide.table.headers.map(() => "---"),
        ...guide.table.rows,
      ]
        .map(
          (row) =>
            `| ${row.map((cell) => cell.replace(/\|/g, "\\|")).join(" | ")} |`,
        )
        .join("\n")
    : "";
  return [
    `<a id="${guide.id}"></a>`,
    `### ${guide.title}`,
    guide.summary,
    ...(guide.paragraphs ?? []),
    table,
    ...(guide.examples ?? []).map(
      (e) => `#### ${e.label}\n\n\`\`\`${e.lang}\n${e.content}\n\`\`\``,
    ),
    ...(guide.links ?? []).map((link) => `[${link.label}](${link.href})`),
  ]
    .filter(Boolean)
    .join("\n\n");
}
