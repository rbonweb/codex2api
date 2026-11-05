import assert from 'node:assert/strict'
import test from 'node:test'

import { resolveByteLimitCommit } from './byteLimitCommit.ts'

const KIB = 1024
const MIB = 1024 * 1024

// Replicates the commitByteLimitField decision on the Settings page: only submit=true counts as a real write.
function makeCommitHandler(persistedBytes, bytesPerUnit) {
  const writes = []
  const commit = (submittedUnits) => {
    const decision = resolveByteLimitCommit(persistedBytes, submittedUnits, bytesPerUnit)
    if (decision.submit) writes.push(decision.bytes)
  }
  return { commit, writes }
}

test('unedited blur keeps the persisted non-multiple byte value', () => {
  // 131073 bytes is shown as 128 KiB in the UI; writing the displayed value back on blur would silently change it to 131072.
  const decision = resolveByteLimitCommit(131073, 128, KIB)
  assert.deepEqual(decision, { submit: false, bytes: 131073 })
})

test('unedited blur never submits and edited blur submits exactly once', () => {
  const { commit, writes } = makeCommitHandler(131073, KIB)

  // Not edited: the displayed value is still 128 KiB, even though the form was already changed to 131072 in onValueChange.
  commit(128)
  assert.deepEqual(writes, [], 'unedited blur must not persist a rounded value')

  // The user actually changed it to 129 KiB: submit only once, and the submitted value is the accurate 129 KiB.
  commit(129)
  assert.deepEqual(writes, [129 * KIB])
})

test('an exactly aligned byte value also skips the redundant write', () => {
  assert.deepEqual(resolveByteLimitCommit(131072, 128, KIB), {
    submit: false,
    bytes: 131072,
  })
})

test('MiB byte limits keep their raw persisted value on unedited blur', () => {
  const persisted = 5 * MIB + 1

  assert.deepEqual(resolveByteLimitCommit(persisted, 5, MIB), {
    submit: false,
    bytes: persisted,
  })
  assert.deepEqual(resolveByteLimitCommit(persisted, 6, MIB), {
    submit: true,
    bytes: 6 * MIB,
  })
})

test('no persisted baseline yet still submits the typed value', () => {
  assert.deepEqual(resolveByteLimitCommit(null, 128, KIB), {
    submit: true,
    bytes: 128 * KIB,
  })
  assert.deepEqual(resolveByteLimitCommit(undefined, 2, MIB), {
    submit: true,
    bytes: 2 * MIB,
  })
})
