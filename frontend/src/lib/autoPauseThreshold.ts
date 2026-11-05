// The auto-pause threshold is a 0..1 ratio in the backend/database; in the UI it is always entered and shown as a percentage (0..100).
// The per-account scheduling dialog and the group management dialog of every channel must convert through here, so that 95 is not submitted to the backend as-is
// and trigger "out of range, must be between 0 and 1" (issue #843).

// formatAutoPauseThresholdPercent converts the backend ratio into the text for the percentage input; returns an empty string when unset or 0.
export function formatAutoPauseThresholdPercent(value?: number | null): string {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) return "";
  const percent = value * 100;
  if (Number.isInteger(percent)) return String(percent);
  return String(Number(percent.toFixed(2)));
}

// isAutoPauseThresholdPercentInvalid validates the percentage input: an empty string is valid (meaning inherit / not set); otherwise it must be within 0..100.
export function isAutoPauseThresholdPercentInvalid(value: string): boolean {
  const trimmed = value.trim();
  if (!trimmed) return false;
  const parsed = Number(trimmed);
  return !Number.isFinite(parsed) || parsed < 0 || parsed > 100;
}

// autoPauseThresholdPercentToRatio converts the percentage input into the backend ratio; an empty string or <= 0 returns null (inherit / not set).
export function autoPauseThresholdPercentToRatio(value: string): number | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  const parsed = Number(trimmed);
  if (!Number.isFinite(parsed) || parsed <= 0) return null;
  return parsed / 100;
}
