import type { AccountRow, InviteRecipientRecord } from '../types'

export type InviteRecipientIndex = Record<string, InviteRecipientRecord>

const BLOCKED_INVITE_STATUSES = new Set(['unauthorized', 'error', 'banned'])

export function normalizeInviteEmail(email: string): string {
  return email.trim().toLowerCase()
}

// Invitee status lookups normalize emails the same way as the backend unique key: trim + lower, and duplicates are removed before the request is sent.
export function normalizeInviteEmails(emails: string[]): string[] {
  const seen = new Set<string>()
  const normalized: string[] = []
  for (const email of emails) {
    const key = normalizeInviteEmail(email)
    if (!key || seen.has(key)) continue
    seen.add(key)
    normalized.push(key)
  }
  return normalized
}

// The invited status only moves forward; merge server results or emails just sent successfully into the existing index.
export function mergeInviteRecipientIndex(
  current: InviteRecipientIndex,
  recipients: InviteRecipientRecord[],
): InviteRecipientIndex {
  if (recipients.length === 0) return current

  let next = current
  for (const recipient of recipients) {
    const key = normalizeInviteEmail(recipient.email)
    if (!key) continue
    if (next === current) next = { ...current }
    next[key] = { ...recipient, email: recipient.email.trim() }
  }
  return next
}

export function inviteRecipientRecord(
  index: InviteRecipientIndex,
  email: string,
): InviteRecipientRecord | undefined {
  return index[normalizeInviteEmail(email)]
}

export function alreadyInvitedEmails(
  emails: string[],
  index: InviteRecipientIndex,
): string[] {
  return emails
    .map((email) => email.trim())
    .filter((email) => Boolean(email) && Boolean(inviteRecipientRecord(index, email)))
}

// The two account pickers on the invite page share this visibility rule. Banned/error accounts and disabled accounts
// must not appear in the candidate list. Locked, rate-limited and other temporary scheduling states do not affect an account's role as an invitee,
// nor do they mean its referral credential has expired, so they stay in the list.
export function isInviteAccountSelectable(account: AccountRow): boolean {
  if (account.enabled === false) return false

  const status = (account.status || '').trim().toLowerCase()
  if (BLOCKED_INVITE_STATUSES.has(status)) return false

  return (account.health_tier || '').trim().toLowerCase() !== 'banned'
}

export function isCodexInviteSenderCandidate(account: AccountRow): boolean {
  if (!isInviteAccountSelectable(account)) return false

  // Relay accounts and AT-only accounts have no persistent Codex OAuth credential that can be used for referrals.
  return !account.at_only
}

export function inviteRecipientCandidates(
  rows: AccountRow[],
  excludeEmail?: string,
): AccountRow[] {
  const excluded = (excludeEmail ?? '').trim().toLowerCase()
  const seen = new Set<string>()
  const candidates: AccountRow[] = []

  for (const row of rows) {
    if (!isInviteAccountSelectable(row)) continue

    const email = row.email?.trim()
    if (!email) continue

    const key = email.toLowerCase()
    if (key === excluded || seen.has(key)) continue

    seen.add(key)
    candidates.push(row)
  }

  return candidates
}
