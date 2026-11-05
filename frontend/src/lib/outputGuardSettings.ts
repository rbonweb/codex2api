export const KIB = 1024
export const MIB = 1024 * 1024
export const MIN_OUTPUT_GUARD_TOOL_BYTES = 16 * KIB
export const MAX_OUTPUT_GUARD_TOOL_BYTES = 4 * MIB
export const MIN_OUTPUT_GUARD_TOOL_DELTAS = 256
export const MAX_OUTPUT_GUARD_TOOL_DELTAS = 65536
export const MIN_OUTPUT_GUARD_CHANNEL_BYTES = 128 * KIB
export const MAX_OUTPUT_GUARD_CHANNEL_BYTES = 16 * MIB
export const MIN_OUTPUT_GUARD_REQUEST_BYTES = 256 * KIB
export const MAX_OUTPUT_GUARD_REQUEST_BYTES = 64 * MIB

export interface OutputGuardSettingsValues {
  output_guard_enabled: boolean
  output_guard_max_tool_argument_bytes: number
  output_guard_max_tool_argument_deltas: number
  output_guard_max_channel_bytes: number
  output_guard_max_request_bytes: number
}

export const DEFAULT_OUTPUT_GUARD_SETTINGS: OutputGuardSettingsValues = {
  output_guard_enabled: false,
  output_guard_max_tool_argument_bytes: 256 * KIB,
  output_guard_max_tool_argument_deltas: 4096,
  output_guard_max_channel_bytes: 2 * MIB,
  output_guard_max_request_bytes: 4 * MIB,
}

export const OUTPUT_GUARD_KEYS = [
  'output_guard_enabled',
  'output_guard_max_tool_argument_bytes',
  'output_guard_max_tool_argument_deltas',
  'output_guard_max_channel_bytes',
  'output_guard_max_request_bytes',
] as const

export function isOutputGuardKey(key: string): boolean {
  return OUTPUT_GUARD_KEYS.indexOf(key as (typeof OUTPUT_GUARD_KEYS)[number]) !== -1
}

/**
 * Before a full-form save, guard write fields are removed: guard may only go through autosave (patching by key)
 * path. A full-form submit sends the stale snapshot read when the page was opened manually, together with the new generation
 * and may overwrite guard settings that another admin just changed or even just turned off.
 */
export function omitOutputGuardFields<T extends object>(settings: T): T {
  const payload = { ...settings }
  for (const key of OUTPUT_GUARD_KEYS) {
    Reflect.deleteProperty(payload, key)
  }
  return payload
}

export function validateOutputGuardHierarchy(settings: {
  output_guard_max_tool_argument_bytes: number
  output_guard_max_channel_bytes: number
  output_guard_max_request_bytes: number
}): string | null {
  if (settings.output_guard_max_tool_argument_bytes > settings.output_guard_max_channel_bytes) {
    return 'tool argument limit cannot be greater than channel limit (tool <= channel)'
  }
  if (settings.output_guard_max_channel_bytes > settings.output_guard_max_request_bytes) {
    return 'channel limit cannot be greater than request limit (channel <= request)'
  }
  return null
}

export function validateOutputGuardRanges(settings: {
  output_guard_max_tool_argument_bytes?: number
  output_guard_max_tool_argument_deltas?: number
  output_guard_max_channel_bytes?: number
  output_guard_max_request_bytes?: number
}): string | null {
  const tool = settings.output_guard_max_tool_argument_bytes
  if (tool !== undefined && (tool < MIN_OUTPUT_GUARD_TOOL_BYTES || tool > MAX_OUTPUT_GUARD_TOOL_BYTES)) {
    return `tool argument byte range must be between ${MIN_OUTPUT_GUARD_TOOL_BYTES} and ${MAX_OUTPUT_GUARD_TOOL_BYTES}`
  }
  const deltas = settings.output_guard_max_tool_argument_deltas
  if (deltas !== undefined && (deltas < MIN_OUTPUT_GUARD_TOOL_DELTAS || deltas > MAX_OUTPUT_GUARD_TOOL_DELTAS)) {
    return `tool Delta count must be between ${MIN_OUTPUT_GUARD_TOOL_DELTAS} and ${MAX_OUTPUT_GUARD_TOOL_DELTAS}`
  }
  const channel = settings.output_guard_max_channel_bytes
  if (channel !== undefined && (channel < MIN_OUTPUT_GUARD_CHANNEL_BYTES || channel > MAX_OUTPUT_GUARD_CHANNEL_BYTES)) {
    return `channel byte range must be between ${MIN_OUTPUT_GUARD_CHANNEL_BYTES} and ${MAX_OUTPUT_GUARD_CHANNEL_BYTES}`
  }
  const request = settings.output_guard_max_request_bytes
  if (request !== undefined && (request < MIN_OUTPUT_GUARD_REQUEST_BYTES || request > MAX_OUTPUT_GUARD_REQUEST_BYTES)) {
    return `request byte range must be between ${MIN_OUTPUT_GUARD_REQUEST_BYTES} and ${MAX_OUTPUT_GUARD_REQUEST_BYTES}`
  }
  return null
}

/**
 * Keeps guard fields from the response or the form:
 * Compares the server response's output_guard_config_generation with the local generation.
 * If the server response is newer (larger), another admin changed guard while the page was open, so the server guard fields and generation are adopted;
 * otherwise the local form's guard fields and generation are kept (this protects an autosave still in flight).
 */
export function preserveOutputGuardFields<T extends object>(
  updated: T,
  current: T,
): T {
  const updatedGen = (updated as Record<string, unknown>).output_guard_config_generation
  const currentGen = (current as Record<string, unknown>).output_guard_config_generation

  const serverGen = typeof updatedGen === 'number' && Number.isFinite(updatedGen) ? updatedGen : -1
  const localGen = typeof currentGen === 'number' && Number.isFinite(currentGen) ? currentGen : -1

  if (serverGen > localGen) {
    return { ...updated }
  }

  const result = { ...updated }
  for (const key of OUTPUT_GUARD_KEYS) {
    if (Object.prototype.hasOwnProperty.call(current, key)) {
      result[key as keyof T] = current[key as keyof T]
    }
  }
  if (Object.prototype.hasOwnProperty.call(current, 'output_guard_config_generation')) {
    (result as Record<string, unknown>).output_guard_config_generation =
      (current as Record<string, unknown>).output_guard_config_generation
  }
  return result
}

/**
 * On a successful manual save, refresh the confirmed snapshot:
 * 1. Copy the non-guard fields from the form to the confirmed snapshot;
 * 2. If the output_guard_config_generation in the server response is greater than the generation in the confirmed snapshot,
 *    the server has committed and confirmed a newer guard config, so the confirmed snapshot also adopts the guard fields and generation from the server response;
 * 3. Otherwise keep the guard fields and generation already in the confirmed snapshot,
 *    so an unconfirmed guard value from an in-flight autosave does not enter the confirmed snapshot (if that autosave fails, the rollback would restore the rejected value).
 */
export function preserveConfirmedGuardFields<T extends object>(
  currentForm: T,
  confirmed: T | null,
  serverResponse?: T | null,
): T {
  const result = { ...currentForm }

  const serverGenRaw = serverResponse ? (serverResponse as Record<string, unknown>).output_guard_config_generation : undefined
  const confirmedGenRaw = confirmed ? (confirmed as Record<string, unknown>).output_guard_config_generation : undefined

  const serverGen = typeof serverGenRaw === 'number' && Number.isFinite(serverGenRaw) ? serverGenRaw : -1
  const confirmedGen = typeof confirmedGenRaw === 'number' && Number.isFinite(confirmedGenRaw) ? confirmedGenRaw : -1

  if (serverGen > confirmedGen && serverResponse) {
    for (const key of OUTPUT_GUARD_KEYS) {
      if (Object.prototype.hasOwnProperty.call(serverResponse, key)) {
        result[key as keyof T] = serverResponse[key as keyof T]
      }
    }
    if (Object.prototype.hasOwnProperty.call(serverResponse, 'output_guard_config_generation')) {
      (result as Record<string, unknown>).output_guard_config_generation =
        (serverResponse as Record<string, unknown>).output_guard_config_generation
    }
    return result
  }

  if (confirmed) {
    for (const key of OUTPUT_GUARD_KEYS) {
      if (Object.prototype.hasOwnProperty.call(confirmed, key)) {
        result[key as keyof T] = confirmed[key as keyof T]
      }
    }
    if (Object.prototype.hasOwnProperty.call(confirmed, 'output_guard_config_generation')) {
      (result as Record<string, unknown>).output_guard_config_generation =
        (confirmed as Record<string, unknown>).output_guard_config_generation
    }
  }

  return result
}

export function bytesToKiB(bytes: number): number { return Math.round(bytes / KIB) }
export function kibToBytes(kib: number): number { return kib * KIB }
export function bytesToMiB(bytes: number): number { return Math.round(bytes / MIB) }
export function mibToBytes(mib: number): number { return mib * MIB }
