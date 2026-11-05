import { useEffect, useRef, useState } from 'react'
import { api } from '../api'
import { refreshIntelligenceBars } from '../hooks/useIntelligenceBars'
import { intelligenceColor, intelligenceLabels, intelligenceScheduleLabel, type IntelligenceBar, type IntelligenceRun, type IntelligencePage } from '../lib/intelligenceCheck'
import { getErrorMessage } from '../utils/error'
import { Button } from './ui/button'
import { Switch } from './ui/switch'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from './ui/dialog'

function verdict(run: IntelligenceRun) {
  if (run.status === 'queued' || run.status === 'running') return intelligenceLabels[run.status]
  if (run.status === 'error') return 'Request did not succeed, so it was not part of the answer and latency verdict'
  if (run.status === 'interrupted') return 'Check was interrupted and is not part of the verdict'
  const checks = []
  if (run.judge_mode !== 'latency') checks.push(run.answer_passed ? 'Answer matched' : 'Answer did not match')
  if (run.judge_mode !== 'answer') checks.push(run.latency_passed ? 'Latency met' : 'Latency over threshold')
  return checks.join('; ')
}

export default function AccountIntelligenceHistoryDialog({ accountID, accountName, onClose }: {
  accountID: number | null
  accountName?: string
  onClose: () => void
}) {
  const [page, setPage] = useState(1)
  const [data, setData] = useState<IntelligencePage<IntelligenceRun> | null>(null)
  const [enabled, setEnabled] = useState<boolean | null>(null)
  const [error, setError] = useState('')
  const [schedule, setSchedule] = useState<IntelligenceBar | null>(null)
  const [saving, setSaving] = useState(false)
  const changing = useRef(false)
  const [refresh, setRefresh] = useState(0)
  useEffect(() => { setPage(1); setEnabled(null); setSchedule(null) }, [accountID])
  useEffect(() => {
    setData(null)
    setError('')
    if (!accountID) return
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout> | undefined
    const load = async () => {
      if (document.hidden) { timer = setTimeout(load, 4000); return }
      try {
        const [runs, bars] = await Promise.all([
          api.getIntelligenceRuns(page, { account_id: accountID }, controller.signal),
          api.getIntelligenceBars([accountID], controller.signal),
        ])
        if (controller.signal.aborted) return
        setData(runs)
        setSchedule(bars.bars[String(accountID)] ?? null)
        if (!changing.current) setEnabled(bars.bars[String(accountID)]?.enabled ?? false)
        setError('')
      } catch (e) {
        if (!controller.signal.aborted) setError(getErrorMessage(e))
      } finally {
        if (!controller.signal.aborted) timer = setTimeout(load, 4000)
      }
    }
    void load()
    return () => { controller.abort(); clearTimeout(timer) }
  }, [accountID, page, refresh])
  const toggle = async (value: boolean) => {
    if (!accountID) return
    changing.current = true
    setSaving(true)
    try {
      await api.setIntelligenceAccounts([accountID], value)
      setEnabled(value)
      setError('')
      refreshIntelligenceBars()
    } catch (e) { setError(getErrorMessage(e)) }
    finally { changing.current = false; setSaving(false) }
  }
  return <Dialog open={!!accountID} onOpenChange={open => { if (!open) onClose() }}>
    <DialogContent className="sm:max-w-3xl">
      <DialogHeader>
        <DialogTitle className="break-all pr-6">Intelligence check records · {accountName || data?.items[0]?.account_name || `#${accountID}`}</DialogTitle>
        <DialogDescription>Newest records first; request errors do not count toward the pass rate.</DialogDescription>
      </DialogHeader>
      <div className="flex flex-wrap items-center justify-between gap-3 border-b pb-4 text-sm">
        <label className="flex items-center gap-2">Auto check for this account
          <Switch aria-label="Auto check for this account" checked={enabled ?? false} disabled={enabled === null || saving} onCheckedChange={v => void toggle(v)} />
        </label>
        <Button variant="outline" size="sm" onClick={() => setRefresh(n => n + 1)}>Refresh records</Button>
      </div>
      {schedule && <p className="text-sm text-muted-foreground">{intelligenceScheduleLabel(schedule)}</p>}
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      {!data && !error && <p className="py-8 text-center text-sm text-muted-foreground">Loading…</p>}
      {data?.items.length === 0 && <p className="py-8 text-center text-sm text-muted-foreground">No check records yet. Click Check now above the account status bar to run one.</p>}
      <div className="min-w-0 space-y-4">
        {data?.items.slice().sort((a, b) => b.id - a.id).map(run => <article key={run.id} className="min-w-0 space-y-3 rounded-lg border bg-card p-4 text-sm">
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <span className="inline-flex items-center gap-1.5 font-medium"><span className="size-2 rounded-full bg-muted" style={{ backgroundColor: intelligenceColor(run.status) }} />{intelligenceLabels[run.status]}</span>
            <span className="text-xs text-muted-foreground">{run.source === 'manual' ? 'Manual' : 'Auto'}</span>
            <time className="text-xs text-muted-foreground">{new Date(run.created_at || 0).toLocaleString()}</time>
            <span className="ml-auto text-xs tabular-nums text-muted-foreground">#{run.id}</span>
          </div>
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
            <span className="break-all">Question: {run.question.name}</span><span className="break-all">Model: {run.model}{run.reasoning_effort ? ` · ${run.reasoning_effort}` : ''}</span>
            <span>Latency: {((run.duration_ms ?? 0) / 1000).toFixed(2)} seconds / threshold {(run.question.max_duration_ms / 1000).toFixed(2)} seconds</span>
          </div>
          <p className="break-words">Verdict reason: {verdict(run)}</p>
          <p className="break-words">Expected answer: <code>{run.question.expected_answer}</code></p>
          <div><p className="mb-1 text-xs text-muted-foreground">Actual output</p><pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-3 font-sans text-sm [overflow-wrap:anywhere]">{run.output || 'No output yet'}</pre></div>
          {run.error && <div><p className="mb-1 text-xs text-muted-foreground">Error</p><pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-3 font-sans text-sm text-red-800 dark:text-red-400 [overflow-wrap:anywhere]">{run.error}</pre></div>}
          {run.action_message && <p className="break-words text-xs text-muted-foreground">{run.action_message}</p>}
        </article>)}
      </div>
      {data && <div className="flex items-center justify-between gap-2 border-t pt-4 text-xs text-muted-foreground">
        <span>{data.total} total · page {page}</span>
        <div className="flex gap-2"><Button size="sm" variant="outline" disabled={page <= 1} onClick={() => setPage(p => p - 1)}>Previous</Button><Button size="sm" variant="outline" disabled={page * data.page_size >= data.total} onClick={() => setPage(p => p + 1)}>Next</Button></div>
      </div>}
    </DialogContent>
  </Dialog>
}
