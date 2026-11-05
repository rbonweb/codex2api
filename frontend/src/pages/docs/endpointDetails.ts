import type { EndpointSpec, ParameterSpec } from "./docsContent";

export function addEndpointDetails(endpoint: EndpointSpec): EndpointSpec {
  const p = (
    name: string,
    type: string,
    required: boolean,
    description: string,
    defaultValue?: string,
  ): ParameterSpec => ({ name, type, required, description, defaultValue });
  const model = p(
    "model",
    "string",
    true,
    "Use a model available to your key in /v1/models.",
  );
  const stream = p(
    "stream",
    "boolean",
    false,
    "true returns SSE; false returns JSON.",
    "false",
  );
  const tier = p(
    "service_tier",
    "string",
    false,
    "Codex: fast maps to priority; ultrafast is retained; default/auto do not force a tier.",
  );
  const input = p(
    "input",
    "string | array",
    true,
    "Text or history items with role/content; preserve tool items on later turns.",
  );
  const messages = p(
    "messages",
    "array",
    true,
    "Conversation history with role and content.",
  );
  const params: Record<string, ParameterSpec[]> = {
    "api-responses": [
      model,
      input,
      stream,
      p(
        "reasoning.effort",
        "string",
        false,
        "none/minimal/low/medium/high/xhigh/ultra; max is normalized by model capability.",
      ),
      tier,
      p(
        "previous_response_id",
        "string",
        false,
        "Previous response ID in the same key scope; requires recoverable context.",
      ),
      p(
        "tools",
        "array",
        false,
        "Functions, custom tools or supported built-in tools.",
      ),
      p(
        "text.format",
        "object",
        false,
        "Structured output format, such as type=json_schema.",
      ),
    ],
    "api-chat": [
      model,
      messages,
      stream,
      p(
        "reasoning_effort",
        "string",
        false,
        "Top-level Chat reasoning effort, normalized by model capability.",
      ),
      tier,
      p(
        "tools",
        "array",
        false,
        "Function/custom tools; pair results using tool_call_id.",
      ),
      p(
        "response_format",
        "object",
        false,
        "JSON or JSON Schema output constraints.",
      ),
    ],
    "api-images-gen": [
      p(
        "model",
        "string",
        false,
        "gpt-image-2 or gpt-image-2.5-flare/sunburst.",
        "gpt-image-2",
      ),
      p("prompt", "string", true, "Image prompt."),
      p(
        "size",
        "string",
        false,
        "WIDTHxHEIGHT; gateway aliases -2k/-4k select size tiers.",
      ),
      p(
        "quality",
        "string",
        false,
        "2.5: auto/low/medium/high/xhigh/max; use supported levels for older models.",
      ),
      p("output_format", "string", false, "png / jpeg / webp"),
      p(
        "response_format",
        "string",
        false,
        "b64_json or url. With object storage, url is a temporary link; otherwise it is a data URL.",
        "b64_json",
      ),
    ],
    "api-images-edit": [
      p("model", "string", false, "An available image model.", "gpt-image-2"),
      p("prompt", "string", true, "Describe the desired edit."),
      p(
        "images[].image_url",
        "string",
        true,
        "JSON image URL or data URL; multipart uses image/image[] files instead.",
      ),
      p(
        "mask.image_url",
        "string",
        false,
        "Optional mask for gpt-image models; multipart uses a mask file.",
      ),
      p(
        "size / quality / output_format",
        "string",
        false,
        "As in image generation; choose values supported by the model.",
      ),
    ],
  };
  return {
    ...endpoint,
    category:
      endpoint.category ?? (endpoint.id.includes("images") ? "media" : "text"),
    parameters: endpoint.parameters ?? params[endpoint.id],
  };
}

export function buildExtraEndpoints(baseUrl: string): EndpointSpec[] {
  const json = (v: unknown) => JSON.stringify(v, null, 2);
  const param = (
    name: string,
    type: string,
    required: boolean,
    description: string,
  ): ParameterSpec => ({ name, type, required, description });
  const post = (path: string, body: unknown) =>
    `curl --request POST '${baseUrl}${path}' \\\n  --header 'Authorization: Bearer YOUR_API_KEY' \\\n  --header 'Content-Type: application/json' \\\n  --data '${json(body)}'`;
  const get = (path: string) =>
    `curl '${baseUrl}${path}' \\\n  --header 'Authorization: Bearer YOUR_API_KEY'`;
  const imageJob = {
    model: "gpt-image-2.5-flare",
    prompt: "A small orange cat",
    size: "1024x1024",
    quality: "high",
    n: 1,
  };
  const inputTokensBody = { model: "gpt-5.5", input: "Hello" };
  const endpoints: EndpointSpec[] = [
    {
      id: "api-image-jobs",
      category: "media",
      method: "POST",
      path: "/v1/images/jobs",
      title: "Create an async image job",
      description:
        "Queues a background job and returns 202. Requires a dashboard-created API key; non-empty input_images selects editing.",
      defaultBody: json(imageJob),
      curl: post("/v1/images/jobs", imageJob),
      parameters: [
        param("prompt", "string", true, "Image prompt, up to 8000 characters."),
        param(
          "model / size / quality",
          "string",
          false,
          "Image model and generation options.",
        ),
        param(
          "input_images",
          "string[]",
          false,
          "Source image URLs or data URLs for editing.",
        ),
        param("n", "integer", false, "Output count, default 1."),
      ],
      responses: [
        {
          code: 202,
          body: json({
            job: { id: 42, status: "queued", prompt: imageJob.prompt },
          }),
        },
      ],
    },
    {
      id: "api-image-job-status",
      category: "media",
      method: "GET",
      path: "/v1/images/jobs/:id",
      title: "Get an image job",
      description:
        "Replace :id with job.id and use the creating API key. Inspect queued/running/succeeded/failed. Download successful assets using job.assets[].proxy_url, resolving relative paths against this service URL; inspect error_message on failure.",
      curl: get("/v1/images/jobs/42"),
      parameters: [
        param("id", "path integer", true, "job.id from the create response."),
      ],
      responses: [
        {
          code: 200,
          label: "running",
          body: json({ job: { id: 42, status: "running" } }),
        },
        {
          code: 200,
          label: "succeeded",
          body: json({
            job: {
              id: 42,
              status: "succeeded",
              assets: [
                {
                  id: 7,
                  mime_type: "image/png",
                  proxy_url: "<signed-download-url>",
                },
              ],
            },
          }),
        },
        {
          code: 404,
          body: json({ error: { message: "Image job not found" } }),
        },
      ],
    },
    {
      id: "api-input-tokens",
      category: "advanced",
      method: "POST",
      path: "/v1/responses/input_tokens",
      title: "Estimate Responses tokens",
      description:
        "A local character-based estimate without upstream calls or account quota consumption. For compatibility probes, not an exact tokenizer or billing measurement.",
      curl: post("/v1/responses/input_tokens", inputTokensBody),
      defaultBody: json(inputTokensBody),
      responses: [{ code: 200, body: json({ input_tokens: 32 }) }],
    },
    {
      id: "api-compact",
      category: "advanced",
      method: "POST",
      path: "/v1/responses/compact",
      title: "Compact Responses context",
      description:
        "Submit the complete required history to receive compaction items for later requests. Preserve output items and opaque fields; support depends on model and channel.",
      curl: post("/v1/responses/compact", {
        model: "gpt-5.5",
        input: [{ role: "user", content: "Summarize this conversation." }],
      }),
      defaultBody: json({
        model: "gpt-5.5",
        input: [{ role: "user", content: "Summarize this conversation." }],
      }),
      responses: [
        {
          code: 200,
          label: "Shape example",
          body: json({
            object: "response.compaction",
            output: [{ type: "compaction", encrypted_content: "..." }],
          }),
        },
      ],
    },
    {
      id: "api-responses-ws",
      category: "advanced",
      method: "GET",
      path: "/v1/responses",
      transport: "websocket",
      title: "Responses WebSocket",
      description:
        "Use a WebSocket client supporting authentication headers. Send response.create after connecting, then read delta and terminal events. Resend full history if continuation is unavailable.",
      curl: `# WebSocket URL: ${baseUrl.replace(/^http/, "ws")}/v1/responses\n# Header: Authorization: Bearer YOUR_API_KEY\n# Send this frame after the connection opens:\n${json({ type: "response.create", model: "gpt-5.5", input: [{ role: "user", content: "Hello" }] })}`,
      responses: [
        {
          code: 101,
          label: "WebSocket",
          body: json({ type: "response.output_text.delta", delta: "Hello" }),
        },
      ],
    },
  ];
  return endpoints;
}
