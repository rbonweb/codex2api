import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { extractBalancedBody } from './sourceBoundary.mjs'

const componentSource = readFileSync(new URL('../components/PromptFilterNewAPIBindings.tsx', import.meta.url), 'utf8')
const pageSource = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const typesSource = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')

test('identity binding only narrows prompt scope without overriding GuardPipeline thresholds', () => {
  assert.equal(componentSource.includes('requireSignedIdentity: false'), true)
  assert.equal(componentSource.includes('Global thresholds and review tiers are still managed by GuardPipeline'), true)
  for (const fragment of ['promptFilterScope', "'inherit'", "'local_only'", "'off'", 'Local detection only: skip remote model review']) {
    assert.equal(componentSource.includes(fragment), true, `binding editor missing prompt scope control: ${fragment}`)
  }
  for (const fragment of ['policyMode', 'policyProfile', 'policy_mode', 'policy_profile', 'Policy mode']) {
    assert.equal(componentSource.includes(fragment), false, `binding editor still exposes policy control: ${fragment}`)
  }
  const bindingStart = typesSource.indexOf('export interface PromptFilterNewAPIBinding')
  const bindingEnd = typesSource.indexOf('export interface CreateAPIKeyRequest')
  assert.ok(bindingStart >= 0, 'missing PromptFilterNewAPIBinding interface')
  assert.ok(bindingEnd > bindingStart, 'missing CreateAPIKeyRequest boundary after PromptFilterNewAPIBinding')
  const bindingTypes = typesSource.slice(bindingStart, bindingEnd)
  assert.equal(bindingTypes.includes('prompt_filter_scope'), true)
  assert.equal(bindingTypes.includes('policy_mode'), false)
  assert.equal(bindingTypes.includes('policy_profile'), false)
})

test('platform binding list never reads plaintext secret from a persisted binding', () => {
  assert.equal(componentSource.includes('binding.secret_masked'), true)
  assert.equal(componentSource.includes('binding.secret}'), false)
  assert.equal(componentSource.includes('binding.secret ??'), false)
  assert.equal(componentSource.includes('getPromptFilterNewAPISecret'), false)
})

test('key binding UI documents one-to-one isolation and one-time secret reveal', () => {
  for (const fragment of ['One key can be bound to only one caller', 'a key with no binding does not accept NewAPI signed identities', 'up to 32 characters, and globally unique', 'maxLength={32}', 'the plaintext is shown only in this response', 'Confirm closing the key window?', 'min={60}', 'the old key can still be used to recover']) {
    assert.equal(componentSource.includes(fragment), true, `missing isolation warning: ${fragment}`)
  }
  for (const deploymentSpecificCopy of ['Fanren', 'buycodekey', 'e.g. fanren']) {
    assert.equal(componentSource.includes(deploymentSpecificCopy), false, `deployment-specific copy leaked into generic UI: ${deploymentSpecificCopy}`)
  }
})

test('key bindings are embedded in an optional NewAPI adapter panel', () => {
  const panelIndex = pageSource.indexOf("t('promptFilter.newapiAdapterSummary')")
  const bindingIndex = pageSource.indexOf('<PromptFilterNewAPIBindings />')
  const expertIndex = pageSource.indexOf("t('promptFilter.expertSettingsSummary')")

  assert.ok(panelIndex >= 0, 'missing optional NewAPI adapter panel')
  assert.ok(bindingIndex > panelIndex, 'Key bindings must be rendered inside the NewAPI integration panel')
  assert.ok(expertIndex > bindingIndex, 'Key bindings must remain inside the NewAPI panel before expert settings')
  assert.equal(pageSource.match(/<PromptFilterNewAPIBindings \/>/g)?.length, 1)
  assert.equal(componentSource.includes('<Card'), false, 'embedded binding editor must not create a second top-level card')
  assert.equal(pageSource.includes('getPromptFilterNewAPISecret'), false)
  assert.equal(pageSource.includes("setBool('newapi', 'enabled'"), false)
  const recommendedPreset = extractBalancedBody(
    pageSource,
    'const applyRecommendedProtection',
    'applyRecommendedProtection body',
  )
  for (const fragment of [
    "recommendedStrength === 'penalty'",
    "t('promptFilter.penaltyRequiresNewAPI')",
    'prompt_filter_review_enabled: true',
  ]) {
    assert.equal(recommendedPreset.includes(fragment), false, `generic local preset still contains a deployment-specific dependency: ${fragment}`)
  }
})

test('enabling required signatures is protected by a 401 safety confirmation', () => {
  for (const fragment of [
    '!editing.require_signed_identity && editForm.requireSignedIdentity',
    'Enable required signed identity?',
    'will immediately return 401',
    'it is not recorded as a Prompt violation and does not trigger violation penalties',
    'if (!approved) return',
  ]) {
    assert.equal(componentSource.includes(fragment), true, `missing required-signature safety guard: ${fragment}`)
  }
})

test('platform binding API client covers CRUD and secret rotation endpoints', () => {
  for (const fragment of [
    "'/prompt-filter/newapi-bindings'",
    '/secret/generate',
    "method: 'PATCH'",
    "method: 'DELETE'",
  ]) {
    assert.equal(apiSource.includes(fragment), true, `missing binding API operation: ${fragment}`)
  }
})
