// Quick-integration software definitions for the Docs page.
// Inspired by NewAPI's BUILTIN_TEMPLATES — adapted for the Codex proxy.

export type QuickToolKind = "protocol" | "config" | "env";

export type QuickTool = {
  id: string;
  name: string;
  badge: string;
  iconHue: string;
  glyph: string;
  blurb: string;
  kind: QuickToolKind;
  url?: string;
  template?: string;
  templateLang?: string;
  templateLabel?: string;
};

export function buildQuickTools(): QuickTool[] {
  return [
    {
      id: "cc-switch",
      name: "CC Switch",
      badge: "Desktop",
      iconHue: "bg-fuchsia-500/12 text-fuchsia-600 dark:text-fuchsia-400",
      glyph: "CS",
      blurb: "Multi-account launcher for Codex. Import config with one click.",
      kind: "protocol",
      url: "ccswitch",
    },
    {
      id: "codex-cli",
      name: "Codex CLI",
      badge: "CLI",
      iconHue: "bg-emerald-500/12 text-emerald-600 dark:text-emerald-400",
      glyph: "CX",
      blurb:
        "Official OpenAI Responses CLI. Paste the generated config.toml and go.",
      kind: "config",
      template: `model_provider = "OpenAI"
model = "gpt-5.5"
review_model = "gpt-5.5"
model_reasoning_effort = "xhigh"
disable_response_storage = true
network_access = "enabled"

[model_providers.OpenAI]
name = "OpenAI"
base_url = "{address}"
wire_api = "responses"
requires_openai_auth = true

[features]
goals = true`,
      templateLang: "toml",
      templateLabel: "~/.codex/config.toml",
    },
    {
      id: "cherry-studio",
      name: "Cherry Studio",
      badge: "Desktop",
      iconHue: "bg-rose-500/12 text-rose-600 dark:text-rose-400",
      glyph: "CY",
      blurb:
        "Launch the desktop app and import OpenAI credentials automatically.",
      kind: "protocol",
      url: "cherrystudio://providers/api-keys?v=1&data={cherryConfig}",
    },
    {
      id: "lobe-chat",
      name: "Lobe Chat",
      badge: "Web",
      iconHue: "bg-sky-500/12 text-sky-600 dark:text-sky-400",
      glyph: "LC",
      blurb: "Open Lobe Chat in the browser with OpenAI settings prefilled.",
      kind: "protocol",
      url: 'https://chat-preview.lobehub.com/?settings={"keyVaults":{"openai":{"apiKey":"{key}","baseURL":"{address}/v1"}}}',
    },
    {
      id: "opencat",
      name: "OpenCat",
      badge: "Mobile",
      iconHue: "bg-amber-500/12 text-amber-600 dark:text-amber-400",
      glyph: "OC",
      blurb:
        "Launch the iOS/macOS client and join the server configuration directly.",
      kind: "protocol",
      url: "opencat://team/join?domain={address}&token={key}",
    },
  ];
}

function encodeBase64(text: string): string {
  if (typeof btoa === "function") {
    return btoa(unescape(encodeURIComponent(text)));
  }
  return text;
}

export function resolveTemplate(
  tool: QuickTool,
  address: string,
  key: string,
): string {
  const base = tool.kind === "protocol" ? tool.url : tool.template;
  if (!base) return "";
  if (tool.id === "cc-switch") {
    const params = new URLSearchParams();
    params.set("resource", "provider");
    params.set("app", "codex");
    params.set("name", "Codex2API");
    params.set("endpoint", `${address}/v1`);
    params.set("apiKey", key);
    params.set("model", "gpt-5.5");
    params.set("homepage", address);
    params.set("enabled", "true");
    return `ccswitch://v1/import?${params.toString()}`;
  }
  if (base.includes("{cherryConfig}")) {
    const cfg = encodeURIComponent(
      encodeBase64(
        JSON.stringify({
          id: "codex2api",
          baseUrl: address,
          apiKey: key,
        }),
      ),
    );
    return base.split("{cherryConfig}").join(cfg);
  }
  if (base.includes("{aionuiConfig}")) {
    const cfg = encodeURIComponent(
      encodeBase64(
        JSON.stringify({
          platform: "codex2api",
          baseUrl: address,
          apiKey: key,
        }),
      ),
    );
    return base.split("{aionuiConfig}").join(cfg);
  }
  const encodedAddress = encodeURIComponent(address);
  const encodedKey = encodeURIComponent(key);
  return base
    .split("{address}")
    .join(tool.kind === "protocol" ? encodedAddress : address)
    .split("{key}")
    .join(tool.kind === "protocol" ? encodedKey : key);
}
