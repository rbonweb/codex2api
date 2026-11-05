import { useCallback, useEffect, useState } from 'react'
import type { AccountGroup } from '../types'

const STORAGE_KEY = 'codex2api.importGroupIds'

function readStored(): number[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw)
    if (!Array.isArray(parsed)) return []
    return parsed.filter((id): id is number => typeof id === 'number' && id > 0)
  } catch {
    return []
  }
}

/**
 * useImportGroupIds remembers the group selected when importing or adding accounts.
 *
 * Batch imports often use the same set of groups, and reselecting them every time is annoying; the selection is kept in localStorage and persists across pages and refreshes. When a group is deleted, the local record becomes invalid, so prune is provided to let the caller, once it has the
 * group list, remove IDs that no longer exist; otherwise the backend rejects the submission with a 400 (group does not exist).
 */
export function useImportGroupIds(): {
  groupIds: number[]
  setGroupIds: (ids: number[]) => void
  prune: (groups: AccountGroup[]) => void
} {
  const [groupIds, setGroupIdsState] = useState<number[]>(readStored)

  useEffect(() => {
    try {
      if (groupIds.length === 0) localStorage.removeItem(STORAGE_KEY)
      else localStorage.setItem(STORAGE_KEY, JSON.stringify(groupIds))
    } catch {
      // When localStorage is unavailable (private mode, etc.), the selection is simply not remembered; functionality is not affected.
    }
  }, [groupIds])

  const setGroupIds = useCallback((ids: number[]) => {
    setGroupIdsState(ids.filter((id) => typeof id === 'number' && id > 0))
  }, [])

  const prune = useCallback((groups: AccountGroup[]) => {
    if (groups.length === 0) return
    const alive = new Set(groups.map((group) => group.id))
    setGroupIdsState((current) => {
      const next = current.filter((id) => alive.has(id))
      return next.length === current.length ? current : next
    })
  }, [])

  return { groupIds, setGroupIds, prune }
}
