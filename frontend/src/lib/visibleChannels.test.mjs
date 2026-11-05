import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { normalizeVisibleChannels, toggleVisibleChannel } from './visibleChannels.ts'

test('normalizeVisibleChannels only ever returns the Codex channel', () => {
  assert.deepEqual(normalizeVisibleChannels(null), ['codex'])
  assert.deepEqual(normalizeVisibleChannels(undefined), ['codex'])
  assert.deepEqual(normalizeVisibleChannels([]), ['codex'])
  assert.deepEqual(normalizeVisibleChannels(['grok']), ['codex'])
  assert.deepEqual(normalizeVisibleChannels([' Codex ', '', 'claude', 'openai', 42]), ['codex'])
  assert.deepEqual(normalizeVisibleChannels('codex'), ['codex'])
})

test('toggleVisibleChannel never drops the fallback channel', () => {
  assert.deepEqual(toggleVisibleChannel(['codex'], 'codex'), ['codex'])
  assert.deepEqual(toggleVisibleChannel(['codex'], 'grok'), ['codex'])
})

test('dashboard, accounts and the usage channel filter consume the visibility setting', () => {
  const read = (p) => readFileSync(new URL(p, import.meta.url), 'utf8')
  assert.match(read('../App.tsx'), /<VisibleChannelsProvider>/)
  assert.match(read('../pages/Dashboard.tsx'), /useVisibleChannels\(\)/)
  assert.match(read('../pages/Accounts.tsx'), /useVisibleChannels\(\)/)
  assert.match(read('../pages/QualityTest.tsx'), /useVisibleChannels\(\)/)
  assert.match(read('../components/ChannelFilter.tsx'), /useVisibleChannels\(\)/)
  // 直接打开被隐藏渠道的账号路由要回落到 Codex，而不是渲染一个切换器里不存在的视图。
  assert.match(read('../pages/Accounts.tsx'), /isChannelVisible\(providerView\)/)
})
