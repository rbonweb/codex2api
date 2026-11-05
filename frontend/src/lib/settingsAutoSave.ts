/**
 * Helper for the settings page autosave's "server-confirmed values".
 *
 * Typed fields (such as the output guard channel limit) sync their draft
 * to settingsFormRef before the blur commit, so if an autosave fails, the "form value before the commit" may only be what the user has just typed
 * and was never confirmed by the server. Rollback must go back to the last server-confirmed snapshot,
 * not to this draft.
 */

/** Field values used for rollback when autosave fails: prefer the last server-confirmed snapshot; the snapshot is not yet
 * built (before the first load completes), fall back to the form value from before the commit. */
export function resolveAutoSaveRollbackValues<T extends object>(
  confirmed: T | null,
  submitted: T,
  keys: ReadonlyArray<keyof T>,
): Partial<T> {
  const source = confirmed ?? submitted
  const values: Partial<T> = {}
  for (const key of keys) {
    values[key] = source[key]
  }
  return values
}

/** Merges the server-confirmed values returned by one successful request into the snapshot, overwriting only the fields that request submitted,
 * and never overwriting the whole snapshot or fields that are not yet confirmed or belong to other requests. */
export function mergeConfirmedSettingsValues<T extends object>(
  confirmed: T | null,
  confirmedValues: Partial<T>,
): T | null {
  return confirmed ? { ...confirmed, ...confirmedValues } : confirmed
}

export interface ConfirmedSettingsVersionState<T extends object> {
  confirmed: T | null
  confirmedVersions: Record<string, number>
}

/**
 * Merges server-confirmed values, with checks on the version or output_guard_config_generation.
 *
 * Records the "last confirmed request version" for each field; when merging:
 * 1. If the field belongs to output guard and incomingValues contains output_guard_config_generation:
 *    compare incomingValues.output_guard_config_generation with the existing output_guard_config_generation in confirmed.
 *    if the response's generation is larger (or equal and incomingVer >= currentVer), the server's commit is newer, so adopt the value;
 *    if the response's generation is smaller, the server committed this response's config first and the confirmed config later, so ignore the old response's fields.
 * 2. For other fields (or when there is no generation), compare field by field whether the request version carried by this response is newer than the field's last confirmed version.
 * If an old response arrives after a newer one, skip the old field values so the rollback baseline is not reverted to stale values.
 */
export function mergeConfirmedSettingsWithVersions<T extends object>(
  confirmed: T | null,
  confirmedVersions: Record<string, number>,
  incomingValues: Partial<T>,
  incomingVersions: Record<string, number>,
): ConfirmedSettingsVersionState<T> {
  const nextVersions: Record<string, number> = { ...confirmedVersions }
  const acceptableValues: Partial<T> = {}
  let hasUpdate = false

  const incomingGuardGen = (incomingValues as Record<string, unknown>).output_guard_config_generation
  const currentGuardGen = confirmed ? (confirmed as Record<string, unknown>).output_guard_config_generation : undefined
  const hasIncomingGuardGen = typeof incomingGuardGen === 'number' && Number.isFinite(incomingGuardGen)
  const hasCurrentGuardGen = typeof currentGuardGen === 'number' && Number.isFinite(currentGuardGen)

  for (const key of Object.keys(incomingValues) as Array<keyof T>) {
    const fieldKey = String(key)
    const incomingVer = incomingVersions[fieldKey] ?? 0
    const currentVer = nextVersions[fieldKey] ?? 0

    // Check whether this is an output guard field
    const isGuardKey = fieldKey.startsWith('output_guard_')
    if (isGuardKey && hasIncomingGuardGen && hasCurrentGuardGen) {
      if ((incomingGuardGen as number) > (currentGuardGen as number)) {
        acceptableValues[key] = incomingValues[key]
        nextVersions[fieldKey] = Math.max(currentVer, incomingVer)
        hasUpdate = true
      } else if ((incomingGuardGen as number) === (currentGuardGen as number)) {
        if (incomingVer >= currentVer) {
          acceptableValues[key] = incomingValues[key]
          nextVersions[fieldKey] = incomingVer
          hasUpdate = true
        }
      }
      // Skip when the generation is smaller
      continue
    }

    if (incomingVer > currentVer) {
      acceptableValues[key] = incomingValues[key]
      nextVersions[fieldKey] = incomingVer
      hasUpdate = true
    }
  }

  if (!confirmed) {
    return {
      confirmed: null,
      confirmedVersions: nextVersions,
    }
  }

  if (!hasUpdate) {
    return {
      confirmed,
      confirmedVersions: nextVersions,
    }
  }

  return {
    confirmed: { ...confirmed, ...acceptableValues },
    confirmedVersions: nextVersions,
  }
}

