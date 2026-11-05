export const ACCOUNT_LIST_FULL_POOL_SCAN_MAX = 500

export const LARGE_POOL_DISABLED_SORTS = ["requests", "today"] as const

export function toAccountListApiSort(
  sortKey: string | null | undefined,
): string | undefined {
  if (!sortKey) return undefined
  if (sortKey === "importTime") return "created_at"
  if (sortKey === "schedulerPriority") return "scheduler_priority"
  if (sortKey === "updated") return "updated_at"
  return sortKey
}

export function resolveDisabledAccountSorts(
  disabledSorts?: string[] | null,
  poolTotal?: number,
): string[] {
  // An empty array means the backend has explicitly said "aggregation is complete, sorting is allowed". Do not fall back based on pool size anymore,
  // otherwise after batched queries finish, the frontend would still disable the 7d / today sort permanently.
  if (Array.isArray(disabledSorts)) {
    return disabledSorts
  }
  if ((poolTotal ?? 0) > ACCOUNT_LIST_FULL_POOL_SCAN_MAX) {
    return [...LARGE_POOL_DISABLED_SORTS]
  }
  return []
}

export function isLargePoolSortDisabled(
  sortKey: string | null | undefined,
  disabledSorts?: string[] | null,
): boolean {
  const apiSort = toAccountListApiSort(sortKey)
  if (!apiSort) return false
  return (disabledSorts ?? []).includes(apiSort)
}
