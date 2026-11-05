import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const settings = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
const en = JSON.parse(readFileSync(new URL('../locales/en.json', import.meta.url), 'utf8'))

const TABS = ['codex', 'appearance', 'general']

test('settings page is split into one panel per tab driven by ?tab=', () => {
  assert.match(settings, /useSearchParams\(\)/)
  assert.match(settings, /searchParams\.get\('tab'\)/)
  assert.match(settings, /type SettingsTabKey = 'codex' \| 'appearance' \| 'general'/)
  for (const tab of TABS) {
    assert.match(settings, new RegExp(`\\{ id: '${tab}', label: t\\('settings\\.nav\\.${tab}'\\)`), `tab pill ${tab}`)
    assert.match(settings, new RegExp(`\\{activeTab === '${tab}' \\? \\(`), `panel ${tab}`)
  }
  // The old scroll-anchor navigation was removed to avoid regressing to one long scrolling page.
  assert.doesNotMatch(settings, /scrollToSection/)
  assert.doesNotMatch(settings, /settingsSections/)
  assert.match(settings, /role="tablist"/)
  assert.match(settings, /role="tab"/)
})

test('legacy #settings-* anchors map onto a tab', () => {
  for (const id of ['settings-overview', 'settings-traffic', 'settings-runtime', 'settings-models', 'settings-appearance']) {
    assert.match(settings, new RegExp(`'${id}': '(${TABS.join('|')})'`), id)
  }
})

test('channel-specific cards live in their channel tab, shared cards in general', () => {
  const panel = (tab) => {
    const start = settings.indexOf(`{activeTab === '${tab}' ? (`)
    const next = TABS.map((t) => settings.indexOf(`{activeTab === '${t}' ? (`)).filter((i) => i > start)
    const end = next.length ? Math.min(...next) : settings.length
    assert.ok(start > 0, `panel ${tab}`)
    return settings.slice(start, end)
  }
  const codex = panel('codex')
  for (const key of ['settings.probeScheduling', 'settings.globalAutoPauseTitle', 'settings.codexWebsocket', 'settings.codexCompatToggles', 'settings.codexOverloadPause', 'settings.responseCache.title', 'settings.codexClientTitle', 'settings2.codexModelMapping']) {
    assert.ok(codex.includes(key), `codex tab should contain ${key}`)
  }
  assert.ok(codex.includes('codex_user_agent_config') || codex.includes('codexUserAgentConfig'))
  assert.ok(!codex.includes('settings.usageLogMode'), 'usage log settings are shared, not Codex-only')
  const appearance = panel('appearance')
  assert.ok(appearance.includes('settings.display') && appearance.includes('settings.backgroundImage'))
  const general = panel('general')
  for (const key of ['settings.systemStatus', 'settings.trafficProtection', 'settings.modelCooldownTitle', 'settings.schedulingStrategy', 'settings.runtimeOptimization', 'settings.usageLogMode', 'settings.githubAccess', 'settings.imageStorage', 'settings.security', 'settings.apiEndpoints']) {
    assert.ok(general.includes(key), `general tab should contain ${key}`)
  }
  assert.ok(!general.includes('settings.codexUserAgentRaw'), 'Codex UA emulation must not stay in general runtime card')
})

test('tab and section labels exist in en', () => {
  for (const locale of [en]) {
    for (const key of ['codex', 'codexDesc', 'appearance', 'general', 'generalDesc', 'codexQuota', 'codexQuotaDesc', 'codexTransport', 'codexTransportDesc', 'codexClient', 'codexClientDesc', 'codexImages', 'codexImagesDesc']) {
      assert.equal(typeof locale.settings?.nav?.[key], 'string', `settings.nav.${key}`)
    }
    assert.equal(typeof locale.settings?.codexClientTitle, 'string')
    assert.equal(typeof locale.settings?.codexClientDesc, 'string')
    for (const key of ['codexImagesDriver', 'codexImagesDriverDesc', 'codexImagesMainModel', 'codexImagesMainModelDesc', 'codexImagesDefault', 'codexImagesScope', 'codexImagesFallback']) {
      assert.equal(typeof locale.settings?.[key], 'string', `settings.${key}`)
    }
  }
})

test('codex tab points at the shared scheduling strategy in general', () => {
  const start = settings.indexOf("{activeTab === 'codex' ? (")
  const end = settings.indexOf("{activeTab === 'appearance' ? (")
  const codex = settings.slice(start, end)
  assert.match(codex, /selectTab\('general', 'settings-traffic'\)/)
  assert.match(codex, /settings\.codexSchedulingHintAction/)
  assert.match(settings, /pendingSectionRef/)
  for (const locale of [en]) {
    for (const key of ['codexSchedulingHintTitle', 'codexSchedulingHintDesc', 'codexSchedulingHintAction']) {
      assert.equal(typeof locale.settings?.[key], 'string', `settings.${key}`)
    }
  }
})

test('settings has no provider tabs, provider cards or channel scope badges left', () => {
  assert.doesNotMatch(settings, /ChannelScopeBadges|ALL_UPSTREAM_CHANNELS|CHANNELS_(CODEX|RELAY|STREAMING)/)
  assert.doesNotMatch(settings, /VisibleChannelsPicker|ClaudeCodeSettingsCard|antigravityOAuth|grokSettings/)
  for (const locale of [en]) {
    assert.doesNotMatch(JSON.stringify(locale.settings.nav), /claude|antigravity|grok/i)
    assert.doesNotMatch(locale.settings.schedulerModeDesc, /Claude|Grok|Antigravity/)
  }
  assert.match(settings, /title=\{t\('settings\.modelCooldownTitle'\)\}/)
})

test('multi-section tabs render a section index that mirrors the rendered sections', () => {
  assert.match(settings, /const SETTINGS_TAB_SECTION_INDEX: Record<SettingsTabKey/)
  assert.match(settings, /function useActiveSettingsSection\(/)
  assert.match(settings, /<SettingsSectionIndex\n/)
  const start = settings.indexOf('const SETTINGS_TAB_SECTION_INDEX')
  const end = settings.indexOf('\n}\n', start)
  const index = settings.slice(start, end)
  for (const id of index.match(/id: '(settings-[a-z-]+)'/g).map((m) => m.slice(5, -1))) {
    assert.match(settings, new RegExp(`<SettingsSection id="${id}"`), `section index entry ${id} must point at a rendered section`)
  }
  for (const tab of TABS) {
    assert.match(index, new RegExp(`\\b${tab}: \\[`), `section index for ${tab}`)
  }
  // The tab bar sticks to the top in page flow and no longer floats fixed over the content.
  assert.doesNotMatch(settings, /fixed left-1\/2 top-\[max\(0\.625rem/)
  assert.match(settings, /sticky top-2[\s\S]{0,400}role="tablist"/)
})

test('manual-save fields are tracked against the persisted snapshot', () => {
  assert.match(settings, /const \[persistedSettings, setPersistedSettings\] = useState<SystemSettings \| null>/)
  assert.match(settings, /setPersistedSettings\(commitSettingsForm\(settings\)\)/, 'load must seed the snapshot')
  assert.match(settings, /setPersistedSettings\(commitSettingsForm\(safeMerged\)\)/, 'manual save must refresh the snapshot')
  assert.match(settings, /markPersisted\(getSettingsPatchValues\(optimistic, patchKeys\)\)/, 'auto-save must merge only its own keys')
  assert.match(settings, /\{dirtyCount > 0 \? \(/, 'bottom save bar only renders with unsaved changes')
  assert.match(settings, /<SaveStatusPill autoSaveStatus=\{autoSaveStatus\} dirtyCount=\{dirtyCount\} \/>/)
  for (const locale of [en]) {
    for (const key of ['saveStatusSaved', 'saveStatusUnsaved', 'saveStatusUnsavedHint', 'discardChanges', 'sectionIndex']) {
      assert.equal(typeof locale.settings?.[key], 'string', `settings.${key}`)
    }
    assert.match(locale.settings.saveStatusUnsaved, /\{\{n\}\}/)
  }
})

test('single-toggle compatibility settings are one row-list card, not three narrow cards', () => {
  const start = settings.indexOf("title={t('settings.codexCompatToggles')}")
  const end = settings.indexOf('</SettingsCard>', start)
  const card = settings.slice(start, end)
  assert.ok(start > 0)
  assert.match(card, /className=\{SETTINGS_ROW_LIST\}/)
  for (const key of ['overflowAutoCompact', 'compactViaResponses', 'codexPreflightSSEPassthrough']) {
    assert.match(card, new RegExp(`label=\\{t\\('settings\\.${key}'\\)\\}\\n\\s+description=\\{t\\('settings\\.${key}Desc'\\)\\}\\n\\s+help=\\{t\\('settings\\.${key}EnabledDesc'\\)\\}\\n\\s+layout="row"`), key)
  }
  // A single switch in a two-column switch grid collapses to half width and wraps: count each grid up to the </div> at the same indent.
  const lines = settings.split('\n')
  lines.forEach((line, i) => {
    if (!line.includes('className={SETTINGS_SWITCH_GRID}')) return
    const indent = line.length - line.trimStart().length
    let switches = 0
    let fields = 0
    for (let j = i + 1; j < lines.length; j++) {
      const l = lines[j]
      if (l.trim() === '</div>' && l.length - l.trimStart().length === indent) break
      if (l.includes('<SettingField')) fields++
      if (l.includes('layout="switch"')) switches++
    }
    assert.ok(fields > 1, `line ${i + 1}: a lone switch must use SETTINGS_SWITCH_ROW, not SETTINGS_SWITCH_GRID (${switches} switch, ${fields} field)`)
  })
  for (const locale of [en]) {
    assert.equal(typeof locale.settings.codexCompatToggles, 'string')
    assert.equal(typeof locale.settings.codexCompatTogglesDesc, 'string')
  }
})
