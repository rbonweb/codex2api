import { useCallback, useEffect, useState } from 'react'
import { ShieldAlert, X } from 'lucide-react'
import { api } from '../api'

const DISMISS_STORAGE_KEY = 'codex2api_security_banner_dismissed_at'
const DISMISS_TTL_MS = 24 * 60 * 60 * 1000 // do not show the banner again for 24h after the user dismisses it

const COPY = {
  title: 'Security notice: no public API key has been configured',
  body: 'By default, /v1/* requests are refused (503) until at least one API key is created. Open the API Keys page to create one before exposing this service. (Note: when the server is started with CODEX_ALLOW_ANONYMOUS=true, anonymous traffic is allowed — recommended for internal/testing scenarios only.)',
  cta: 'Create API key',
  dismiss: 'Dismiss',
} as const

export default function SecurityBanner() {
  const [keyCount, setKeyCount] = useState<number | null>(null)
  const [dismissed, setDismissed] = useState(() => {
    const ts = Number(localStorage.getItem(DISMISS_STORAGE_KEY) ?? '0')
    return ts > 0 && Date.now() - ts < DISMISS_TTL_MS
  })

  const refresh = useCallback(async () => {
    try {
      const res = await api.getAPIKeys({ view: 'lite' })
      setKeyCount((res.keys ?? []).length)
    } catch {
      setKeyCount(null) // hide on 401 or network errors so it does not interrupt before login
    }
  }, [])

  useEffect(() => {
    void refresh()
    const timer = window.setInterval(() => {
      void refresh()
    }, 60_000)
    return () => window.clearInterval(timer)
  }, [refresh])

  if (dismissed) return null
  if (keyCount === null) return null
  if (keyCount > 0) return null

  const copy = COPY

  const handleDismiss = () => {
    localStorage.setItem(DISMISS_STORAGE_KEY, String(Date.now()))
    setDismissed(true)
  }

  return (
    <div className="mb-4 flex min-w-0 max-w-full items-start gap-3 overflow-hidden rounded-lg border border-red-500/40 bg-red-500/10 p-4 text-red-700 dark:text-red-200">
      <ShieldAlert className="mt-0.5 size-5 shrink-0" />
      <div className="flex-1 min-w-0">
        <p className="text-sm font-bold">{copy.title}</p>
        <p className="mt-1 break-words text-sm leading-relaxed [overflow-wrap:anywhere]">{copy.body}</p>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <a
            href="/admin/api-keys"
            className="inline-flex items-center rounded-md bg-red-600 px-3 py-1.5 text-xs font-semibold text-white shadow-sm transition-colors hover:bg-red-500"
          >
            {copy.cta}
          </a>
          <button
            onClick={handleDismiss}
            className="inline-flex items-center rounded-md border border-red-500/40 px-3 py-1.5 text-xs font-semibold text-red-700 transition-colors hover:bg-red-500/10 dark:text-red-200"
          >
            {copy.dismiss}
          </button>
        </div>
      </div>
      <button
        onClick={handleDismiss}
        className="text-red-700/80 hover:text-red-700 dark:text-red-200/80 dark:hover:text-red-200"
        aria-label="dismiss"
      >
        <X className="size-4" />
      </button>
    </div>
  )
}
