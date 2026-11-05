import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const api = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const types = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
const en = JSON.parse(readFileSync(new URL('../locales/en.json', import.meta.url), 'utf8'))

test('risk profiles have independent list and event pagination', () => {
  assert.match(source, /usePersistedPageSize\('prompt_risk_profiles'/)
  assert.match(source, /usePersistedPageSize\('prompt_risk_profile_events'/)
  assert.match(source, /usePersistedPageSize\('prompt_risk_profile_trust_events'/)
  assert.match(api, /event_page=\$\{eventPage\}/)
  assert.match(api, /trust_event_page=\$\{trustEventPage\}/)
  assert.match(source, /page=\{eventPage\}[\s\S]*totalItems=\{detail\?\.event_total \?\? 0\}/)
  assert.match(source, /page=\{trustEventPage\}[\s\S]*totalItems=\{detail\?\.trust_event_total \?\? 0\}/)
})

test('risk profile identity boundaries and operational guardrail are visible', () => {
  for (const subject of ['newapi_user', 'session', 'api_key', 'client_ip', 'upstream_account']) {
    assert.match(types, new RegExp(subject))
    assert.equal(typeof en.promptFilter.risk.subjects[subject], 'string')
  }
  assert.match(source, /profile\.is_person/)
  assert.match(source, /identity_confidence/)
  assert.match(en.promptFilter.risk.guardrail, /never turn a currently safe request into a block by themselves/)
})

test('risk profile API supports filters and exact subject detail', () => {
  assert.match(api, /getPromptRiskProfiles/)
  assert.match(api, /subject_type/)
  assert.match(api, /risk_level/)
  assert.match(api, /min_score/)
  assert.match(api, /getPromptRiskProfile/)
  assert.match(api, /encodeURIComponent\(subjectType\)/)
  assert.match(api, /encodeURIComponent\(subjectKey\)/)
})

test('risk profiles offer quick CY and activity filters', () => {
  assert.match(api, /cy_only/)
  assert.match(api, /activity_state/)
  assert.match(source, /cyOnly/)
  assert.match(source, /activityState/)
  assert.match(source, /upstreamCYProfiles/)
  assert.equal(typeof en.promptFilter.risk.upstreamCYProfiles, 'string')
})

test('risk profile rows expose an explicit freeze class and identity summary', () => {
  assert.match(source, /promptRiskFreezeClass/)
  assert.match(source, /promptRiskIdentityKind/)
  assert.match(source, /risk\.freezeStatus/)
  assert.match(source, /risk\.identitySource/)
  assert.match(en.promptFilter.risk.freezeStates.none, /Not frozen/)
  assert.match(source, /pageSummary/)
  assert.equal(typeof en.promptFilter.risk.summary.highCritical, 'string')
})

test('detail dialogs keep the close control fixed while the body scrolls', () => {
  assert.match(source, /flex max-h-\[90vh\] flex-col overflow-hidden/)
  assert.match(source, /min-h-0 flex-1 overflow-y-auto/)
  assert.match(source, /shrink-0 pr-8/)
})

test('person profiles expose auditable temporary adaptive trust without a permanent allowlist', () => {
  assert.match(source, /subjectType: 'newapi_user'/)
  assert.match(source, /upsertPromptRiskTrust/)
  assert.match(source, /revokePromptRiskTrust/)
  assert.match(api, /\/trust/)
  assert.match(types, /PromptRiskTrustPolicy/)
  assert.match(types, /trust_events/)
  assert.match(types, /model_review_count/)
  assert.match(source, /trust_policy\.source/)
  assert.match(source, /last_model_review_at/)
  assert.match(en.promptFilter.risk.trust.description, /synchronous DS review/)
  assert.match(en.promptFilter.risk.trust.dialogDescription, /not a permanent allowlist/)
  assert.match(en.promptFilter.risk.trust.safetyHint, /CY/)
})

test('adaptive review basis and linked model-review audit are visible', () => {
  assert.match(types, /PromptRiskAdaptiveReviewBasis/)
  assert.match(types, /request_id_hash\?: string/)
  assert.match(source, /adaptive_review_basis/)
  assert.match(source, /clean_review_count/)
  assert.match(source, /next_forced_review_at/)
  assert.match(source, /request_id_hash/)
  assert.match(source, /prompt_preview/)
  assert.match(source, /incident_id/)
  assert.match(en.promptFilter.risk.trust.basisDescription, /why/i)
  assert.match(en.promptFilter.risk.trust.history, /Model-review decisions/)
})

test('risk page separates people from environment subjects', () => {
  assert.match(source, /peopleProfiles/)
  assert.match(source, /allObjects/)
  assert.match(en.promptFilter.risk.nonPersonHint, /environment subjects/)
})

test('active CYB conversation locks and user cooldowns are visible and manually unlockable', () => {
  assert.match(types, /PromptConversationLock/)
  assert.match(types, /conversation_lock\?: PromptConversationLock/)
  assert.match(api, /unlockPromptConversation/)
  assert.match(api, /conversation-locks\/\$\{encodeURIComponent\(lockKey\)\}\/unlock/)
  assert.match(source, /conversation_lock\?\.status === 'active'/)
  assert.match(source, /unlockPromptConversation\(lock\.lock_key[\s\S]*scope\)/)
  assert.match(source, /user_cooldown/)
  assert.match(source, /confirmUserCooldown/)
  assert.match(source, /remaining_seconds/)
  assert.match(types, /restriction_scope\?: 'conversation' \| 'user_cooldown' \| 'fingerprint_replay'/)
  assert.match(en.promptFilter.risk.conversationLock.description, /do not add strikes/)
  assert.match(en.promptFilter.risk.conversationLock.userCooldownDescription, /All sessions/)
  assert.match(source, /fingerprint_replay/)
  assert.match(en.promptFilter.risk.conversationLock.fingerprintReplayDescription, /same Prompt fingerprint/)
  assert.match(source, /unlockReasonPrompt/)
})

test('live restrictions are prioritized, filterable, and link to the original audit', () => {
  assert.match(api, /locked_only/)
  assert.match(source, /lockedOnly: filters\.lockedOnly/)
  assert.match(source, /promptFilter\.risk\.lockedProfiles/)
  assert.match(source, /auditReference = activeRestriction\?\.incident_id/)
  assert.match(source, /\/prompt-filter\/logs\?audit=/)
  assert.match(source, /searchParams\.get\('audit'\)/)
  assert.match(source, /q: auditReference/)
  assert.match(en.promptFilter.risk.lockedProfilesHint, /newest lock first/)
  assert.match(en.promptFilter.risk.conversationLock.openAudit, /original audit/)
})
