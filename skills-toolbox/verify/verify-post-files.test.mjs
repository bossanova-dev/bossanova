import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { buildBlockers } from './verify-blockers.mjs'
import { verdictFileArgs } from './verify-post-files.mjs'

const HEAD = 'e'.repeat(40)
const notApplicableNotes = [{ extension: 'gated', note: 'gated: nothing to check' }]
const blockers = buildBlockers({
  reviewFindings: [
    {
      severity: 'P2',
      verified: true,
      introduced: true,
      fixed: false,
      title: 'Broken branch',
      file: 'src/a.mjs',
      line: 2,
      evidence: 'returns the wrong value',
    },
  ],
})
const fileArgs = (t, judged, io = fs) => {
  const logDir = tempDir('verify-post-files-', t)
  return { logDir, args: verdictFileArgs({ fs: io, logDir, pr: 7, head: HEAD, judged }) }
}
const flagFile = (list, flag) => (list.includes(flag) ? list[list.indexOf(flag) + 1] : null)

test('a noted pass, blocked, defect or human verdict writes the file and passes its flag', (t) => {
  for (const verdict of ['pass', 'blocked', 'defect', 'human']) {
    const judged = { verdict, notApplicableNotes, ...(verdict === 'blocked' ? { blockers } : {}) }
    const { logDir, args } = fileArgs(t, judged)
    assert.notEqual(args, null, verdict)
    const file = flagFile(args, '--not-applicable')
    assert.equal(file, join(logDir, `boss-verify-not-applicable-7-${HEAD}.json`), verdict)
    assert.deepEqual(JSON.parse(fs.readFileSync(file, 'utf8')), notApplicableNotes)
  }
})

test('no not-applicable file on wait or claim, or for an empty or missing list', (t) => {
  for (const judged of [
    { verdict: 'wait', notApplicableNotes },
    { verdict: 'claim', notApplicableNotes },
    { verdict: 'pass', notApplicableNotes: [] },
    { verdict: 'pass' },
  ]) {
    const { logDir, args } = fileArgs(t, judged)
    assert.deepEqual(args, [], JSON.stringify(judged))
    assert.deepEqual(fs.readdirSync(logDir), [])
  }
})

test('an unwritable not-applicable file drops only its flag', (t) => {
  const failing = {
    ...fs,
    writeFileSync: (file, ...rest) => {
      if (String(file).includes('not-applicable')) throw new Error('EACCES')
      return fs.writeFileSync(file, ...rest)
    },
  }
  const findingNotes = [{ title: 'Polish', severity: 'P3' }]
  const { args } = fileArgs(t, { verdict: 'pass', notApplicableNotes, findingNotes }, failing)
  assert.notEqual(args, null)
  assert.equal(args.includes('--not-applicable'), false)
  assert.equal(args[0], '--notes')
  assert.ok(fs.existsSync(args[1]))
})
