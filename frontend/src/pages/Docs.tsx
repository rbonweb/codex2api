import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { DocsSearch, GuideArticle } from "./docs/DocsChrome";
import { buildGuides, type DocsSection } from "./docs/docsGuides";
import { resolveDocsTarget, type DocEntry } from "./docs/docsNavigation";
import "./docs/docs.css";
import {
  Copy,
  Check,
  ExternalLink,
  Sparkles,
  Terminal,
  Wand2,
  Server,
  BookOpen,
  LifeBuoy,
  List,
} from "lucide-react";
import { api, getAdminKey } from "../api";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { useToast } from "../hooks/useToast";
import { CodeBlock, EndpointDoc } from "./docs/EndpointDoc";
import DocsTOC, { type DocsTOCItem } from "./docs/DocsTOC";
import { buildQuickTools, type DocsLocale } from "./docs/quickStartTools";
import {
  buildAdminSpecs,
  buildDocsMarkdown,
  buildEndpointSpecs,
} from "./docs/docsContent";
import { getLobeIconFileUrl } from "../components/ModelLogo";
import type { ModelsResponse, SystemSettings } from "../types";

const FALLBACK_MODELS = [
  "gpt-5.5",
  "gpt-5.6-sol",
  "gpt-5.6-luna",
  "gpt-6-astra",
];
const DEFAULT_QUICK_START_MODEL = "gpt-5.5";
type CCSwitchApp = "codex";
type QuickToolTab = "codex-cli" | "cc-switch" | "cherry-studio";
type QuickServiceTier = "default" | "fast" | "ultrafast";
type QuickReasoningEffort =
  | "none"
  | "minimal"
  | "low"
  | "medium"
  | "high"
  | "xhigh"
  | "ultra";

const CC_SWITCH_LOGO = "https://ccswitch.io/assets/cc-switch-logo-BPrI77SG.png";
// Bundled from npm `@lobehub/icons-static-svg`.
const CLIENT_ICON_SRC: Record<QuickToolTab, string> = {
  "codex-cli": getLobeIconFileUrl("codex-color.svg") ?? "",
  "cc-switch": CC_SWITCH_LOGO,
  "cherry-studio": getLobeIconFileUrl("cherrystudio-color.svg") ?? "",
};

const CC_SWITCH_APPS: Record<
  CCSwitchApp,
  {
    label: string;
    suffix: string;
    endpoint: (baseUrl: string) => string;
    fields: { key: string }[];
  }
> = {
  codex: {
    label: "Codex CLI",
    suffix: "Codex",
    endpoint: (baseUrl) => `${baseUrl}/v1`,
    fields: [{ key: "model" }],
  },
};

function OsTabs({
  active,
  onChange,
}: {
  active: "unix" | "windows";
  onChange: (v: "unix" | "windows") => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="border-b border-border mb-4">
      <nav className="-mb-px flex space-x-4">
        <button
          type="button"
          aria-pressed={active === "unix"}
          onClick={() => onChange("unix")}
          className={`whitespace-nowrap py-2.5 px-1 border-b-2 font-medium text-sm transition-colors flex items-center gap-2 ${
            active === "unix"
              ? "border-primary text-primary"
              : "border-transparent text-muted-foreground hover:text-foreground hover:border-border"
          }`}
        >
          macOS / Linux
        </button>
        <button
          type="button"
          aria-pressed={active === "windows"}
          onClick={() => onChange("windows")}
          className={`whitespace-nowrap py-2.5 px-1 border-b-2 font-medium text-sm transition-colors flex items-center gap-2 ${
            active === "windows"
              ? "border-primary text-primary"
              : "border-transparent text-muted-foreground hover:text-foreground hover:border-border"
          }`}
        >
          Windows
        </button>
      </nav>
    </div>
  );
}

const FIELD_LABEL = "mb-1.5 block text-[11px] font-bold text-muted-foreground";
const FIELD_INPUT =
  "h-8 w-full rounded-lg border border-input bg-background px-2.5 text-[13px] font-medium text-foreground shadow-xs outline-none transition-[border-color,box-shadow,background-color] placeholder:text-muted-foreground hover:border-primary/30 hover:bg-accent/50 focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/20";
const CONFIG_PANEL =
  "grid gap-3 rounded-lg border border-border bg-muted/25 p-3 shadow-[inset_0_1px_0_hsl(0_0%_100%/0.35)] dark:shadow-none";

function FieldBox({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="block min-w-0">
      <span className={FIELD_LABEL}>{label}</span>
      {children}
    </label>
  );
}

async function copyToClipboard(text: string) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.cssText = "position:fixed;left:-9999px";
    document.body.appendChild(ta);
    ta.select();
    document.execCommand("copy");
    document.body.removeChild(ta);
  }
}

function ClientIcon({ id, size = 18 }: { id: QuickToolTab; size?: number }) {
  return (
    <img
      src={CLIENT_ICON_SRC[id]}
      alt=""
      width={size}
      height={size}
      className="rounded-[4px] object-contain"
      loading="lazy"
      decoding="async"
    />
  );
}

function ImportPreviewCard({
  title,
  description,
  icon,
  link,
  disabled,
  onLaunch,
  onCopied,
}: {
  title: string;
  description: string;
  icon: ReactNode;
  link: string;
  disabled?: boolean;
  onLaunch: () => void;
  onCopied: () => void;
}) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(false);
  const [copied, setCopied] = useState(false);

  const handleCopy = async () => {
    await copyToClipboard(link);
    setCopied(true);
    onCopied();
    window.setTimeout(() => setCopied(false), 1200);
  };

  return (
    <div className="rounded-xl border border-border bg-card/80 p-3.5 shadow-sm transition-[border-color,box-shadow] duration-200 hover:border-primary/25 hover:shadow-md">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex items-start gap-3">
          <span className="inline-flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-primary/15">
            {icon}
          </span>
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <h4 className="text-[14px] font-bold text-foreground">{title}</h4>
              <Badge
                variant="outline"
                className="h-5 px-1.5 text-[10px] font-bold"
              >
                {t("docs.quickStart.deeplinkBadge")}
              </Badge>
            </div>
            <p className="mt-0.5 text-[12.5px] leading-relaxed text-muted-foreground">
              {description}
            </p>
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => void handleCopy()}
            className="h-8 gap-1.5"
          >
            {copied ? (
              <Check className="size-3.5 text-emerald-500" />
            ) : (
              <Copy className="size-3.5" />
            )}
            {copied
              ? t("docs.quickStart.copied")
              : t("docs.quickStart.copyLink")}
          </Button>
          <Button
            type="button"
            size="sm"
            disabled={disabled}
            onClick={onLaunch}
            className="h-8 gap-1.5"
          >
            <ExternalLink className="size-3.5" />
            {t("docs.quickStart.openClient")}
          </Button>
        </div>
      </div>
      <button
        type="button"
        onClick={() => setExpanded((current) => !current)}
        className="mt-2 inline-flex items-center gap-1 text-[12px] font-semibold text-muted-foreground hover:text-foreground"
      >
        {expanded
          ? t("docs.quickStart.collapseFullLink")
          : t("docs.quickStart.viewFullLink")}
      </button>
      {expanded && (
        <div className="mt-2 rounded-lg border border-border bg-muted/25 p-2.5">
          <code className="block max-h-28 overflow-auto break-all font-mono text-[12px] leading-relaxed text-foreground">
            {link}
          </code>
        </div>
      )}
    </div>
  );
}

function ToolTabButton({
  selected,
  id,
  label,
  onClick,
}: {
  selected: boolean;
  id: QuickToolTab;
  label: string;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      aria-pressed={selected}
      onClick={onClick}
      className={`inline-flex h-8 items-center justify-center gap-2 rounded-lg px-3 text-[13px] font-semibold transition-[background-color,color,box-shadow] ${
        selected
          ? "bg-background text-foreground shadow-sm ring-1 ring-border"
          : "text-muted-foreground hover:bg-background/60 hover:text-foreground"
      }`}
    >
      <span className="inline-flex size-5 items-center justify-center">
        <ClientIcon id={id} size={18} />
      </span>
      {label}
    </button>
  );
}

function UnderlineTabs<T extends string>({
  tabs,
  active,
  onChange,
}: {
  tabs: { value: T; label: string; hint?: string }[];
  active: T;
  onChange: (value: T) => void;
}) {
  return (
    <div className="border-b border-border">
      <nav className="-mb-px flex space-x-4">
        {tabs.map((tab) => {
          const selected = active === tab.value;
          return (
            <button
              key={tab.value}
              type="button"
              aria-pressed={selected}
              onClick={() => onChange(tab.value)}
              className={`whitespace-nowrap border-b-2 px-1 py-2.5 text-sm font-medium transition-colors ${
                selected
                  ? "border-primary text-primary"
                  : "border-transparent text-muted-foreground hover:border-border hover:text-foreground"
              }`}
              title={tab.hint}
            >
              {tab.label}
            </button>
          );
        })}
      </nav>
    </div>
  );
}

function buildCCSwitchImportUrl({
  app,
  name,
  endpoint,
  apiKey,
  models,
  homepage,
  serviceTier,
  reasoningEffort,
}: {
  app: CCSwitchApp;
  name: string;
  endpoint: string;
  apiKey: string;
  models: Record<string, string>;
  homepage: string;
  serviceTier?: QuickServiceTier;
  reasoningEffort?: QuickReasoningEffort;
}) {
  const params = new URLSearchParams();
  params.set("resource", "provider");
  params.set("app", app);
  params.set("name", name);
  params.set("endpoint", endpoint);
  params.set("apiKey", apiKey);
  Object.entries(models).forEach(([key, value]) => {
    if (value) params.set(key, value);
  });
  if (app === "codex" && serviceTier && serviceTier !== "default") {
    params.set("service_tier", serviceTier);
  }
  if (app === "codex" && reasoningEffort) {
    params.set("model_reasoning_effort", reasoningEffort);
  }
  params.set("homepage", homepage);
  params.set("enabled", "true");
  return `ccswitch://v1/import?${params.toString()}`;
}

function slugProviderId(name: string) {
  const slug = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return slug || "codexproxy";
}

function modelIncludes(models: string[], model?: string) {
  return Boolean(model && models.includes(model));
}

function encodeBase64(text: string): string {
  return btoa(unescape(encodeURIComponent(text)));
}

export default function Docs() {
  const { t, i18n } = useTranslation();
  const baseUrl = useMemo(() => window.location.origin, []);
  const adminSeed = useMemo(() => getAdminKey(), []);
  const docsLocale = useMemo<DocsLocale>(
    () => ((i18n.language || "zh").startsWith("zh") ? "zh" : "en"),
    [i18n.language],
  );
  const [quickBaseUrl, setQuickBaseUrl] = useState(baseUrl);
  const [codexOs, setCodexOs] = useState<"unix" | "windows">("unix");
  const [firstKey, setFirstKey] = useState("");
  const [allKeys, setAllKeys] = useState<{ name: string; key: string }[]>([]);
  const [copyingMd, setCopyingMd] = useState(false);
  const { showToast } = useToast();
  const [selectedKey, setSelectedKey] = useState("");
  const [activeToolTab, setActiveToolTab] = useState<QuickToolTab>("codex-cli");
  const [quickStartModel, setQuickStartModel] = useState(
    DEFAULT_QUICK_START_MODEL,
  );
  const [quickServiceTier, setQuickServiceTier] =
    useState<QuickServiceTier>("default");
  const [quickReasoningEffort, setQuickReasoningEffort] =
    useState<QuickReasoningEffort>("xhigh");
  const [settings, setSettings] = useState<SystemSettings | null>(null);
  const ccSwitchApp: CCSwitchApp = "codex";
  const [ccSwitchName, setCcSwitchName] = useState("");
  const [ccSwitchNameEdited, setCcSwitchNameEdited] = useState(false);
  const [ccSwitchModels, setCcSwitchModels] = useState<Record<string, string>>({
    model: DEFAULT_QUICK_START_MODEL,
  });
  const [cherryProviderId, setCherryProviderId] = useState("");
  const [cherryProviderEdited, setCherryProviderEdited] = useState(false);
  const [activeCurl, setActiveCurl] = useState<"responses" | "chat">(
    "responses",
  );
  const [curlModel, setCurlModel] = useState(DEFAULT_QUICK_START_MODEL);
  const [models, setModels] = useState(FALLBACK_MODELS);

  useEffect(() => {
    api
      .getAPIKeys({ view: 'lite' })
      .then((res) => {
        const keys = (res.keys ?? []).map((k) => ({
          name: k.name,
          key: k.raw_key || k.key,
        }));
        setAllKeys(keys);
        if (keys.length > 0) {
          setFirstKey(keys[0].key);
          setSelectedKey(keys[0].key);
        }
      })
      .catch(() => {});
  }, []);

  useEffect(() => {
    Promise.all([
      api.getModels().catch(
        (): ModelsResponse => ({
          models: [],
          items: [],
          source_url: "",
        }),
      ),
      api.getSettings().catch(() => null),
    ])
      .then(([res, nextSettings]) => {
        setSettings(nextSettings);
        const next = [
          ...(res.models ?? []),
          ...(res.items ?? []).map((item) => item.id),
        ].filter((model): model is string => Boolean(model));
        const unique = Array.from(new Set(next));
        if (unique.length === 0) return;
        setModels(unique);
        const configuredModel = nextSettings?.test_model;
        const preferred =
          configuredModel && unique.includes(configuredModel)
            ? configuredModel
            : unique.includes(DEFAULT_QUICK_START_MODEL)
              ? DEFAULT_QUICK_START_MODEL
              : unique[0];
        setQuickStartModel((current) =>
          unique.includes(current) ? current : preferred,
        );
        setCurlModel((current) =>
          unique.includes(current) ? current : preferred,
        );
        setCcSwitchModels((current) => ({
          ...current,
          model: unique.includes(current.model) ? current.model : preferred,
        }));
      })
      .catch(() => {});
  }, []);

  const modelOptions = useMemo(
    () => models.map((model) => ({ label: model, value: model })),
    [models],
  );
  const curlModelOptions = modelOptions;
  useEffect(() => {
    if (curlModelOptions.some((option) => option.value === curlModel)) return;
    if (curlModelOptions[0]) setCurlModel(curlModelOptions[0].value);
  }, [curlModel, curlModelOptions]);
  const quickTools = useMemo(() => buildQuickTools(docsLocale), [docsLocale]);
  const ccSwitchConfig = CC_SWITCH_APPS[ccSwitchApp];
  const siteName = settings?.site_name?.trim() || "CodexProxy";
  const defaultCcSwitchName = `${siteName} ${ccSwitchConfig.suffix}`;
  const defaultCherryProviderId = slugProviderId(siteName);
  const showQuickCodexOptions =
    activeToolTab === "codex-cli" || activeToolTab === "cc-switch";

  useEffect(() => {
    if (!ccSwitchNameEdited) setCcSwitchName(defaultCcSwitchName);
  }, [ccSwitchNameEdited, defaultCcSwitchName]);

  useEffect(() => {
    if (!cherryProviderEdited) setCherryProviderId(defaultCherryProviderId);
  }, [cherryProviderEdited, defaultCherryProviderId]);

  useEffect(() => {
    setCcSwitchModels((current) => {
      const preferredCodex = models.includes(quickStartModel)
        ? quickStartModel
        : (models[0] ?? FALLBACK_MODELS[0]);
      const next: Record<string, string> = {};
      ccSwitchConfig.fields.forEach((field) => {
        next[field.key] = modelIncludes(models, current[field.key])
          ? current[field.key]
          : preferredCodex;
      });
      return next;
    });
  }, [ccSwitchConfig, models, quickStartModel]);

  const modelEndpoints = useMemo(
    () => buildEndpointSpecs(quickBaseUrl, docsLocale),
    [quickBaseUrl, docsLocale],
  );
  const adminEndpoints = useMemo(
    () => buildAdminSpecs(quickBaseUrl, docsLocale),
    [quickBaseUrl, docsLocale],
  );

  const copy = useMemo(
    () => (zh: string, en: string) => (docsLocale === "zh" ? zh : en),
    [docsLocale],
  );
  const guides = useMemo(
    () => buildGuides(quickBaseUrl, docsLocale),
    [quickBaseUrl, docsLocale],
  );
  const sections = useMemo(
    () => [
      {
        id: "quick-start",
        label: copy("快速接入", "Quick start"),
        summary: copy(
          "选好客户端，生成配置，完成第一次请求。",
          "Choose a client, generate its configuration and make your first request.",
        ),
        icon: <Sparkles className="size-4" />,
      },
      {
        id: "development",
        label: copy("开发指南", "Development"),
        summary: copy(
          "从地址与认证到 SDK、流式响应和工具调用。",
          "URLs, authentication, SDKs, streaming and tool calls.",
        ),
        icon: <Terminal className="size-4" />,
      },
      {
        id: "model-api",
        label: copy("接口参考", "API reference"),
        summary: copy(
          "按需展开文本、图片与进阶接口，查看参数和示例。",
          "Expand text, image and advanced endpoints for parameters and examples.",
        ),
        icon: <Wand2 className="size-4" />,
      },
      {
        id: "troubleshooting",
        label: copy("故障排查", "Troubleshooting"),
        summary: copy(
          "根据错误码找到原因，明确下一步操作。",
          "Find the cause by error code and choose the next action.",
        ),
        icon: <LifeBuoy className="size-4" />,
      },
      {
        id: "admin-api",
        label: copy("管理与进阶", "Administration"),
        summary: copy(
          "理解密钥范围、额度、凭据与模型映射，查阅常用管理接口。",
          "Key scopes, budgets, credentials, model mappings and common admin endpoints.",
        ),
        icon: <Server className="size-4" />,
      },
    ],
    [copy],
  );
  const entries = useMemo<DocEntry[]>(
    () => [
      ...sections.map((s) => ({
        id: s.id,
        section: s.id,
        title: s.label,
        summary: s.summary,
      })),
      {
        id: "qs-tools",
        section: "quick-start",
        title: copy("客户端配置", "Client configuration"),
        summary: "Codex CLI · CC Switch · Cherry Studio",
      },
      {
        id: "qs-curl",
        section: "quick-start",
        title: copy("cURL 验证", "cURL verification"),
        summary: copy(
          "发送第一个请求并验证响应",
          "Send your first request and verify its response",
        ),
      },
      ...guides.map((g) => ({
        id: g.id,
        section: g.section,
        title: g.title,
        summary: [
          g.summary,
          ...(g.paragraphs || []),
          ...(g.table?.rows.map((r) => r.join(" ")) || []),
        ].join(" "),
      })),
      ...modelEndpoints.map((e) => ({
        id: e.id,
        section: "model-api",
        title: `${e.title} · ${e.path}`,
        summary: [
          e.description,
          ...(e.parameters || []).map((p) => `${p.name} ${p.description}`),
        ].join(" "),
        method: e.method,
      })),
      ...adminEndpoints.map((e) => ({
        id: e.id,
        section: "admin-api",
        title: `${e.title} · ${e.path}`,
        summary: e.description,
        method: e.method,
      })),
    ],
    [sections, guides, modelEndpoints, adminEndpoints, copy],
  );
  const [hash, setHash] = useState(() => window.location.hash);
  const [navigationRevision, setNavigationRevision] = useState(0);
  const [mobileToc, setMobileToc] = useState(false);
  useEffect(() => {
    const navigate = () => {
      setHash(window.location.hash);
      setNavigationRevision((n) => n + 1);
      setMobileToc(false);
    };
    window.addEventListener("hashchange", navigate);
    return () => window.removeEventListener("hashchange", navigate);
  }, []);
  const target = useMemo(
    () => resolveDocsTarget(hash, entries),
    [hash, entries],
  );
  const activeSection = target.section as DocsSection;
  const section = sections.find((s) => s.id === activeSection) || sections[0];
  useEffect(() => {
    if (target.client) setActiveToolTab(target.client as QuickToolTab);
    if (!hash) return;
    const frame = requestAnimationFrame(() =>
      document.getElementById(target.id)?.scrollIntoView({ block: "start" }),
    );
    return () => cancelAnimationFrame(frame);
  }, [target.id, target.client, hash, navigationRevision]);
  const tocItems = useMemo<DocsTOCItem[]>(
    () => [
      {
        id: section.id,
        label: section.label,
        children: entries
          .filter((e) => e.section === activeSection && e.id !== activeSection)
          .map((e) => ({
            id: e.id,
            label: e.method ? e.title.split(" · ")[0] : e.title,
            method: e.method,
          })),
      },
    ],
    [entries, activeSection, section],
  );

  const handleCopyMarkdown = async () => {
    setCopyingMd(true);
    const md = buildDocsMarkdown({
      baseUrl: quickBaseUrl,
      quickTools,
      apiKeyExample: "YOUR_API_KEY",
      clientConfigs: [
        { label: codexConfigPath, lang: "toml", content: codexConfigToml },
        {
          label: codexAuthPath,
          lang: "json",
          content: codexAuthJson.split(activeKey).join("YOUR_API_KEY"),
        },
      ],
      locale: docsLocale,
    });
    try {
      await navigator.clipboard.writeText(md);
      showToast(t("docs.markdownCopied"), "success");
    } catch {
      const ta = document.createElement("textarea");
      ta.value = md;
      ta.style.cssText = "position:fixed;left:-9999px";
      document.body.appendChild(ta);
      ta.select();
      document.execCommand("copy");
      document.body.removeChild(ta);
      showToast(t("docs.markdownCopied"), "success");
    } finally {
      setTimeout(() => setCopyingMd(false), 1200);
    }
  };

  const codexConfigDir =
    codexOs === "windows" ? "%userprofile%\\.codex" : "~/.codex";
  const codexConfigPath =
    codexOs === "windows"
      ? `${codexConfigDir}\\config.toml`
      : `${codexConfigDir}/config.toml`;
  const codexAuthPath =
    codexOs === "windows"
      ? `${codexConfigDir}\\auth.json`
      : `${codexConfigDir}/auth.json`;
  const activeKey = selectedKey || firstKey || "YOUR_API_KEY";

  const codexServiceTierLine =
    quickServiceTier === "default"
      ? ""
      : `\nservice_tier = "${quickServiceTier}"`;
  const codexConfigToml = `model_provider = "OpenAI"
model = "${quickStartModel}"
review_model = "${quickStartModel}"
model_reasoning_effort = "${quickReasoningEffort}"${codexServiceTierLine}
disable_response_storage = true
network_access = "enabled"

[model_providers.OpenAI]
name = "OpenAI"
base_url = "${quickBaseUrl}"
wire_api = "responses"
requires_openai_auth = true

[features]
goals = true`;

  const codexAuthJson = `{
  "OPENAI_API_KEY": "${activeKey}"
}`;

  const responsesCurl = `curl -N -X POST ${quickBaseUrl}/v1/responses \\
  -H "Authorization: Bearer ${activeKey}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${curlModel}",
    "input": [{"role": "user", "content": [{"type": "input_text", "text": "Hello"}]}],
    "stream": true
  }'`;
  const chatCurl = `curl -N -X POST ${quickBaseUrl}/v1/chat/completions \\
  -H "Authorization: Bearer ${activeKey}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${curlModel}",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'`;
  const curlExamples = {
    responses: responsesCurl,
    chat: chatCurl,
  };
  const ccSwitchUrl = buildCCSwitchImportUrl({
    app: ccSwitchApp,
    name: ccSwitchName,
    endpoint: ccSwitchConfig.endpoint(quickBaseUrl),
    apiKey: activeKey,
    models: ccSwitchModels,
    homepage: quickBaseUrl,
    serviceTier: quickServiceTier,
    reasoningEffort: quickReasoningEffort,
  });
  const cherryConfig = `cherrystudio://providers/api-keys?v=1&data=${encodeURIComponent(
    encodeBase64(
      JSON.stringify({
        id: cherryProviderId || defaultCherryProviderId,
        baseUrl: quickBaseUrl,
        apiKey: activeKey,
      }),
    ),
  )}`;
  const hasUsableKey = Boolean(selectedKey || firstKey);
  const handleImportLinkCopied = (name: string) => {
    showToast(t("docs.quickStart.copiedToast", { name }), "success");
  };
  return (
    <div
      className="docs-page"
      onClick={(event) => {
        const anchor = (event.target as Element).closest<HTMLAnchorElement>(
          'a[href^="#"]',
        );
        if (anchor?.getAttribute("href") === window.location.hash)
          setNavigationRevision((n) => n + 1);
      }}
    >
      <header className="docs-hero">
        <div>
          <div className="docs-eyebrow">
            <BookOpen className="size-3.5" />
            CODEX2API / {copy("使用文档", "DOCUMENTATION")}
          </div>
          <h2>{copy("从接入到用好每个接口", "Connect. Build. Go further.")}</h2>
          <p>
            {copy(
              "客户端配置、开发示例与接口参考，都从这里开始。",
              "Client configuration, working examples and API reference, all in one place.",
            )}
          </p>
        </div>
        <div className="docs-hero-actions">
          <Button
            variant="outline"
            size="sm"
            onClick={() => void handleCopyMarkdown()}
            disabled={copyingMd}
            aria-label={t("docs.copyMarkdown")}
            title={t("docs.copyMarkdown")}
          >
            {copyingMd ? (
              <Check className="size-4" />
            ) : (
              <Copy className="size-4" />
            )}
            <span className="docs-export-text">{t("docs.copyMarkdown")}</span>
          </Button>
        </div>
      </header>
      <div className="docs-toolbar">
        <nav
          className="docs-tabs"
          aria-label={copy("文档分类", "Documentation sections")}
        >
          {sections.map((s) => (
            <a
              key={s.id}
              href={`#${s.id}`}
              aria-current={activeSection === s.id ? "page" : undefined}
            >
              {s.icon}
              {s.label}
            </a>
          ))}
        </nav>
        <DocsSearch entries={entries} locale={docsLocale} />
        <Button
          variant="outline"
          size="sm"
          className="docs-mobile-toc"
          onClick={() => setMobileToc(true)}
          aria-label={copy("打开目录", "Open table of contents")}
        >
          <List className="size-4" />
        </Button>
      </div>
      <div className="docs-layout">
        <div className="docs-content">
          <div
            id={section.id}
            className={
              activeSection === "quick-start"
                ? "docs-start-anchor"
                : "docs-section-heading"
            }
          >
            {activeSection !== "quick-start" && (
              <>
                <h3>{section.label}</h3>
                <p>{section.summary}</p>
              </>
            )}
          </div>
          {activeSection === "quick-start" && (
            <>
              <div className="docs-steps">
                <div className="docs-step">
                  <span>01</span>
                  <div>
                    <strong>
                      {copy("准备可用账号", "Prepare an account")}
                    </strong>
                    <p>
                      <a href="/admin/accounts">
                        {copy("账号管理", "Accounts")}
                      </a>
                      <span className="docs-step-detail">
                        {" "}
                        ·{" "}
                        {copy(
                          "确认账号已启用且模型可用。",
                          "Enable an account with the model you need.",
                        )}
                      </span>
                    </p>
                  </div>
                </div>
                <div className="docs-step">
                  <span>02</span>
                  <div>
                    <strong>
                      {copy("选择客户端密钥", "Choose a client key")}
                    </strong>
                    <p>
                      <a href="/admin/api-keys">
                        {copy("API 密钥", "API keys")}
                      </a>
                      <span className="docs-step-detail">
                        {" "}
                        ·{" "}
                        {copy(
                          "为客户端创建独立访问凭据。",
                          "Create a dedicated key for your client.",
                        )}
                      </span>
                    </p>
                  </div>
                </div>
                <div className="docs-step">
                  <span>03</span>
                  <div>
                    <strong>{copy("配置并验证", "Configure & verify")}</strong>
                    <p>
                      <a href="#qs-curl">
                        {copy("发送验证请求", "Send a test request")}
                      </a>
                      <span className="docs-step-detail">
                        {" "}
                        ·{" "}
                        {copy(
                          "在使用统计确认结果。",
                          "Check the result in Usage.",
                        )}
                      </span>
                    </p>
                  </div>
                </div>
              </div>
              <Card id="qs-tools" className="mb-4 scroll-mt-20 py-0">
                <CardContent className="p-5">
                  <div className="mb-3 flex flex-wrap items-end justify-between gap-3">
                    <div className="min-w-0">
                      <h3 className="text-[15px] font-semibold text-foreground">
                        {copy("生成客户端配置", "Configure your client")}
                      </h3>
                      <p className="mt-0.5 text-[12.5px] text-muted-foreground">
                        {copy(
                          "选择工具后，复制对应配置或一键导入。",
                          "Choose a tool, then copy its configuration or import it.",
                        )}
                      </p>
                    </div>
                    <div className="flex shrink-0 items-center gap-2">
                      {allKeys.length > 0 ? (
                        <>
                          <span className="text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
                            {t("docs.quickStart.useKey")}
                          </span>
                          <Select
                            compact
                            className="w-44"
                            value={selectedKey}
                            onValueChange={setSelectedKey}
                            options={allKeys.map((k) => ({
                              label: k.name
                                ? `${k.name} · ${k.key.slice(0, 6)}…${k.key.slice(-4)}`
                                : k.key,
                              value: k.key,
                            }))}
                          />
                        </>
                      ) : (
                        <a
                          href="/admin/api-keys"
                          className="inline-flex items-center gap-1 rounded-md border border-amber-500/30 bg-amber-500/10 px-2.5 py-1 text-[11px] font-bold text-amber-600 dark:text-amber-400"
                        >
                          {t("docs.quickStart.createKeyFirst")}
                        </a>
                      )}
                    </div>
                  </div>
                  <div className="mb-3 rounded-xl border border-border bg-muted/25 p-1">
                    <div className="docs-client-tabs">
                      {[
                        { value: "codex-cli", label: "Codex CLI" },
                        { value: "cc-switch", label: "CC Switch" },
                        { value: "cherry-studio", label: "Cherry Studio" },
                      ].map((tab) => (
                        <ToolTabButton
                          key={tab.value}
                          selected={activeToolTab === tab.value}
                          id={tab.value as QuickToolTab}
                          label={tab.label}
                          onClick={() =>
                            setActiveToolTab(tab.value as QuickToolTab)
                          }
                        />
                      ))}
                    </div>
                  </div>
                  <div className="docs-config-fields">
                    <FieldBox label={t("docs.clientConfig.endpointLabel")}>
                      <input
                        className={`${FIELD_INPUT} font-mono`}
                        value={
                          activeToolTab === "cc-switch"
                            ? ccSwitchConfig.endpoint(quickBaseUrl)
                            : quickBaseUrl
                        }
                        onChange={(event) => {
                          const value = event.target.value;
                          if (
                            activeToolTab === "cc-switch" &&
                            value.endsWith("/v1")
                          ) {
                            setQuickBaseUrl(value.slice(0, -3));
                          } else {
                            setQuickBaseUrl(value);
                          }
                        }}
                      />
                    </FieldBox>
                    <FieldBox label={t("docs.clientConfig.defaultModel")}>
                      <Select
                        compact
                        value={quickStartModel}
                        onValueChange={setQuickStartModel}
                        options={modelOptions}
                      />
                    </FieldBox>
                  </div>
                  {showQuickCodexOptions && (
                    <details className="docs-advanced">
                      <summary>
                        {copy(
                          "高级选项 · 思考强度与服务档位",
                          "Advanced · reasoning effort & service tier",
                        )}
                      </summary>
                      <div>
                        {showQuickCodexOptions ? (
                          <FieldBox
                            label={t("docs.clientConfig.reasoningEffort")}
                          >
                            <Select
                              compact
                              value={quickReasoningEffort}
                              onValueChange={(value) =>
                                setQuickReasoningEffort(
                                  value as QuickReasoningEffort,
                                )
                              }
                              options={[
                                { label: "None", value: "none" },
                                { label: "Minimal", value: "minimal" },
                                { label: "Low", value: "low" },
                                { label: "Medium", value: "medium" },
                                { label: "High", value: "high" },
                                { label: "xhigh", value: "xhigh" },
                                { label: "ultra", value: "ultra" },
                              ]}
                            />
                          </FieldBox>
                        ) : null}
                        {showQuickCodexOptions ? (
                          <FieldBox label={t("docs.clientConfig.fastMode")}>
                            <Select
                              compact
                              value={quickServiceTier}
                              onValueChange={(value) =>
                                setQuickServiceTier(value as QuickServiceTier)
                              }
                              options={[
                                {
                                  label: t("docs.clientConfig.fastModeDefault"),
                                  value: "default",
                                },
                                {
                                  label: t("docs.clientConfig.fastModeEnabled"),
                                  value: "fast",
                                },
                                { label: "Ultrafast", value: "ultrafast" },
                              ]}
                            />
                          </FieldBox>
                        ) : null}
                      </div>
                    </details>
                  )}
                  <div className="space-y-4">
                    {activeToolTab === "codex-cli" && (
                      <>
                        <OsTabs active={codexOs} onChange={setCodexOs} />
                        <p className="docs-callout">
                          {t("docs.clientConfig.codexConfigHint")} ·{" "}
                          {codexOs === "windows"
                            ? t("docs.clientConfig.codexNoteWindows")
                            : t("docs.clientConfig.codexNoteUnix")}
                        </p>
                        <CodeBlock
                          label={codexConfigPath}
                          content={codexConfigToml}
                          lang="toml"
                        />
                        <CodeBlock
                          label={codexAuthPath}
                          content={codexAuthJson}
                          lang="json"
                        />
                      </>
                    )}
                    {activeToolTab === "cc-switch" && (
                      <>
                        <div className={`${CONFIG_PANEL} md:grid-cols-2`}>
                          <FieldBox label={t("docs.clientConfig.importTarget")}>
                            <code
                              className={`${FIELD_INPUT} flex items-center truncate font-mono`}
                            >
                              {ccSwitchConfig.label}
                            </code>
                          </FieldBox>
                          <FieldBox label={t("docs.clientConfig.configName")}>
                            <input
                              className={FIELD_INPUT}
                              value={ccSwitchName}
                              onChange={(event) => {
                                setCcSwitchNameEdited(true);
                                setCcSwitchName(event.target.value);
                              }}
                            />
                          </FieldBox>
                          {ccSwitchConfig.fields.map((field) => (
                            <FieldBox
                              key={field.key}
                              label={t(
                                `docs.clientConfig.ccSwitchFields.${field.key}`,
                              )}
                            >
                              <div className="space-y-1.5">
                                <Select
                                  compact
                                  value={ccSwitchModels[field.key] || ""}
                                  onValueChange={(value) =>
                                    setCcSwitchModels((current) => ({
                                      ...current,
                                      [field.key]: value,
                                    }))
                                  }
                                  options={modelOptions}
                                />
                              </div>
                            </FieldBox>
                          ))}
                        </div>
                        <ImportPreviewCard
                          title={t("docs.clientConfig.ccSwitchPreviewTitle")}
                          description={t(
                            "docs.clientConfig.ccSwitchPreviewDesc",
                          )}
                          icon={<ClientIcon id="cc-switch" size={20} />}
                          link={ccSwitchUrl}
                          disabled={!hasUsableKey}
                          onCopied={() => handleImportLinkCopied("CC Switch")}
                          onLaunch={() => {
                            window.open(ccSwitchUrl, "_blank");
                            showToast(
                              t("docs.quickStart.launchedToast", {
                                name: "CC Switch",
                              }),
                              "success",
                            );
                          }}
                        />
                      </>
                    )}
                    {activeToolTab === "cherry-studio" && (
                      <>
                        <div className={`${CONFIG_PANEL} md:grid-cols-2`}>
                          <FieldBox label={t("docs.clientConfig.importTarget")}>
                            <code
                              className={`${FIELD_INPUT} flex items-center truncate font-mono`}
                            >
                              Cherry Studio
                            </code>
                          </FieldBox>
                          <FieldBox label={t("docs.clientConfig.providerId")}>
                            <input
                              className={`${FIELD_INPUT} font-mono`}
                              value={cherryProviderId}
                              onChange={(event) => {
                                setCherryProviderEdited(true);
                                setCherryProviderId(event.target.value);
                              }}
                            />
                          </FieldBox>
                        </div>
                        <ImportPreviewCard
                          title={t("docs.clientConfig.cherryPreviewTitle")}
                          description={t("docs.clientConfig.cherryPreviewDesc")}
                          icon={<ClientIcon id="cherry-studio" size={20} />}
                          link={cherryConfig}
                          disabled={!hasUsableKey}
                          onCopied={() =>
                            handleImportLinkCopied("Cherry Studio")
                          }
                          onLaunch={() => {
                            window.open(cherryConfig, "_blank");
                            showToast(
                              t("docs.quickStart.launchedToast", {
                                name: "Cherry Studio",
                              }),
                              "success",
                            );
                          }}
                        />
                      </>
                    )}
                  </div>
                </CardContent>
              </Card>

              <Card id="qs-curl" className="mb-4 scroll-mt-20 py-0">
                <CardContent className="p-6">
                  <div className="mb-4 flex flex-wrap items-start justify-between gap-3">
                    <div className="min-w-0">
                      <h3 className="text-base font-semibold text-foreground mb-1">
                        {t("docs.quickStart.curlTitle")}
                      </h3>
                      <p className="text-sm text-muted-foreground">
                        {copy(
                          "使用当前选中的密钥与模型验证。Responses / Chat 返回 SSE。",
                          "Verify with the selected key and model. Responses / Chat return SSE.",
                        )}
                      </p>
                    </div>
                    <Select
                      compact
                      className="w-52"
                      value={curlModel}
                      onValueChange={setCurlModel}
                      options={curlModelOptions}
                    />
                  </div>
                  <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
                    <UnderlineTabs
                      active={activeCurl}
                      onChange={setActiveCurl}
                      tabs={[
                        {
                          value: "responses",
                          label: "Responses",
                          hint: "/v1/responses",
                        },
                        {
                          value: "chat",
                          label: "Chat",
                          hint: "/v1/chat/completions",
                        },
                      ]}
                    />
                    <code className="code-inline text-[11px]">
                      {activeCurl === "responses"
                        ? "/v1/responses"
                        : "/v1/chat/completions"}
                    </code>
                  </div>
                  <CodeBlock
                    label="cURL"
                    content={curlExamples[activeCurl]}
                    lang="bash"
                  />
                </CardContent>
              </Card>

              <p className="docs-callout">
                {copy(
                  "看到完整结束事件或成功 JSON 后，在使用统计确认最终模型与用量。健康检查成功仅代表服务在线。",
                  "After a terminal event or successful JSON response, verify the final model and usage in Usage. A healthy service alone does not prove generation works.",
                )}{" "}
                <a href="#connection-check" className="text-primary">
                  {copy("连接排查 →", "Connection checks →")}
                </a>
              </p>
            </>
          )}
          {guides
            .filter((g) => g.section === activeSection)
            .map((guide) => (
              <GuideArticle
                key={`${guide.id}-${target.id === guide.id ? navigationRevision : 0}`}
                guide={guide}
                locale={docsLocale}
                activeId={target.id}
              />
            ))}
          {activeSection === "model-api" &&
            (["text", "media", "advanced"] as const).map((category) => (
              <section key={category}>
                <h4 className="docs-group-title">
                  {category === "text"
                    ? copy("文本与基础接口", "Text & essentials")
                    : category === "media"
                      ? copy("图片", "Images")
                      : copy("进阶与兼容接口", "Advanced & compatibility")}
                  <span>
                    {
                      modelEndpoints.filter((e) => e.category === category)
                        .length
                    }
                  </span>
                </h4>
                {modelEndpoints
                  .filter((e) => e.category === category)
                  .map((endpoint) => (
                    <EndpointDoc
                      key={`${endpoint.id}-${target.id === endpoint.id ? navigationRevision : 0}`}
                      endpoint={endpoint}
                      activeId={target.id}
                      apiKey={activeKey}
                      baseUrl={quickBaseUrl}
                      allKeys={allKeys}
                    />
                  ))}
              </section>
            ))}
          {activeSection === "admin-api" && (
            <>
              <h4 className="docs-group-title">
                {copy("常用管理接口", "Common admin endpoints")}
                <span>{adminEndpoints.length}</span>
              </h4>
              {adminEndpoints.map((endpoint) => (
                <EndpointDoc
                  key={`${endpoint.id}-${target.id === endpoint.id ? navigationRevision : 0}`}
                  endpoint={endpoint}
                  activeId={target.id}
                  apiKey={adminSeed}
                  baseUrl={quickBaseUrl}
                />
              ))}
            </>
          )}
        </div>
        <aside className="docs-sidebar">
          <DocsTOC
            items={tocItems}
            title={copy("本页目录", "On this page")}
            activeId={target.id}
          />
        </aside>
      </div>
      <Dialog open={mobileToc} onOpenChange={setMobileToc}>
        <DialogContent className="max-h-[80dvh]" aria-describedby={undefined}>
          <DialogTitle>{copy("文档目录", "Contents")}</DialogTitle>
          <DocsTOC
            items={tocItems}
            title={section.label}
            activeId={target.id}
            onNavigate={() => setMobileToc(false)}
          />
        </DialogContent>
      </Dialog>
    </div>
  );
}
