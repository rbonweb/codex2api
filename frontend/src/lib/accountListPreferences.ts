// Page preferences for the account list: sort order (per tab) and auto-refresh interval (per browser).
//
// The sort order is stored in sessionStorage: switching menus or pressing F5 within the same tab keeps it; closing the tab or
// opening a new tab returns to the default sort, avoiding the long-term surprise of "remembered but unknown to the user".
// The auto-refresh interval is an option the user turns on explicitly and its button is always visible, so it is stored in localStorage.

export const ACCOUNT_LIST_SORT_STORAGE_KEY = "codex2api:accounts:sort";
export const ACCOUNT_AUTO_REFRESH_STORAGE_KEY =
  "codex2api:accounts:auto-refresh-seconds";

export const ACCOUNT_LIST_SORT_KEYS = [
  "requests",
  "today",
  "usage",
  "importTime",
  "schedulerPriority",
  "group",
  "id",
] as const;
export type AccountListSortKey = (typeof ACCOUNT_LIST_SORT_KEYS)[number];
export type AccountListSortDir = "asc" | "desc";
export type AccountListSort = {
  key: AccountListSortKey | null;
  dir: AccountListSortDir;
};

// 0 means auto-refresh is off.
export const ACCOUNT_AUTO_REFRESH_INTERVALS = [0, 5, 10, 30] as const;
export type AccountAutoRefreshSeconds =
  (typeof ACCOUNT_AUTO_REFRESH_INTERVALS)[number];

// page-stats runs several aggregations over the last 7 days of usage_logs and has no server-side cache,
// so during auto-refresh it re-fetches at most at this interval; with a shorter interval only the list itself refreshes.
export const ACCOUNT_PAGE_STATS_MIN_REFRESH_MS = 30_000;

type PreferenceStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

const DEFAULT_SORT: AccountListSort = { key: null, dir: "desc" };

function resolveStorage(
  kind: "session" | "local",
  storage?: PreferenceStorage,
): PreferenceStorage | null {
  if (storage) return storage;
  if (typeof window === "undefined") return null;
  try {
    return kind === "session" ? window.sessionStorage : window.localStorage;
  } catch {
    return null;
  }
}

function isSortKey(value: unknown): value is AccountListSortKey {
  return (
    typeof value === "string" &&
    (ACCOUNT_LIST_SORT_KEYS as readonly string[]).includes(value)
  );
}

export function readAccountListSort(
  storage?: PreferenceStorage,
): AccountListSort {
  try {
    const raw = resolveStorage("session", storage)?.getItem(
      ACCOUNT_LIST_SORT_STORAGE_KEY,
    );
    if (!raw) return { ...DEFAULT_SORT };
    const parsed = JSON.parse(raw) as { key?: unknown; dir?: unknown };
    if (!isSortKey(parsed.key)) return { ...DEFAULT_SORT };
    return { key: parsed.key, dir: parsed.dir === "asc" ? "asc" : "desc" };
  } catch {
    return { ...DEFAULT_SORT };
  }
}

export function writeAccountListSort(
  sort: AccountListSort,
  storage?: PreferenceStorage,
): void {
  try {
    const target = resolveStorage("session", storage);
    if (!target) return;
    if (sort.key === null) {
      target.removeItem(ACCOUNT_LIST_SORT_STORAGE_KEY);
      return;
    }
    target.setItem(
      ACCOUNT_LIST_SORT_STORAGE_KEY,
      JSON.stringify({ key: sort.key, dir: sort.dir }),
    );
  } catch {
    // Keep the in-memory sort working when storage is unavailable.
  }
}

export function normalizeAccountAutoRefreshSeconds(
  value: unknown,
): AccountAutoRefreshSeconds {
  const parsed = typeof value === "number" ? value : Number(value);
  return (ACCOUNT_AUTO_REFRESH_INTERVALS as readonly number[]).includes(parsed)
    ? (parsed as AccountAutoRefreshSeconds)
    : 0;
}

export function readAccountAutoRefreshSeconds(
  storage?: PreferenceStorage,
): AccountAutoRefreshSeconds {
  try {
    const raw = resolveStorage("local", storage)?.getItem(
      ACCOUNT_AUTO_REFRESH_STORAGE_KEY,
    );
    return raw ? normalizeAccountAutoRefreshSeconds(raw) : 0;
  } catch {
    return 0;
  }
}

export function writeAccountAutoRefreshSeconds(
  seconds: AccountAutoRefreshSeconds,
  storage?: PreferenceStorage,
): void {
  try {
    const target = resolveStorage("local", storage);
    if (!target) return;
    if (seconds === 0) {
      target.removeItem(ACCOUNT_AUTO_REFRESH_STORAGE_KEY);
      return;
    }
    target.setItem(ACCOUNT_AUTO_REFRESH_STORAGE_KEY, String(seconds));
  } catch {
    // Keep the in-memory interval working when storage is unavailable.
  }
}

// shouldRefreshPageStats decides whether this auto-refresh round also re-fetches page-stats.
export function shouldRefreshPageStats(
  lastRefreshAt: number,
  now: number,
  minIntervalMs: number = ACCOUNT_PAGE_STATS_MIN_REFRESH_MS,
): boolean {
  return now - lastRefreshAt >= minIntervalMs;
}
