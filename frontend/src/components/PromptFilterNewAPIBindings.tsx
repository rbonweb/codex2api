import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  AlertTriangle,
  Check,
  Copy,
  KeyRound,
  Loader2,
  Pencil,
  Plus,
  RefreshCw,
  ShieldCheck,
  Trash2,
} from 'lucide-react'
import { api } from '../api'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import type {
  APIKeyRow,
  PromptFilterNewAPIBinding,
  PromptFilterScope,
} from '../types'
import { getErrorMessage } from '../utils/error'
import { formatBeijingTime } from '../utils/time'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'

type BindingForm = {
  apiKeyId: string
  platformCode: string
  platformName: string
  enabled: boolean
  requireSignedIdentity: boolean
  promptFilterScope: PromptFilterScope
}

const emptyBindingForm: BindingForm = {
  apiKeyId: '',
  platformCode: '',
  platformName: '',
  enabled: true,
  requireSignedIdentity: false,
  promptFilterScope: 'inherit',
}

function apiKeyLabel(apiKey: APIKeyRow): string {
  return `#${apiKey.id} ${apiKey.name || 'Unnamed Key'} · ${apiKey.key || 'Redacted'}`
}

function BindingSwitch({
  label,
  description,
  checked,
  onCheckedChange,
}: {
  label: string
  description: string
  checked: boolean
  onCheckedChange: (checked: boolean) => void
}) {
  return (
    <label className="flex items-start justify-between gap-4 rounded-lg border border-border/70 bg-muted/20 p-3">
      <span className="min-w-0">
        <span className="block text-sm font-medium text-foreground">{label}</span>
        <span className="mt-1 block text-xs leading-5 text-muted-foreground">{description}</span>
      </span>
      <Switch checked={checked} onCheckedChange={onCheckedChange} className="mt-0.5" />
    </label>
  )
}

function BindingFormFields({
  form,
  setForm,
  apiKeys,
  editing,
}: {
  form: BindingForm
  setForm: (next: BindingForm) => void
  apiKeys: APIKeyRow[]
  editing: boolean
}) {
  return (
    <div className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <label className="space-y-1.5 sm:col-span-2">
          <span className="text-sm font-medium">Codex2API Key</span>
          <Select
            value={form.apiKeyId}
            disabled={editing}
            placeholder="Select the Codex2API Key used for access"
            onValueChange={(apiKeyId) => setForm({ ...form, apiKeyId })}
            options={apiKeys.map((apiKey) => ({ value: String(apiKey.id), label: apiKeyLabel(apiKey) }))}
          />
          <span className="block text-xs leading-5 text-muted-foreground">
            One key can be bound to only one caller; a key with no binding does not accept NewAPI signed identities.
          </span>
        </label>
        <label className="space-y-1.5">
          <span className="text-sm font-medium">Caller code</span>
          <Input
            value={form.platformCode}
            placeholder="Enter a unique access identifier"
            maxLength={32}
            onChange={(event) => setForm({
              ...form,
              platformCode: event.target.value.toLowerCase().replace(/[^a-z0-9_-]/g, ''),
            })}
          />
          <span className="block text-xs leading-5 text-muted-foreground">Only lowercase letters, digits, underscores, and hyphens are allowed; up to 32 characters, and globally unique.</span>
        </label>
        <label className="space-y-1.5">
          <span className="text-sm font-medium">Caller name</span>
          <Input
            value={form.platformName}
            placeholder="Enter a name that is easy to recognize"
            maxLength={255}
            onChange={(event) => setForm({ ...form, platformName: event.target.value })}
          />
        </label>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <BindingSwitch
          label="Enable this binding"
          description="When off, this key does not accept this caller's signed identity, and it will not use another key's binding."
          checked={form.enabled}
          onCheckedChange={(enabled) => setForm({ ...form, enabled })}
        />
        <BindingSwitch
          label="Require signed identity"
          description="Off by default. Turn it on only after confirming that the corresponding NewAPI has this binding's key configured correctly; otherwise requests will be rejected for missing or wrong signatures."
          checked={form.requireSignedIdentity}
          onCheckedChange={(requireSignedIdentity) => setForm({ ...form, requireSignedIdentity })}
        />
      </div>
      <label className="block space-y-1.5 rounded-lg border border-border/70 bg-muted/20 p-3">
        <span className="text-sm font-medium">Prompt check scope for this key</span>
        <Select
          value={form.promptFilterScope}
          onValueChange={(promptFilterScope) => setForm({ ...form, promptFilterScope: promptFilterScope as PromptFilterScope })}
          options={[
            { value: 'inherit', label: 'Inherit global: local detection + remote model review' },
            { value: 'local_only', label: 'Local detection only: skip remote model review' },
            { value: 'off', label: 'Skip entirely: do not run Prompt checks' },
          ]}
        />
        <span className="block text-xs leading-5 text-muted-foreground">
          "Local detection only" still runs high-risk rules, session correlation, audit records, and risk profiling; "Skip entirely" only keeps the API key and signed identity checks, and suits clearly controlled internal callers.
        </span>
      </label>
    </div>
  )
}

export default function PromptFilterNewAPIBindings() {
  const [apiKeys, setAPIKeys] = useState<APIKeyRow[]>([])
  const [bindings, setBindings] = useState<PromptFilterNewAPIBinding[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [createOpen, setCreateOpen] = useState(false)
  const [createForm, setCreateForm] = useState<BindingForm>(emptyBindingForm)
  const [editing, setEditing] = useState<PromptFilterNewAPIBinding | null>(null)
  const [editForm, setEditForm] = useState<BindingForm>(emptyBindingForm)
  const [rotating, setRotating] = useState<PromptFilterNewAPIBinding | null>(null)
  const [graceSeconds, setGraceSeconds] = useState('300')
  const [saving, setSaving] = useState(false)
  const [secretReveal, setSecretReveal] = useState<{ platformName: string; secret: string } | null>(null)
  const [secretCopied, setSecretCopied] = useState(false)
  const [secretCloseConfirmOpen, setSecretCloseConfirmOpen] = useState(false)
  const { showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const [keyResult, bindingResult] = await Promise.all([
        api.getAPIKeys({ view: 'lite' }),
        api.getPromptFilterNewAPIBindings(),
      ])
      setAPIKeys(keyResult.keys ?? [])
      setBindings(bindingResult.bindings ?? [])
    } catch (loadError) {
      setError(getErrorMessage(loadError))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { void load() }, [load])

  const boundKeyIDs = useMemo(() => new Set(bindings.map((binding) => binding.api_key_id)), [bindings])
  const availableAPIKeys = useMemo(
    () => apiKeys.filter((apiKey) => !boundKeyIDs.has(apiKey.id)),
    [apiKeys, boundKeyIDs],
  )
  const apiKeyByID = useMemo(() => new Map(apiKeys.map((apiKey) => [apiKey.id, apiKey])), [apiKeys])

  const openCreate = () => {
    const firstAvailable = availableAPIKeys[0]
    setCreateForm({ ...emptyBindingForm, apiKeyId: firstAvailable ? String(firstAvailable.id) : '' })
    setCreateOpen(true)
  }

  const openEdit = (binding: PromptFilterNewAPIBinding) => {
    setEditForm({
      apiKeyId: String(binding.api_key_id),
      platformCode: binding.platform_code,
      platformName: binding.platform_name,
      enabled: binding.enabled,
      requireSignedIdentity: binding.require_signed_identity,
      promptFilterScope: binding.prompt_filter_scope || 'inherit',
    })
    setEditing(binding)
  }

  const revealSecret = (platformName: string, secret?: string) => {
    if (!secret) {
      showToast('The operation succeeded, but the server did not return a one-time key. Please rotate again.', 'error')
      return
    }
    setSecretCopied(false)
    setSecretReveal({ platformName, secret })
  }

  const createBinding = async () => {
    const apiKeyId = Number(createForm.apiKeyId)
    if (!Number.isInteger(apiKeyId) || apiKeyId <= 0 || !createForm.platformCode.trim()) {
      showToast('Select a Codex2API Key and enter a caller code.', 'error')
      return
    }
    if (createForm.promptFilterScope === 'off') {
      const approved = await confirm({
        title: 'Skip all Prompt checks for this key?',
        description: 'Requests from this key will skip local high-risk rules and remote model review. API key authentication and the optional NewAPI signed-identity check will still apply.',
        confirmText: 'Confirm skip entirely',
        tone: 'warning',
      })
      if (!approved) return
    }
    setSaving(true)
    try {
      const result = await api.createPromptFilterNewAPIBinding({
        api_key_id: apiKeyId,
        platform_code: createForm.platformCode.trim(),
        platform_name: createForm.platformName.trim(),
        enabled: createForm.enabled,
        require_signed_identity: createForm.requireSignedIdentity,
        prompt_filter_scope: createForm.promptFilterScope,
      })
      setCreateOpen(false)
      await load()
      revealSecret(result.platform_name || result.platform_code, result.secret)
    } catch (createError) {
      showToast(getErrorMessage(createError), 'error')
    } finally {
      setSaving(false)
    }
  }

  const updateBinding = async () => {
    if (!editing || !editForm.platformCode.trim()) return
    if (!editing.require_signed_identity && editForm.requireSignedIdentity) {
      const approved = await confirm({
        title: 'Enable required signed identity?',
        description: (
          <span>
            The corresponding NewAPI for "{editForm.platformName.trim() || editForm.platformCode.trim()}" must first have this binding's key configured; otherwise, once enabled, requests from this Codex2API Key will immediately return 401. A signature failure is an identity verification error: it is not recorded as a Prompt violation and does not trigger violation penalties.
          </span>
        ),
        confirmText: 'Configured, confirm enable',
        tone: 'warning',
      })
      if (!approved) return
    }
    if (editing.prompt_filter_scope !== 'off' && editForm.promptFilterScope === 'off') {
      const approved = await confirm({
        title: 'Skip all Prompt checks for this key?',
        description: 'After saving, this key will no longer run local high-risk rules and remote model review. API key authentication and the optional NewAPI signed-identity check will still apply.',
        confirmText: 'Confirm skip entirely',
        tone: 'warning',
      })
      if (!approved) return
    }
    setSaving(true)
    try {
      await api.updatePromptFilterNewAPIBinding(editing.api_key_id, {
        platform_code: editForm.platformCode.trim(),
        platform_name: editForm.platformName.trim(),
        enabled: editForm.enabled,
        require_signed_identity: editForm.requireSignedIdentity,
        prompt_filter_scope: editForm.promptFilterScope,
      })
      setEditing(null)
      await load()
      showToast('Audit identity binding updated.')
    } catch (updateError) {
      showToast(getErrorMessage(updateError), 'error')
    } finally {
      setSaving(false)
    }
  }

  const rotateSecret = async () => {
    if (!rotating) return
    const grace = Number(graceSeconds)
    if (!Number.isInteger(grace) || grace < 60 || grace > 86400) {
      showToast('When auto-generating a new key, the grace period in seconds must be an integer between 60 and 86400.', 'error')
      return
    }
    setSaving(true)
    try {
      const result = await api.generatePromptFilterNewAPIBindingSecret(rotating.api_key_id, grace)
      setRotating(null)
      await load()
      revealSecret(result.platform_name || result.platform_code, result.secret)
    } catch (rotateError) {
      showToast(getErrorMessage(rotateError), 'error')
    } finally {
      setSaving(false)
    }
  }

  const deleteBinding = async (binding: PromptFilterNewAPIBinding) => {
    const approved = await confirm({
        title: `Delete binding "${binding.platform_name || binding.platform_code}"?`,
      description: (
        <span>
          After deletion, Key #{binding.api_key_id} will no longer accept NewAPI signed identities; first confirm that the caller has stopped using this key.
        </span>
      ),
      confirmText: 'Confirm delete',
      tone: 'destructive',
      confirmVariant: 'destructive',
    })
    if (!approved) return
    try {
      await api.deletePromptFilterNewAPIBinding(binding.api_key_id)
      await load()
      showToast('Audit identity binding deleted.')
    } catch (deleteError) {
      showToast(getErrorMessage(deleteError), 'error')
    }
  }

  const copySecret = async () => {
    if (!secretReveal) return
    try {
      await navigator.clipboard.writeText(secretReveal.secret)
      setSecretCopied(true)
      showToast('Binding key copied.')
    } catch {
      showToast('Copy failed. Please select and copy the key manually.', 'error')
    }
  }

  const requestCloseSecretReveal = () => {
    if (!secretReveal) return
    setSecretCloseConfirmOpen(true)
  }

  const confirmCloseSecretReveal = () => {
    setSecretCloseConfirmOpen(false)
    setSecretReveal(null)
    setSecretCopied(false)
  }

  return (
    <>
      <section aria-labelledby="newapi-key-bindings-title" className="space-y-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <div className="flex items-center gap-2">
              <KeyRound className="size-5 text-primary" />
              <h4 id="newapi-key-bindings-title" className="text-sm font-semibold">Bind audit identities per Codex2API Key</h4>
            </div>
            <p className="mt-1 text-sm leading-6 text-muted-foreground">
              Matches signed identities to the Codex2API Key the request actually uses, and can narrow the Prompt check scope for a specific key.
              Global thresholds and review tiers are still managed by GuardPipeline; callers cannot change this policy through request headers or metadata.
            </p>
          </div>
          <div className="flex gap-2">
            <Button variant="outline" size="sm" onClick={() => void load()} disabled={loading}>
              <RefreshCw className={`size-4 ${loading ? 'animate-spin' : ''}`} />
              Refresh
            </Button>
            <Button size="sm" onClick={openCreate} disabled={loading || availableAPIKeys.length === 0}>
              <Plus className="size-4" />
              Add binding
            </Button>
          </div>
        </div>

        <div className="flex items-start gap-3 rounded-lg border border-sky-500/25 bg-sky-500/[0.06] p-3">
          <ShieldCheck className="mt-0.5 size-5 shrink-0 text-sky-600 dark:text-sky-300" />
          <div className="text-sm leading-6">
            <div className="font-medium">Use this key's audit key after binding</div>
            <div className="text-muted-foreground">
              If the signature is missing, the caller code is wrong, or the key does not match, the request will not borrow another key's identity and will not link audit records to the wrong binding.
            </div>
          </div>
        </div>

        {error ? (
          <div className="flex items-center justify-between gap-3 rounded-lg border border-destructive/25 bg-destructive/[0.05] p-3 text-sm text-destructive">
            <span>{error}</span>
            <Button variant="outline" size="sm" onClick={() => void load()}>Retry</Button>
          </div>
        ) : null}

        {loading ? (
          <div className="flex min-h-28 items-center justify-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />Reading audit identity bindings...
          </div>
        ) : bindings.length === 0 ? (
          <div className="rounded-lg border border-dashed border-border p-7 text-center">
            <div className="text-sm font-medium">No per-key audit identities configured yet</div>
            <p className="mt-1 text-xs leading-5 text-muted-foreground">
              A 256-bit binding key is generated automatically on creation and is shown only once.
            </p>
            <Button className="mt-4" size="sm" onClick={openCreate} disabled={availableAPIKeys.length === 0}>
              <Plus className="size-4" />Add the first binding
            </Button>
            {availableAPIKeys.length === 0 ? <p className="mt-2 text-xs text-amber-600">No API keys available to bind; create a dedicated key first.</p> : null}
          </div>
        ) : (
            <div className="overflow-x-auto rounded-lg border border-border">
              <table className="w-full min-w-[860px] text-sm">
                <thead className="bg-muted/50 text-left text-xs text-muted-foreground">
                  <tr>
                    <th className="px-3 py-2.5 font-medium">Caller</th>
                    <th className="px-3 py-2.5 font-medium">Codex2API Key</th>
                    <th className="px-3 py-2.5 font-medium">Status</th>
                    <th className="px-3 py-2.5 font-medium">Binding key</th>
                    <th className="px-3 py-2.5 font-medium">Updated</th>
                    <th className="px-3 py-2.5 text-right font-medium">Actions</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {bindings.map((binding) => {
                    const apiKey = apiKeyByID.get(binding.api_key_id)
                    return (
                      <tr key={binding.api_key_id} className="align-top">
                        <td className="px-3 py-3">
                          <div className="font-medium">{binding.platform_name || binding.platform_code}</div>
                          <div className="mt-0.5 font-mono text-xs text-muted-foreground">{binding.platform_code}</div>
                        </td>
                        <td className="px-3 py-3">
                          <div className="font-medium">#{binding.api_key_id} {apiKey?.name || 'Key deleted or not visible'}</div>
                          <div className="mt-0.5 font-mono text-xs text-muted-foreground">{apiKey?.key || '—'}</div>
                        </td>
                        <td className="px-3 py-3">
                          <div className="flex flex-wrap gap-1.5">
                            <Badge variant={binding.enabled ? 'default' : 'outline'}>{binding.enabled ? 'Enabled' : 'Off'}</Badge>
                            <Badge variant={binding.require_signed_identity ? 'default' : 'outline'}>
                              {binding.require_signed_identity ? 'Signature required' : 'Signature not required'}
                            </Badge>
                            <Badge variant={binding.prompt_filter_scope === 'off' ? 'destructive' : 'outline'}>
                              {binding.prompt_filter_scope === 'local_only'
                                ? 'Local detection only'
                                : binding.prompt_filter_scope === 'off'
                                  ? 'Skip Prompt checks'
                                  : 'Inherit global review'}
                            </Badge>
                          </div>
                        </td>
                        <td className="px-3 py-3">
                          <div className="font-mono text-xs">{binding.secret_configured ? binding.secret_masked : 'Not configured'}</div>
                          {binding.previous_secret_active ? (
                            <div className="mt-1 text-xs text-amber-600">
                              Old key grace period ends {formatBeijingTime(binding.previous_secret_expires_at)}
                            </div>
                          ) : null}
                        </td>
                        <td className="whitespace-nowrap px-3 py-3 text-xs text-muted-foreground">{formatBeijingTime(binding.updated_at)}</td>
                        <td className="px-3 py-3">
                          <div className="flex justify-end gap-1.5">
                            <Button variant="outline" size="sm" onClick={() => openEdit(binding)}>
                              <Pencil className="size-3.5" />Edit
                            </Button>
                            <Button variant="outline" size="sm" onClick={() => { setGraceSeconds('300'); setRotating(binding) }}>
                              <RefreshCw className="size-3.5" />Rotate
                            </Button>
                            <Button variant="ghost" size="icon" className="text-destructive" aria-label="Delete binding" onClick={() => void deleteBinding(binding)}>
                              <Trash2 className="size-4" />
                            </Button>
                          </div>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
        )}
      </section>

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>Add Key audit identity binding</DialogTitle>
            <DialogDescription>Choose the Codex2API Key the request actually uses. After saving, the system generates a binding key and shows it only once.</DialogDescription>
          </DialogHeader>
          <BindingFormFields form={createForm} setForm={setCreateForm} apiKeys={availableAPIKeys} editing={false} />
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>Cancel</Button>
            <Button onClick={() => void createBinding()} disabled={saving || !createForm.apiKeyId || !createForm.platformCode.trim()}>
              {saving ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
              Create and generate key
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(editing)} onOpenChange={(open) => { if (!open && !saving) setEditing(null) }}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>Edit audit identity binding</DialogTitle>
            <DialogDescription>Manage the caller identity and the check scope for this key; global block thresholds and review tiers are still managed by the unified GuardPipeline.</DialogDescription>
          </DialogHeader>
          <BindingFormFields form={editForm} setForm={setEditForm} apiKeys={apiKeys.filter((key) => key.id === editing?.api_key_id)} editing />
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditing(null)} disabled={saving}>Cancel</Button>
            <Button onClick={() => void updateBinding()} disabled={saving || !editForm.platformCode.trim()}>
              {saving ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />}
              Save changes
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(rotating)} onOpenChange={(open) => { if (!open && !saving) setRotating(null) }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>Rotate binding key for "{rotating?.platform_name || rotating?.platform_code}"</DialogTitle>
            <DialogDescription>The system will generate a new key. During the grace period both old and new keys verify, so the corresponding NewAPI config can be updated without interruption.</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <label className="space-y-1.5">
              <span className="text-sm font-medium">Old key grace period (seconds)</span>
              <Input type="number" min={60} max={86400} step={1} value={graceSeconds} onChange={(event) => setGraceSeconds(event.target.value)} />
              <span className="block text-xs leading-5 text-muted-foreground">300 seconds is recommended, minimum 60; even if the browser disconnects before the response returns, the old key can still be used to recover and rotate again.</span>
            </label>
            <div className="flex items-start gap-2 rounded-lg border border-amber-500/25 bg-amber-500/[0.06] p-3 text-sm leading-6">
              <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-600" />
              After a successful rotation, the new plaintext key is shown only once. Update the corresponding caller before the grace period ends; other bindings need no changes.
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRotating(null)} disabled={saving}>Cancel</Button>
            <Button onClick={() => void rotateSecret()} disabled={saving}>
              {saving ? <Loader2 className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
              Confirm rotation
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(secretReveal)} onOpenChange={(open) => { if (!open) requestCloseSecretReveal() }}>
        <DialogContent
          className="sm:max-w-2xl"
          onEscapeKeyDown={(event) => { event.preventDefault(); requestCloseSecretReveal() }}
          onPointerDownOutside={(event) => { event.preventDefault(); requestCloseSecretReveal() }}
        >
          <DialogHeader>
            <DialogTitle>{secretReveal?.platformName} binding key</DialogTitle>
            <DialogDescription>The key has been saved to the Codex2API database, but the plaintext is shown only in this response. The list will not show it again.</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="flex gap-2">
              <Input readOnly value={secretReveal?.secret ?? ''} className="font-mono text-xs" />
              <Button variant="outline" onClick={() => void copySecret()}>
                {secretCopied ? <Check className="size-4" /> : <Copy className="size-4" />}
                {secretCopied ? 'Copied' : 'Copy key'}
              </Button>
            </div>
            <div className="rounded-lg border border-amber-500/25 bg-amber-500/[0.06] p-3 text-sm leading-6 text-amber-800 dark:text-amber-200">
              Configure this key only in the NewAPI for "{secretReveal?.platformName}". After closing, the plaintext cannot be retrieved again; you can only rotate.
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={requestCloseSecretReveal}>Close</Button>
            <Button onClick={() => void copySecret()}>
              <Copy className="size-4" />{secretCopied ? 'Copied' : 'Copy and configure'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={secretCloseConfirmOpen} onOpenChange={setSecretCloseConfirmOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Confirm closing the key window?</DialogTitle>
            <DialogDescription>After closing, the plaintext display is cleared and the list will not show it again. A new key can only be obtained through rotation from now on.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setSecretCloseConfirmOpen(false)}>Go back to copy</Button>
            <Button variant="destructive" onClick={confirmCloseSecretReveal}>Confirm close</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {confirmDialog}
    </>
  )
}
