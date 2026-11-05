import i18n from '../i18n'
import { readTimezonePreference, resolveDisplayTimezone, writeTimezonePreference } from '../lib/displayTimezone'

export { AUTO_DISPLAY_TIMEZONE } from '../lib/displayTimezone'

export interface RelativeTimeOptions {
  variant?: 'long' | 'compact'
  includeSeconds?: boolean
  fallback?: string
}

export function formatRelativeTime(dateStr?: string | null, options: RelativeTimeOptions = {}): string {
  const {
    variant = 'long',
    includeSeconds = false,
    fallback = '-',
  } = options

  if (!dateStr) {
    return fallback
  }

  const timestamp = new Date(dateStr).getTime()
  if (Number.isNaN(timestamp)) {
    return fallback
  }

  const diff = Math.max(0, Date.now() - timestamp)
  const seconds = Math.floor(diff / 1000)

  if (includeSeconds && seconds < 60) {
    return variant === 'compact'
      ? i18n.t('common.secondsAgoCompact', { count: seconds })
      : i18n.t('common.secondsAgoLong', { count: seconds })
  }

  const minutes = Math.floor(seconds / 60)
  if (minutes < 1) {
    return i18n.t('common.justNow')
  }

  if (minutes < 60) {
    return variant === 'compact'
      ? i18n.t('common.minutesAgoCompact', { count: minutes })
      : i18n.t('common.minutesAgoLong', { count: minutes })
  }

  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return variant === 'compact'
      ? i18n.t('common.hoursAgoCompact', { count: hours })
      : i18n.t('common.hoursAgoLong', { count: hours })
  }

  const days = Math.floor(hours / 24)
  return variant === 'compact'
    ? i18n.t('common.daysAgoCompact', { count: days })
    : i18n.t('common.daysAgoLong', { count: days })
}

/** Currently effective display timezone (IANA). When no choice is saved or "Auto" is selected, it follows the browser timezone, and falls back to UTC if that cannot be read. */
export function getTimezone(): string {
  return resolveDisplayTimezone()
}

/** Saved display timezone preference: an IANA name, or AUTO_DISPLAY_TIMEZONE to follow the browser. */
export function getTimezonePreference(): string {
  return readTimezonePreference()
}

/** Saves the display timezone preference to localStorage; passing AUTO_DISPLAY_TIMEZONE clears the saved value (follow the browser). */
export function setTimezone(tz: string): void {
  writeTimezonePreference(tz)
  dateTimeFormatters.clear()
}

const dateTimeFormatters = new Map<string, Intl.DateTimeFormat>()

function dateTimeFormatter(timeZone: string): Intl.DateTimeFormat {
  let fmt = dateTimeFormatters.get(timeZone)
  if (!fmt) {
    fmt = new Intl.DateTimeFormat('sv-SE', {
      timeZone,
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hourCycle: 'h23',
    })
    dateTimeFormatters.set(timeZone, fmt)
  }
  return fmt
}

/**
 * Formats a time in the current display timezone, outputting YYYY-MM-DD HH:mm:ss.
 *
 * The function name is kept from earlier naming; it actually uses the display timezone returned by getTimezone() and is not fixed to Beijing time.
 * It uses Intl.DateTimeFormat for conversion, so it displays correctly whether the backend returns UTC (with Z) or an offset (+08:00),
 * avoiding duplicate conversion caused by manually adding or subtracting offsets.
 */
export function formatBeijingTime(dateStr?: string | null, fallback = '-'): string {
  if (!dateStr) return fallback

  const date = new Date(dateStr)
  if (Number.isNaN(date.getTime())) return fallback

  // The sv-SE locale outputs "YYYY-MM-DD HH:mm:ss", which is exactly the target format
  return dateTimeFormatter(getTimezone()).format(date)
}
