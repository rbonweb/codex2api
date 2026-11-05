/**
 * The byte limit field is shown in KiB/MiB in the form, and the displayed value is a rounded integer. If, on blur, the
 * displayed value is converted back to bytes as-is and sent to the server, a persisted byte value that is not a whole multiple (such as 131073) gets
 * silently changed to 131072.
 *
 * Here the baseline is the last persisted raw byte value: if the displayed unit value has not changed, skip the submit and keep the original
 * byte value; only submit when the user actually changes the unit value. The baseline must be the persisted value, because the form value is already
 * changed to the converted byte value while typing, so comparing against the form value would also count an unedited blur as an
 * edit and miss this guard.
 */

export interface ByteLimitCommitDecision {
  /** Whether bytes should be submitted to the server; false means the unit value is unchanged, so the persisted original is kept. */
  submit: boolean
  /** The byte value submitted this time when submit=true; the persisted original byte value when false. */
  bytes: number
}

export function resolveByteLimitCommit(
  persistedBytes: number | null | undefined,
  submittedUnits: number,
  bytesPerUnit: number,
): ByteLimitCommitDecision {
  const submittedBytes = submittedUnits * bytesPerUnit
  if (typeof persistedBytes !== 'number' || !Number.isFinite(persistedBytes)) {
    return { submit: true, bytes: submittedBytes }
  }
  if (Math.round(persistedBytes / bytesPerUnit) === submittedUnits) {
    return { submit: false, bytes: persistedBytes }
  }
  return { submit: true, bytes: submittedBytes }
}
