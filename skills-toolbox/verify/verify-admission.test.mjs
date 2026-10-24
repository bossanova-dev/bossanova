import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { classifyAdmission, storeAdmission } from './verify-admission.mjs'

const head = (letter) => letter.repeat(40)
const pr = (number, extra = {}) => ({
  number,
  headRefOid: head('a'),
  headRefName: 'dependabot/npm_and_yarn/pkg',
  body: '',
  ...extra,
})
const cron = (number, extra = {}) => pr(number, { headRefName: 'cron-test-123', ...extra })
const ticket = { identifier: 'BOS-1', attachments: [{ url: 'https://github.com/o/r/pull/4' }] }

test('admission classifies the admission seam without widening candidate selection', () => {
  const prs = [
    pr(1),
    cron(2),
    pr(3),
    pr(4),
    pr(5, { body: 'Linear issue: https://linear.app/demo/issue/BOS-2/title' }),
    cron(6),
    pr(7, { isDraft: true }),
    pr(8),
  ]
  const result = classifyAdmission({
    prs,
    linked: [
      { ticket, pr: prs[0] },
      { ticket: null, pr: prs[1] },
    ],
    tickets: [ticket],
    ticketlessSkipped: [
      { pr: 6, reason: 'no-session' },
      { pr: 8, reason: 'ambiguous-pr' },
    ],
    repo: 'o/r',
    admitsTicketless: true,
  })
  assert.deepEqual(
    result.map(({ pr, state, reason }) => ({ pr, state, reason })),
    [
      { pr: 1, state: 'admitted', reason: 'ticket' },
      { pr: 2, state: 'admitted', reason: 'ticketless-factory' },
      { pr: 3, state: 'excluded', reason: 'no-ticket' },
      { pr: 4, state: 'pending', reason: 'attached' },
      { pr: 5, state: 'pending', reason: 'ticket-not-in-review' },
      { pr: 6, state: 'pending', reason: 'no-session' },
      { pr: 7, state: 'pending', reason: 'draft' },
      { pr: 8, state: 'pending', reason: 'ambiguous-pr' },
    ],
  )
  assert.deepEqual(classifyAdmission({ prs: [cron(9)], admitsTicketless: false })[0], {
    pr: 9,
    head: head('a'),
    state: 'excluded',
    reason: 'selection',
  })
  for (const reason of ['session-has-ticket', 'claimed', 'unknown:claims', 'unknown:sessions']) {
    assert.equal(
      classifyAdmission({
        prs: [cron(9)],
        ticketlessSkipped: [{ pr: 9, reason }],
        admitsTicketless: true,
      })[0].state,
      'pending',
    )
  }
})

const at = '2026-10-24T00:00:00.000Z'
const entries = [
  { pr: 3550, head: head('A'), state: 'excluded', reason: 'no-ticket' },
  { pr: 3551, head: head('b'), state: 'admitted', reason: 'ticket' },
  { pr: 3552, head: head('c'), state: 'pending', reason: 'ticket-not-in-review' },
]

test('admission writer agrees with the shared version-one Go fixture', () => {
  const file = join(tempDir('admission-fixture-'), 'verify-admission.json')
  assert.equal(storeAdmission(file, 'default', entries, at), true)
  assert.equal(
    storeAdmission(
      file,
      'secondary',
      [entries[0], { pr: 3553, head: head('d'), state: 'excluded', reason: 'no-ticket' }],
      at,
    ),
    true,
  )
  assert.deepEqual(
    JSON.parse(fs.readFileSync(file, 'utf8')),
    JSON.parse(
      fs.readFileSync(
        new URL(
          '../../services/bossd/internal/verifyadmission/testdata/verify-admission.v1.json',
          import.meta.url,
        ),
        'utf8',
      ),
    ),
  )
})

test('admission writer replaces full scans and merges only the event PR in its scope', () => {
  const file = join(tempDir('admission-merge-'), 'verify-admission.json')
  storeAdmission(file, 'other', entries, at)
  storeAdmission(file, 'scope', entries, at)
  storeAdmission(
    file,
    'scope',
    [
      { ...entries[0], state: 'admitted' },
      { ...entries[1], state: 'excluded' },
    ],
    at,
    { eventPr: 3550 },
  )
  let data = JSON.parse(fs.readFileSync(file, 'utf8'))
  assert.equal(data.scopes.scope.prs[3550].state, 'admitted')
  assert.equal(data.scopes.scope.prs[3551].state, 'admitted')
  assert.equal(data.scopes.other.prs[3550].state, 'excluded')
  storeAdmission(file, 'scope', [entries[2]], at)
  data = JSON.parse(fs.readFileSync(file, 'utf8'))
  assert.deepEqual(Object.keys(data.scopes.scope.prs), ['3552'])
  assert.equal(Object.keys(data.scopes.other.prs).length, 3)
})

test('admission writer treats corrupt and other-version snapshots as empty and failures as false', () => {
  const dir = tempDir('admission-invalid-')
  const file = join(dir, 'snapshot.json')
  for (const content of ['bad json', JSON.stringify({ version: 2, scopes: { stale: {} } })]) {
    fs.writeFileSync(file, content)
    assert.equal(storeAdmission(file, 'scope', [], at), true)
    assert.deepEqual(Object.keys(JSON.parse(fs.readFileSync(file, 'utf8')).scopes), ['scope'])
  }
  assert.equal(storeAdmission(join(file, 'blocked'), 'scope', [], at), false)
  assert.equal(
    storeAdmission(file, 'scope', [], at, {
      fs: {
        ...fs,
        renameSync() {
          throw new Error('EACCES')
        },
      },
    }),
    false,
  )
  assert.deepEqual(fs.readdirSync(dir), ['snapshot.json'])
})
