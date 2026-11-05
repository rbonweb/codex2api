import { useEffect, useState, type MouseEvent } from "react";
import { formatBeijingTime, getTimezone } from "../utils/time";
import { useTranslation } from "react-i18next";
import { CloudOff, RefreshCw } from "lucide-react";
import { api, AdminAPIError } from "../api";
import { useToast } from "../hooks/useToast";
import { cn } from "@/lib/utils";
import type { SubscriptionRefreshResponse, SubscriptionStatus } from "../types";

// SubscriptionBadge is the subscription status badge (shared by the accounts page).
// It only consumes the subscription object that the server computes in the business timezone, and no longer estimates days locally at 24-hour granularity.
// "due today" / "expired", to avoid timezone mismatches between frontend and backend. The business status decides the text and color; the sync status
// (failed/pending) only adds a marker and does not change the meaning of the text. Every status has text, not only color.
//
// Display rules:
//   active          expiry/renewal date (>9 days green, 4-9 days yellow, ≤3 days red)
//   expiring_today  expires today (orange)
//   expired         overdue by N days (red)
//   grace_period    grace period · overdue by N days (orange)
//   unknown+pending renewed · pending confirmation (blue; the stale expiry time was just cleared, waiting for the authoritative sync)
//   unknown         status unknown (gray; shown only when canRefresh is set, to give the user a manual lookup entry point)
//   sync=failed     a muted "cloud disconnected" icon is placed before the badge (not a warning triangle, to avoid being misread as an account error) + on hover, "data may be out of date"

export interface SubscriptionBadgeProps {
  accountId: number;
  subscription?: SubscriptionStatus;
  /** Whether to show a "Refresh subscription status" button (only Codex/ChatGPT accounts have a subscription provider that can be queried). */
  canRefresh?: boolean;
  className?: string;
}

type Tone = "neutral" | "amber" | "orange" | "red" | "blue" | "gray";

const TONE_CLASS: Record<Tone, string> = {
  neutral:
    "bg-emerald-50 text-emerald-700 ring-emerald-500/20 dark:bg-emerald-500/10 dark:text-emerald-300 dark:ring-emerald-400/20",
  amber:
    "bg-amber-100 text-amber-700 ring-amber-500/30 dark:bg-amber-500/20 dark:text-amber-300 dark:ring-amber-400/30",
  orange:
    "bg-orange-100 text-orange-700 ring-orange-500/30 dark:bg-orange-500/20 dark:text-orange-300 dark:ring-orange-400/30",
  red: "bg-red-100 text-red-700 ring-red-500/30 dark:bg-red-500/20 dark:text-red-300 dark:ring-red-400/30",
  blue: "bg-sky-100 text-sky-700 ring-sky-500/30 dark:bg-sky-500/20 dark:text-sky-300 dark:ring-sky-400/30",
  gray: "bg-zinc-200 text-zinc-700 ring-zinc-400/30 dark:bg-zinc-700/50 dark:text-zinc-300 dark:ring-zinc-500/30",
};

function formatDateTime(iso: string | undefined, locale: string): string {
  if (!iso) return "-";
  const ts = Date.parse(iso);
  if (Number.isNaN(ts)) return iso;
  return new Date(ts).toLocaleString(locale, {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    timeZone: getTimezone(),
  });
}

/** YYYY-MM-DD in the display timezone (the badge only shows the date; the exact moment goes in the hover tooltip). */
function formatDate(iso: string | undefined): string {
  if (!iso) return "-";
  if (Number.isNaN(Date.parse(iso))) return iso;
  return formatBeijingTime(iso).slice(0, 10);
}

/** Decides the badge text and color; returns null when this account does not need a badge. */
export function describeSubscription(
  sub: SubscriptionStatus | undefined,
  t: (key: string, opts?: Record<string, unknown>) => string,
  canRefresh: boolean,
): { label: string; tone: Tone; emphasis?: boolean } | null {
  if (!sub) return null;
  const pendingRenewal = sub.sync_state === "pending" && !!sub.renewal_detected_at;
  switch (sub.business_status) {
    case "active": {
      const days = sub.days_remaining;
      return {
        label: t("accounts.subscriptionStatusActive", { date: formatDate(sub.expires_at) }),
        tone: days <= 3 ? "red" : days <= 9 ? "amber" : "neutral",
      };
    }
    case "expiring_today":
      return { label: t("accounts.subscriptionStatusExpiringToday"), tone: "orange", emphasis: true };
    case "expired":
      return { label: t("accounts.subscriptionStatusExpired", { days: sub.days_overdue }), tone: "red", emphasis: true };
    case "grace_period":
      return { label: t("accounts.subscriptionStatusGrace", { days: sub.days_overdue }), tone: "orange" };
    default:
      if (pendingRenewal) {
        return { label: t("accounts.subscriptionStatusPendingRenewal"), tone: "blue" };
      }
      if (sub.sync_state === "pending") {
        return { label: t("accounts.subscriptionStatusPending"), tone: "blue" };
      }
      // No expiry information has ever been received: only offer an entry point on pages where it can be refreshed manually, and do not fill pages without a data source with a row of "unknown".
      if (!canRefresh) return null;
      return { label: t("accounts.subscriptionStatusUnknown"), tone: "gray" };
  }
}

function sourceLabel(source: string | undefined, t: (key: string) => string): string {
  switch (source) {
    case "provider_api":
      return t("accounts.subscriptionSourceProviderApi");
    case "plan_header":
      return t("accounts.subscriptionSourcePlanHeader");
    case "jwt":
      return t("accounts.subscriptionSourceJwt");
    default:
      return source || "-";
  }
}

function enumLabel(prefix: string, value: string | undefined, t: (key: string) => string): string {
  if (!value) return "-";
  const key = `accounts.${prefix}${value.charAt(0).toUpperCase()}${value.slice(1)}`;
  const label = t(key);
  return label === key ? value : label;
}

function statusLabel(sub: SubscriptionStatus, status: string | undefined, t: (key: string, opts?: Record<string, unknown>) => string): string {
  switch (status) {
    case "active":
      return t("accounts.subscriptionStatusActive", { date: formatDate(sub.expires_at) });
    case "expiring_today":
      return t("accounts.subscriptionStatusExpiringToday");
    case "expired":
      return t("accounts.subscriptionStatusExpired", { days: sub.days_overdue });
    case "grace_period":
      return t("accounts.subscriptionStatusGrace", { days: sub.days_overdue });
    default:
      return t("accounts.subscriptionStatusUnknown");
  }
}

export function buildSubscriptionTooltip(
  sub: SubscriptionStatus,
  t: (key: string, opts?: Record<string, unknown>) => string,
  locale: string,
): string {
  const lines: string[] = [];
  if (sub.expires_at) lines.push(t("accounts.subscriptionTipExpiresAt", { value: formatDateTime(sub.expires_at, locale) }));
  if (sub.grace_until) lines.push(t("accounts.subscriptionTipGraceUntil", { value: formatDateTime(sub.grace_until, locale) }));
  lines.push(
    sub.last_checked_at
      ? t("accounts.subscriptionTipLastChecked", { value: formatDateTime(sub.last_checked_at, locale) })
      : t("accounts.subscriptionTipNeverChecked"),
  );
  lines.push(t("accounts.subscriptionTipSync", { value: enumLabel("subscriptionSync", sub.sync_state, t) }));
  if (sub.source) lines.push(t("accounts.subscriptionTipSource", { value: sourceLabel(sub.source, t) }));
  lines.push(t("accounts.subscriptionTipAutoRenew", { value: enumLabel("subscriptionAutoRenew", sub.auto_renew, t) }));
  if (sub.business_status === "unknown" && sub.last_known_status) {
    lines.push(t("accounts.subscriptionTipLastKnown", { value: statusLabel(sub, sub.last_known_status, t) }));
  }
  if (sub.renewal_detected_at) {
    lines.push(t("accounts.subscriptionTipRenewalDetected", { value: formatDateTime(sub.renewal_detected_at, locale) }));
  }
  if (sub.error) {
    lines.push(
      t("accounts.subscriptionTipError", {
        value: sub.error.includes("anti-bot challenge") ? t("accounts.subscriptionRefreshBlocked") : sub.error,
      }),
    );
  }
  if (sub.timezone) lines.push(t("accounts.subscriptionTipTimezone", { value: sub.timezone }));
  return lines.join("\n");
}

export default function SubscriptionBadge({ accountId, subscription, canRefresh = false, className }: SubscriptionBadgeProps) {
  const { t, i18n } = useTranslation();
  const { showToast } = useToast();
  // Refresh results land in a local overlay first, so the new status shows right away even before the whole list reloads; once the parent data updates, the parent wins.
  const [override, setOverride] = useState<SubscriptionStatus | undefined>(undefined);
  const [refreshing, setRefreshing] = useState(false);
  useEffect(() => {
    setOverride(undefined);
  }, [subscription]);

  const sub = override ?? subscription;
  const described = describeSubscription(sub, t, canRefresh);
  if (!sub || !described) return null;

  const stale = sub.sync_state === "failed";
  const tooltip = buildSubscriptionTooltip(sub, t, i18n.language);

  const handleRefresh = async (event: MouseEvent<HTMLButtonElement>) => {
    event.preventDefault();
    event.stopPropagation();
    if (refreshing) return;
    setRefreshing(true);
    try {
      const res: SubscriptionRefreshResponse = await api.refreshAccountSubscription(accountId);
      if (res.subscription) setOverride(res.subscription);
      switch (res.outcome) {
        case "updated":
          showToast(
            t("accounts.subscriptionRefreshUpdated", {
              date: formatDateTime(res.subscription?.expires_at ?? res.subscription_expires_at, i18n.language),
            }),
            "success",
          );
          break;
        case "unchanged":
          showToast(t("accounts.subscriptionRefreshUnchanged"), "info");
          break;
        case "no_subscription":
          showToast(t("accounts.subscriptionRefreshNoSubscription"), "info");
          break;
        case "unsupported":
          showToast(t("accounts.subscriptionRefreshUnsupported"), "info");
          break;
        default:
          if (res.error && res.error.includes("anti-bot challenge")) {
            showToast(t("accounts.subscriptionRefreshBlocked"), "warning", 8000);
          } else {
            showToast(t("accounts.subscriptionRefreshFailed", { error: res.error || res.outcome }), "error");
          }
      }
    } catch (err) {
      if (err instanceof AdminAPIError && err.status === 429) {
        const match = /(\d+)/.exec(err.message);
        showToast(t("accounts.subscriptionRefreshRateLimited", { seconds: match ? match[1] : 30 }), "info");
      } else {
        showToast(t("accounts.subscriptionRefreshFailed", { error: err instanceof Error ? err.message : String(err) }), "error");
      }
    } finally {
      setRefreshing(false);
    }
  };

  return (
    <span
      className={cn(
        "inline-flex max-w-full items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] ring-1 ring-inset",
        described.emphasis ? "font-semibold" : "font-medium",
        TONE_CLASS[described.tone],
        className,
      )}
      title={tooltip}
      data-subscription-status={sub.business_status}
      data-subscription-sync={sub.sync_state}
    >
      {stale && <CloudOff className="size-3 shrink-0 opacity-60" aria-label={t("accounts.subscriptionStale")} />}
      <span className="truncate">{described.label}</span>
      {stale && <span className="sr-only">{t("accounts.subscriptionStale")}</span>}
      {canRefresh && (
        <button
          type="button"
          onClick={handleRefresh}
          disabled={refreshing}
          className="ml-0.5 inline-flex shrink-0 items-center rounded p-0.5 opacity-70 transition hover:opacity-100 focus-visible:opacity-100 disabled:cursor-wait"
          title={refreshing ? t("accounts.subscriptionRefreshing") : t("accounts.subscriptionRefresh")}
          aria-label={t("accounts.subscriptionRefresh")}
        >
          <RefreshCw className={cn("size-3", refreshing && "animate-spin")} />
        </button>
      )}
    </span>
  );
}
