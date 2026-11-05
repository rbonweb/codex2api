import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { X, Zap } from "lucide-react";

import { api } from "../api";
import type { ProxyRow } from "../api";
import { ProxyPoolSelect } from "./ProxyPoolSelect";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";
import { cn } from "@/lib/utils";

// ProxyUrlInput is a proxy URL input with a built-in clear button. It is shared by the proxy fields of every channel's account dialog;
// typing a URL and picking one from the proxy pool write the same value, and the X clears both in one click.
export function ProxyUrlInput({
  value,
  onChange,
  placeholder,
  disabled = false,
  className,
}: {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  disabled?: boolean;
  className?: string;
}) {
  const { t } = useTranslation();
  return (
    <div className={cn("relative", className)}>
      <Input
        className={value ? "w-full pr-8" : "w-full"}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        disabled={disabled}
      />
      {value && !disabled ? (
        <button
          type="button"
          onClick={() => onChange("")}
          title={t("accounts.clearProxy")}
          aria-label={t("accounts.clearProxy")}
          className="absolute inset-y-0 right-0 flex w-8 items-center justify-center rounded-r-md text-muted-foreground transition-colors hover:text-foreground"
        >
          <X className="size-3.5" />
        </button>
      ) : null}
    </div>
  );
}

// ProxyField is the only proxy selection field in the account form:
//   Row 1: type a proxy URL manually + a "Test" button (calls /proxies/test to verify connectivity and exit location)
//   Row 2: pick from the proxy pool dropdown (shows location and bound count, idle first; when the pool is empty, a disabled placeholder with guidance)
//   Row 3: when the current value matches a pool node, a hint about the link: accounts and nodes are linked by URL,
//          so editing a node's address on the Proxies page updates every bound account, and deleting it unbinds them automatically.
// The add/edit dialogs of every channel must use it; do not build per-page copies that make the experience inconsistent.
export function ProxyField({
  value,
  onChange,
  proxies,
  label,
  labelClassName,
  linkedHint,
  placeholder = "socks5://user:pass@host:port",
  disabled = false,
  className,
}: {
  value: string;
  onChange: (value: string) => void;
  proxies: ProxyRow[];
  /** Field title; pass an empty string to render no title (the caller places it). */
  label?: string;
  /** Title style override, to match the title style of neighbouring fields in the dialog. */
  labelClassName?: string;
  /** Override the association hint for fields that copy a URL without binding. */
  linkedHint?: string;
  placeholder?: string;
  disabled?: boolean;
  className?: string;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [testing, setTesting] = useState(false);

  const trimmed = value.trim();
  // Whether the current value matches a proxy pool node: if so it is "linked", and the user is told that editing the node cascades.
  const linked = useMemo(
    () => (trimmed ? proxies.find((p) => p.url === trimmed) : undefined),
    [proxies, trimmed],
  );

  const testProxy = async () => {
    const url = trimmed;
    if (!url) {
      showToast(t("accounts.proxyUrlRequired"), "error");
      return;
    }
    setTesting(true);
    try {
      const res = await api.testProxy(url, undefined, "en");
      if (!res.success) {
        showToast(
          t("accounts.proxyTestFailed", { error: res.error || t("accounts.proxyTestUnknownError") }),
          "error",
        );
        return;
      }
      const location = res.location || [res.country, res.region, res.city].filter(Boolean).join(" ");
      showToast(
        t("accounts.proxyTestSuccess", {
          ip: res.ip || "-",
          location: location || "-",
          latency: res.latency_ms ?? 0,
        }),
        "success",
      );
    } catch (error) {
      showToast(t("accounts.proxyTestFailed", { error: getErrorMessage(error) }), "error");
    } finally {
      setTesting(false);
    }
  };

  const title = label ?? t("accounts.proxyUrl");

  return (
    <div className={cn("space-y-2", className)}>
      {title ? (
        <span className={cn("block text-xs font-semibold text-muted-foreground", labelClassName)}>{title}</span>
      ) : null}
      <div className="flex flex-col gap-2 sm:flex-row sm:items-stretch">
        <ProxyUrlInput
          className="min-w-0 flex-1"
          value={value}
          onChange={onChange}
          placeholder={placeholder}
          disabled={disabled}
        />
        <Button
          type="button"
          variant="outline"
          className="shrink-0 justify-center gap-1.5 sm:min-w-[104px]"
          disabled={disabled || testing || !trimmed}
          onClick={() => void testProxy()}
        >
          <Zap className={`size-3.5 ${testing ? "animate-pulse" : ""}`} />
          {testing ? t("accounts.testingProxy") : t("accounts.testProxy")}
        </Button>
      </div>
      {proxies.length > 0 ? (
        <ProxyPoolSelect className="w-full" proxies={proxies} value={value} onSelect={onChange} disabled={disabled} />
      ) : (
        // When the pool is empty, still show a disabled placeholder dropdown + guidance so "pick from the proxy pool" is always visible,
        // and users do not assume the feature is missing (pool entries come from the Proxies page).
        <div className="flex h-9 w-full items-center justify-between rounded-md border border-dashed border-input bg-muted/20 px-2.5 text-xs text-muted-foreground/70">
          <span>{t("accounts.proxyPoolEmpty")}</span>
          <span className="opacity-60">▾</span>
        </div>
      )}
      {linked ? (
        <p className="text-[11px] leading-relaxed text-muted-foreground">{linkedHint ?? t("accounts.proxyPoolLinkedHint")}</p>
      ) : null}
    </div>
  );
}
