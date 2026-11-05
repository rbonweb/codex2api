import { useTranslation } from "react-i18next";
import { AlertTriangle, Globe, Plus } from "lucide-react";

import {
  formatProxyDisplayURL,
  formatProxyEgressSummary,
  formatProxyShortName,
  resolveAccountProxyBinding,
  type AccountProxyBindingKind,
  type ProxyBindingAccount,
  type ProxyBindingContext,
} from "../lib/accountProxyBinding";
import { cn } from "@/lib/utils";

// Proxy badge in the account list: shows at a glance which egress this account goes through, and whether it no longer has an egress at all.
// All the verdicts live in lib/accountProxyBinding; this component only handles colors and copy.
const TONE: Record<AccountProxyBindingKind, string> = {
  resin:
    "bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-300 dark:ring-emerald-400/20",
  bound:
    "bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-950 dark:text-sky-300 dark:ring-sky-400/20",
  bound_custom:
    "bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-400 dark:ring-amber-400/20",
  bound_unusable:
    "bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-300 dark:ring-rose-400/20",
  direct_blocked:
    "bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-300 dark:ring-rose-400/20",
  group:
    "bg-violet-50 text-violet-700 ring-violet-600/20 dark:bg-violet-950 dark:text-violet-400 dark:ring-violet-400/20",
  pool: "bg-muted/60 text-muted-foreground ring-border",
  global: "bg-muted/60 text-muted-foreground ring-border",
  direct: "text-muted-foreground ring-transparent",
};

interface AccountProxyBadgeProps {
  account: ProxyBindingAccount | null | undefined;
  ctx: ProxyBindingContext;
  onClick?: () => void;
  className?: string;
}

export default function AccountProxyBadge({
  account,
  ctx,
  onClick,
  className,
}: AccountProxyBadgeProps) {
  const { t } = useTranslation();
  const binding = resolveAccountProxyBinding(account, ctx);
  const shortName = formatProxyShortName(binding.proxy, binding.url);
  const egress = formatProxyEgressSummary(binding.proxy);
  const latency = binding.proxy?.test_latency_ms ?? 0;
  // Always strip credentials from the URL in the tooltip: hovering over the list is not an action for "I want to see the credentials".
  const safeURL = formatProxyDisplayURL(binding.url);

  let text: string;
  let tip: string;
  switch (binding.kind) {
    case "resin":
      text = t("accounts.proxyBadgeResin");
      tip = binding.url
        ? t("accounts.proxyTipResinOverride", { url: safeURL })
        : t("accounts.proxyTipResin");
      break;
    case "bound":
      text =
        binding.proxy?.test_status === "success" && latency > 0
          ? `${shortName} · ${latency}ms`
          : shortName;
      tip = [t("accounts.proxyTipBound", { url: safeURL }), egress]
        .filter(Boolean)
        .join("\n");
      break;
    case "bound_custom":
      text = shortName;
      tip = t("accounts.proxyTipCustom", { url: safeURL });
      break;
    case "bound_unusable":
      text = t("accounts.proxyBadgeUnusable");
      tip = t("accounts.proxyTipUnusable", { url: safeURL });
      break;
    case "group":
      text = t("accounts.proxyBadgeGroup", { name: binding.groupName });
      tip = t("accounts.proxyTipGroup", { name: binding.groupName });
      break;
    case "pool":
      text = t("accounts.proxyBadgePool");
      // Use total rather than count: i18next looks for the _one/_other plural keys first when it sees count.
      tip = t("accounts.proxyTipPool", { total: ctx.poolSize });
      break;
    case "global":
      text = t("accounts.proxyBadgeGlobal");
      tip = t("accounts.proxyTipGlobal", {
        url: formatProxyDisplayURL(ctx.globalProxy),
      });
      break;
    case "direct_blocked":
      text = t("accounts.proxyBadgeBlocked");
      tip = t("accounts.proxyTipBlocked");
      break;
    default:
      text = t("accounts.proxyBadgeDirect");
      tip = t("accounts.proxyTipDirect");
      break;
  }

  const title = onClick ? `${tip}\n${t("accounts.proxyTipEdit")}` : tip;
  const Icon =
    binding.kind === "bound_unusable" || binding.kind === "direct_blocked"
      ? AlertTriangle
      : binding.kind === "direct"
        ? Plus
        : Globe;

  const body = (
    <span
      className={cn(
        "inline-flex min-w-0 max-w-full items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-semibold ring-1 ring-inset",
        binding.kind === "direct" && "border border-dashed border-border",
        TONE[binding.kind],
      )}
    >
      <Icon className="size-2.5 shrink-0" />
      <span className="truncate">{text}</span>
    </span>
  );

  if (!onClick) {
    return (
      <span className={cn("inline-flex min-w-0 max-w-full", className)} title={title}>
        {body}
      </span>
    );
  }

  return (
    <button
      type="button"
      className={cn(
        "inline-flex min-w-0 max-w-full text-left transition-opacity hover:opacity-80",
        className,
      )}
      title={title}
      onClick={(event) => {
        event.stopPropagation();
        onClick();
      }}
    >
      {body}
    </button>
  );
}
