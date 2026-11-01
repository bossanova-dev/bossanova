import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  POST_FAILURE_PARK_AFTER,
  notePostFailure,
  clearPostFailure,
  readSweepEntries,
  writeSweepEntries,
} from './verify-post-failures.mjs'

test('post failures count one head, reset on a new head and clear on success', () => {
  const entries = { quiet: { fingerprint: 'quiet', at: 'before' } }
  for (let count = 1; count <= POST_FAILURE_PARK_AFTER; count++)
    assert.equal(notePostFailure(entries, 'o/r', 3, 'a', 'exit-2', 'now').count, count)
  assert.equal(notePostFailure(entries, 'o/r', 3, 'b', 'exit-1', 'later').count, 1)
  assert.equal(notePostFailure(entries, 'other/r', 3, 'b', 'exit-1', 'later').count, 1)
  clearPostFailure(entries, 'o/r', 3)
  assert.equal(entries['post-failure:o/r#3'], undefined)
  assert.deepEqual(entries.quiet, { fingerprint: 'quiet', at: 'before' })
})

test('state round trips preserve quiet fingerprints alongside post failures', () => {
  const dir = tempDir('verify-post-state-')
  try {
    const file = join(dir, 'state.json')
    const entries = { scope: { fingerprint: 'tick', checkedAt: 42 } }
    notePostFailure(entries, 'o/r', 3, 'head', 'error', 'now')
    assert.equal(writeSweepEntries(fs, file, entries), true)
    assert.deepEqual(readSweepEntries(fs, file), entries)
    const reread = readSweepEntries(fs, file)
    clearPostFailure(reread, 'o/r', 3)
    writeSweepEntries(fs, file, reread)
    assert.deepEqual(readSweepEntries(fs, file), { scope: entries.scope })
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})
