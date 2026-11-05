import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const en = JSON.parse(readFileSync(new URL('../locales/en.json', import.meta.url), 'utf8'))
const operations = readFileSync(new URL('../pages/Operations.tsx', import.meta.url), 'utf8')
const settings = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')

test('scheduler engine choices use English labels', () => {
  assert.deepEqual(
    [en.settings.schedulerEngineLegacy, en.settings.schedulerEngineShadow, en.settings.schedulerEngineIndexed],
    ['Legacy', 'Shadow', 'Indexed'],
  )

  for (const locale of [en.settings]) {
    assert.ok(locale.schedulerEngineLegacyDesc.length > 20)
    assert.match(locale.schedulerEngineShadowDesc, /64/)
    assert.ok(locale.schedulerEngineIndexedDesc.length > 20)
  }
  assert.match(en.settings.schedulerEngineCompatibilityNote, /Fast Scheduler/)
  assert.match(en.settings.schedulerEngineCompatibilityNote, /Off maps to Legacy/)
  assert.match(en.settings.schedulerEngineCompatibilityNote, /On maps to Indexed/)
  assert.match(settings, /schedulerEngineExplanations\.map/)
  assert.match(settings, /settings\.schedulerEngineCompatibilityNote/)
  assert.match(settings, /function SettingsCollapsibleNote/)
  assert.match(settings, /role="note"/)
  assert.match(settings, /aria-current=\{active \? 'true' : undefined\}/)
})

test('operations overview localizes the scheduler engine value', () => {
  assert.match(operations, /value=\{formatSchedulerEngine\(overview\.scheduler\.engine, t\)\}/)
  assert.match(operations, /case 'legacy':[\s\S]*settings\.schedulerEngineLegacy/)
  assert.match(operations, /case 'shadow':[\s\S]*settings\.schedulerEngineShadow/)
  assert.match(operations, /case 'indexed':[\s\S]*settings\.schedulerEngineIndexed/)
})

test('scheduling cards keep their own content height on desktop', () => {
  assert.match(settings, /className="grid auto-rows-min items-start gap-4 lg:grid-cols-2"/)
  assert.equal(
    settings.match(/h-fit space-y-3 rounded-xl border border-border\/60 bg-muted\/10 p-3.5/g)?.length,
    2,
  )
})
