import assert from 'node:assert/strict'
import test from 'node:test'

import {
  emptyModelMappingEntries,
  parseModelMappingEntries,
  serializeModelMappingEntries,
} from './modelMapping.ts'

test('model mapping entries round-trip exact and wildcard aliases', () => {
  const parsed = parseModelMappingEntries(
    '{"gpt-5.5":"gpt-5.5-codex","gpt-5.*":"gpt-5.6-codex"}',
  )
  assert.equal(parsed.ok, true)
  assert.deepEqual(parsed.entries, [
    { from: 'gpt-5.5', to: 'gpt-5.5-codex' },
    { from: 'gpt-5.*', to: 'gpt-5.6-codex' },
  ])
  assert.deepEqual(serializeModelMappingEntries(parsed.entries), {
    ok: true,
    value: '{"gpt-5.5":"gpt-5.5-codex","gpt-5.*":"gpt-5.6-codex"}',
  })
})

test('model mapping parser rejects invalid JSON and non-string targets', () => {
  assert.deepEqual(parseModelMappingEntries('{bad json'), { ok: false })
  assert.deepEqual(parseModelMappingEntries('{"gpt-5.5":42}'), { ok: false })
})

test('model mapping serializer ignores blank rows and rejects partial or duplicate sources', () => {
  assert.deepEqual(serializeModelMappingEntries(emptyModelMappingEntries()), {
    ok: true,
    value: '',
  })
  assert.deepEqual(
    serializeModelMappingEntries([{ from: 'gpt-5.5', to: '' }]),
    { ok: false },
  )
  assert.deepEqual(
    serializeModelMappingEntries([
      { from: 'gpt-5.5', to: 'gpt-5.5-codex' },
      { from: 'GPT-5.5', to: 'gpt-5.6-codex' },
    ]),
    { ok: false },
  )
})
