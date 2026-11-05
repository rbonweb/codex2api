// Account proxy binding resolution: maps the priority of the backend resolveProxyForAccountSnapshot
// (account self-bound > group > proxy pool > global > direct) into one status the list badge can show.
// Resin is a full-layer override on top of this chain (issue #679): when enabled, Codex accounts always report resin,
// and an account's self-bound URL is only mentioned in the badge tooltip as "overridden" and no longer takes part in the decision.
//
// Two deliberate boundaries:
//   1. The inherited state reports only the source, not a specific URL. Both within the pool and within the group, selection is sticky hashing by account ID,
//      so a frontend copy of the hash would inevitably drift from the scheduler; to get the exact effective URL, the backend must send it in the list projection.
//   2. URLs are only trimmed, with no other normalization. The backend buildProxyPoolSet does an exact match after trimming,
//      so both sides must be identical; any "clever" normalization would create false mismatches.

export type AccountProxyBindingKind =
  // Resin is enabled: all outbound traffic for this account goes through Resin, and the layers below do not take part for now
  | "resin"
  // Account self-bound to a healthy entry in the proxy pool
  | "bound"
  // Account self-bound to a managed proxy, but that entry is disabled, failed its test or was deleted; with the proxy pool on, there is no usable egress
  | "bound_unusable"
  // Account self-bound to a URL that is not part of the proxy pool; unaffected by the pool being on or off
  | "bound_custom"
  // Not self-bound; inherits the group proxy
  | "group"
  // Not self-bound; selected from the proxy pool by sticky account ID hashing
  | "pool"
  // Not self-bound; falls back to the global proxy
  | "global"
  // Not self-bound and no proxy at all; connect directly to upstream
  | "direct"
  // The proxy pool is on but has no usable entries, and there is no global proxy: direct connection is not allowed, so the account is skipped
  | "direct_blocked";

export interface ProxyBindingProxy {
  id: number;
  url: string;
  label?: string;
  enabled: boolean;
  test_status?: string;
  test_ip?: string;
  test_location?: string;
  test_latency_ms?: number;
}

export interface ProxyBindingGroup {
  id: number;
  name: string;
  proxy_urls?: string[] | null;
}

export interface ProxyBindingAccount {
  proxy_url?: string | null;
  group_ids?: number[] | null;
}

export interface ProxyBindingContext {
  /** Resin reverse proxy is enabled (only the Codex accounts page passes true; relay-type channels do not go through Resin). */
  resinEnabled: boolean;
  poolEnabled: boolean;
  globalProxy: string;
  /** Number of enabled pool entries: enabled and not failing its test, on the same basis as the backend ListEnabledProxies. */
  poolSize: number;
  /** All entries in the proxies table (including disabled or failed-test ones), used to identify "managed but unusable". */
  managed: Map<string, ProxyBindingProxy>;
  /** Set of enabled pool URLs. */
  usable: Set<string>;
  groups: Map<number, ProxyBindingGroup>;
}

export interface AccountProxyBinding {
  kind: AccountProxyBindingKind;
  /** The URL when self-bound; an empty string when inherited. */
  url: string;
  /** The row matched in the proxies table, used by the badge to show label / egress / latency. */
  proxy: ProxyBindingProxy | null;
  /** Group name when kind === "group". */
  groupName: string;
  /** Whether this account currently has a usable egress; false means the scheduler will skip it. */
  usable: boolean;
}

function trimValue(value: string | null | undefined): string {
  return (value ?? "").trim();
}

/** Same basis as the WHERE clause of the backend ListEnabledProxies: enabled and not failing its test. */
export function proxyIsUsable(
  proxy: Pick<ProxyBindingProxy, "enabled" | "test_status">,
): boolean {
  return proxy.enabled && (proxy.test_status || "untested") !== "error";
}

export function buildProxyBindingContext(input: {
  proxies?: ProxyBindingProxy[] | null;
  groups?: ProxyBindingGroup[] | null;
  poolEnabled?: boolean | null;
  globalProxy?: string | null;
  resinEnabled?: boolean | null;
}): ProxyBindingContext {
  const managed = new Map<string, ProxyBindingProxy>();
  const usable = new Set<string>();
  for (const proxy of input.proxies ?? []) {
    const url = trimValue(proxy?.url);
    if (!url) continue;
    managed.set(url, proxy);
    if (proxyIsUsable(proxy)) usable.add(url);
  }

  const groups = new Map<number, ProxyBindingGroup>();
  for (const group of input.groups ?? []) {
    if (!group) continue;
    groups.set(group.id, group);
  }

  return {
    resinEnabled: Boolean(input.resinEnabled),
    poolEnabled: Boolean(input.poolEnabled),
    globalProxy: trimValue(input.globalProxy),
    poolSize: usable.size,
    managed,
    usable,
    groups,
  };
}

/**
 * Managed but unusable: the URL is in the proxies table but not in the enabled pool. This rule does not apply when the proxy pool is off
 * (in that case disabled entries still work), and custom URLs not in the pool are never affected.
 */
export function isManagedProxyUnavailable(
  url: string,
  ctx: ProxyBindingContext,
): boolean {
  if (!ctx.poolEnabled) return false;
  if (!ctx.managed.has(url)) return false;
  return !ctx.usable.has(url);
}

export function resolveAccountProxyBinding(
  account: ProxyBindingAccount | null | undefined,
  ctx: ProxyBindingContext,
): AccountProxyBinding {
  const base = { url: "", proxy: null, groupName: "", usable: true } as const;

  const accountProxy = trimValue(account?.proxy_url);
  // Resin full-layer override: egress is provided by Resin, so an empty pool or an invalid binding no longer causes the account to be skipped.
  // The self-bound URL is included only to indicate "this config has been overridden", not that it is in effect.
  if (ctx.resinEnabled) {
    return {
      ...base,
      kind: "resin",
      url: accountProxy,
      proxy: accountProxy ? (ctx.managed.get(accountProxy) ?? null) : null,
    };
  }
  if (accountProxy) {
    const proxy = ctx.managed.get(accountProxy) ?? null;
    if (isManagedProxyUnavailable(accountProxy, ctx)) {
      return { ...base, kind: "bound_unusable", url: accountProxy, proxy, usable: false };
    }
    return {
      ...base,
      kind: proxy ? "bound" : "bound_custom",
      url: accountProxy,
      proxy,
    };
  }

  // Groups are taken in group_ids order, using the first group with a usable proxy; if every entry in a group is unusable, move on to the next,
  // matching the skip semantics of the backend group loop.
  for (const groupID of account?.group_ids ?? []) {
    const group = ctx.groups.get(groupID);
    if (!group) continue;
    const hasUsable = (group.proxy_urls ?? []).some((raw) => {
      const url = trimValue(raw);
      return Boolean(url) && !isManagedProxyUnavailable(url, ctx);
    });
    if (!hasUsable) continue;
    return { ...base, kind: "group", groupName: group.name };
  }

  if (ctx.poolEnabled && ctx.poolSize > 0) {
    return { ...base, kind: "pool" };
  }
  if (ctx.globalProxy) {
    return { ...base, kind: "global" };
  }
  // The pool is on but there is nowhere to go: the backend does not allow direct connection at this point, and AccountHasUsableEgress filters the account out.
  if (ctx.poolEnabled) {
    return { ...base, kind: "direct_blocked", usable: false };
  }
  return { ...base, kind: "direct" };
}

/** Short name shown in the badge: the proxy note first, then host:port, and finally the raw URL. */
export function formatProxyShortName(
  proxy: ProxyBindingProxy | null,
  url: string,
): string {
  const label = trimValue(proxy?.label);
  if (label) return label;
  const target = trimValue(url) || trimValue(proxy?.url);
  if (!target) return "";
  try {
    const parsed = new URL(target);
    return parsed.host || target;
  } catch {
    return target;
  }
}

/**
 * The URL shown in the hover tooltip: keeps scheme/host/port so the user can tell which proxy it is, but masks the credentials.
 * The proxy page masks URLs by default and provides a show/hide toggle; the hover tooltip on the list should not be looser than that. The user
 * has no "show credentials" action. To see the full value, use the input in the edit dialog.
 */
export function formatProxyDisplayURL(url: string): string {
  const target = trimValue(url);
  if (!target) return "";
  try {
    const parsed = new URL(target);
    if (!parsed.username && !parsed.password) return target;
    return `${parsed.protocol}//***:***@${parsed.host}`;
  } catch {
    return target;
  }
}

/** Summary of a proxy pool entry's egress info (IP / region / latency); an empty string if it has not been tested. */
export function formatProxyEgressSummary(
  proxy: ProxyBindingProxy | null,
): string {
  if (!proxy || proxy.test_status !== "success") return "";
  const latency = proxy.test_latency_ms ?? 0;
  return [
    trimValue(proxy.test_ip),
    trimValue(proxy.test_location),
    latency > 0 ? `${latency}ms` : "",
  ]
    .filter(Boolean)
    .join(" · ");
}
