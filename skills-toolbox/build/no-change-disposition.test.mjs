import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  NO_CHANGE_BREADCRUMB_PREFIX,
  decideNoChangeDisposition,
  parsePriorBreadcrumbs,
  runCli,
} from './no-change-disposition.mjs'

const helper = fileURLToPath(new URL('./no-change-disposition.mjs', import.meta.url))
const config = {
  adapters: { tracker: 'linear' },
  trackerConfig: { linear: { states: { inProgress: 'Building', done: 'Delivered' } } },
  acceptance: { fullGateCommands: ['make lint', 'make test'] },
}
const parkConfig = {
  ...config,
  trackerConfig: {
    linear: {
      ...config.trackerConfig.linear,
      labels: { needsHuman: 'needs-human' },
    },
  },
}
const criterion = {
  id: 'AC1',
  check: 'node --test scope.test.mjs',
  result: 'met',
  satisfiedBy: null,
  retried: false,
}
const input = () => ({
  claimVerdict: 'WON',
  currentState: 'Building',
  head: 'head',
  tree: 'tree',
  baseTree: 'tree',
  worktreeClean: true,
  acceptance: { head: 'head', tree: 'tree', criteria: [{ ...criterion }] },
})

test('closes only independently certified scope on the owned clean base tree', () => {
  assert.deepEqual(decideNoChangeDisposition(input(), config), {
    disposition: 'close',
    reason: 'already-satisfied',
    targetState: 'Delivered',
    criteria: [criterion],
    breadcrumb: 'boss-build-no-change: close (already-satisfied)',
  })
})

for (const [name, patch, reason] of [
  ['base moved', { baseTree: 'moved' }, 'base-tree-moved'],
  ['base unreadable', { baseTree: null }, 'base-tree-moved'],
  ['claim unknown', { claimVerdict: 'unknown' }, 'claim-not-won'],
  ['no winner', { claimVerdict: 'NO_WINNER' }, 'claim-not-won'],
  ['report absent', { acceptance: null }, 'no-certification'],
  ['report reused', { acceptance: { ...input().acceptance, reused: true } }, 'certification-stale'],
  [
    'report head stale',
    { acceptance: { ...input().acceptance, head: 'old' } },
    'certification-stale',
  ],
  [
    'report tree stale',
    { acceptance: { ...input().acceptance, tree: 'old' } },
    'certification-stale',
  ],
  ['worktree dirty', { worktreeClean: false }, 'certification-stale'],
  ['head unknown', { head: null }, 'certification-stale'],
  ['empty criteria', { acceptance: { ...input().acceptance, criteria: [] } }, 'criterion-not-met'],
  [
    'malformed criteria',
    { acceptance: { ...input().acceptance, criteria: null } },
    'criterion-not-met',
  ],
]) {
  test(`restores when ${name}`, () => {
    const result = decideNoChangeDisposition({ ...input(), ...patch }, config)
    assert.equal(result.disposition, 'restore')
    assert.equal(result.reason, reason)
    assert.equal(result.targetState, null)
  })
}

for (const patch of [
  { result: 'unmet' },
  { result: 'unchecked', reason: 'budget-spent' },
  { satisfiedBy: 'ci-wait' },
  { retried: true },
  { retried: undefined },
  { satisfiedBy: undefined },
  { check: '' },
]) {
  test(`rejects uncertified criterion ${JSON.stringify(patch)}`, () => {
    const value = input()
    value.acceptance.criteria.push({ ...criterion, ...patch })
    assert.equal(decideNoChangeDisposition(value, config).reason, 'criterion-not-met')
  })
}

test('full-gate-only plans restore; a specific derived command can close', () => {
  const value = input()
  value.acceptance.criteria = [{ ...criterion, check: 'make lint && make test' }]
  assert.equal(decideNoChangeDisposition(value, config).reason, 'only-full-gate-criteria')
  value.acceptance.criteria.push({
    ...criterion,
    check: '',
    derivedCheck: 'node --test scope.test.mjs',
  })
  assert.equal(decideNoChangeDisposition(value, config).disposition, 'close')
})

test('peer ownership and peer state take precedence over every certification', () => {
  assert.equal(
    decideNoChangeDisposition({ ...input(), claimVerdict: 'LOST' }, config).reason,
    'peer-owns',
  )
  for (const currentState of ['Delivered', 'Todo', null, '']) {
    const result = decideNoChangeDisposition({ ...input(), currentState }, config)
    assert.equal(result.disposition, 'leave')
    assert.equal(result.reason, 'state-not-ours')
  }
})

function fixture(repoConfig = config) {
  const repo = tempDir('no-change-disposition-')
  const git = (...args) => execFileSync('git', ['-C', repo, ...args], { encoding: 'utf8' }).trim()
  git('init', '-q')
  git('config', 'commit.gpgsign', 'false')
  git('config', 'user.name', 'Test')
  git('config', 'user.email', 'test@example.com')
  fs.writeFileSync(path.join(repo, '.boss-skills.json'), JSON.stringify(repoConfig))
  git('add', '.boss-skills.json')
  git('commit', '-qm', 'base')
  git('branch', 'base')
  const report = path.join(git('rev-parse', '--absolute-git-dir'), 'acceptance.json')
  fs.writeFileSync(
    report,
    JSON.stringify({
      head: git('rev-parse', 'HEAD'),
      tree: git('rev-parse', 'HEAD^{tree}'),
      criteria: [criterion],
    }),
  )
  const cli = (args = []) =>
    spawnSync(
      process.execPath,
      [
        helper,
        'decide',
        '--repo',
        repo,
        '--base',
        'base',
        '--claim-verdict',
        'WON',
        '--current-state',
        'Building',
        ...args,
      ],
      { encoding: 'utf8' },
    )
  return { repo, git, report, cli }
}

test('CLI resolves the actual git trees and configured terminal state', () => {
  const { cli, report, repo } = fixture()
  let result = cli(['--acceptance', report])
  assert.equal(result.status, 0, result.stderr)
  assert.equal(JSON.parse(result.stdout).targetState, 'Delivered')
  fs.writeFileSync(path.join(repo, 'uncommitted'), 'dirty')
  result = cli(['--acceptance', report])
  assert.equal(JSON.parse(result.stdout).disposition, 'restore')
})

test('CLI ignores daemon artifacts while preserving unrelated dirty work', () => {
  const { cli, report, repo, git } = fixture()
  const claudeDir = path.join(repo, '.claude')
  fs.mkdirSync(claudeDir)
  for (const file of ['scheduled_tasks.lock', 'settings.local.json']) {
    fs.writeFileSync(path.join(claudeDir, file), '{}')
  }
  const decide = () => {
    const result = cli(['--acceptance', report])
    assert.equal(result.status, 0, result.stderr)
    return JSON.parse(result.stdout).disposition
  }
  assert.equal(decide(), 'close')
  fs.writeFileSync(path.join(claudeDir, 'other.lock'), 'unrelated')
  assert.equal(decide(), 'restore')
  fs.unlinkSync(path.join(claudeDir, 'other.lock'))
  assert.equal(decide(), 'close')
  fs.appendFileSync(path.join(repo, '.boss-skills.json'), '\n')
  assert.equal(decide(), 'restore')
  git('add', '.boss-skills.json')
  assert.equal(decide(), 'restore')
})

test('CLI restores on missing, unreadable and malformed acceptance reports', () => {
  const { cli, report } = fixture()
  for (const args of [[], ['--acceptance', report + '.absent']]) {
    const result = cli(args)
    assert.equal(result.status, 0, result.stderr)
    assert.equal(JSON.parse(result.stdout).reason, 'no-certification')
  }
  fs.writeFileSync(report, '{')
  assert.equal(JSON.parse(cli(['--acceptance', report]).stdout).reason, 'no-certification')
})

test('CLI detects a changed base tree and unavailable git evidence', () => {
  const { cli, git, repo, report } = fixture()
  git('checkout', '-q', 'base')
  fs.writeFileSync(path.join(repo, 'base-change'), 'moved')
  git('add', 'base-change')
  git('commit', '-qm', 'moved base')
  git('checkout', '-q', '-')
  assert.equal(JSON.parse(cli(['--acceptance', report]).stdout).reason, 'base-tree-moved')
  const result = cli(['--base', 'missing', '--acceptance', report])
  assert.equal(result.status, 0)
  assert.equal(JSON.parse(result.stdout).disposition, 'restore')
})

test('CLI usage errors exit 2 and never decide', () => {
  for (const args of [
    [],
    ['other'],
    ['decide'],
    ['decide', '--bogus'],
    ['park'],
    ['park', '--bogus'],
    ['park', '--issue-id', ''],
    ['park', '--issue-id', 'uuid-1', 'extra'],
  ]) {
    const result = spawnSync(process.execPath, [helper, ...args], { encoding: 'utf8' })
    assert.equal(result.status, 2, JSON.stringify(args))
    assert.equal(result.stdout, '', JSON.stringify(args))
    assert.match(result.stderr, /usage: no-change-disposition\.mjs/)
  }
})

const unmetInput = () => {
  const value = input()
  value.acceptance.criteria = [{ ...criterion, result: 'unmet' }]
  return value
}

test('BOS-2589 regression: repeat breadcrumb or --superseded-by BOS-2404 parks with needs-human', () => {
  const comments = {
    comments: [
      {
        body: 'boss-build-no-change: restore (certification-stale)\n\nSuperseded by BOS-2404; add needs-human.',
      },
    ],
  }
  const priorBreadcrumbs = parsePriorBreadcrumbs(comments)
  assert.deepEqual(priorBreadcrumbs, [{ disposition: 'restore', reason: 'certification-stale' }])

  const repeat = decideNoChangeDisposition({ ...unmetInput(), priorBreadcrumbs }, parkConfig)
  assert.equal(repeat.disposition, 'park')
  assert.equal(repeat.reason, 'repeat-no-change')
  assert.equal(repeat.priorReason, 'certification-stale')
  assert.equal(repeat.label, 'needs-human')
  assert.equal(repeat.targetState, null)
  assert.equal(repeat.breadcrumb, 'boss-build-no-change: park (repeat-no-change)')

  const superseded = decideNoChangeDisposition(
    { ...unmetInput(), supersededBy: 'BOS-2404' },
    parkConfig,
  )
  assert.equal(superseded.disposition, 'park')
  assert.equal(superseded.reason, 'superseded')
  assert.equal(superseded.supersededBy, 'BOS-2404')
  assert.equal(superseded.label, 'needs-human')
  assert.equal(superseded.targetState, null)
  assert.equal(superseded.breadcrumb, 'boss-build-no-change: park (superseded)')

  const both = decideNoChangeDisposition(
    { ...unmetInput(), supersededBy: 'BOS-2404', priorBreadcrumbs },
    parkConfig,
  )
  assert.equal(both.disposition, 'park')
  assert.equal(both.reason, 'superseded')
  assert.equal(both.supersededBy, 'BOS-2404')
})

test('first run with no prior breadcrumb and no flag restores even when needsHuman is configured', () => {
  const result = decideNoChangeDisposition(unmetInput(), parkConfig)
  assert.equal(result.disposition, 'restore')
  assert.equal(result.reason, 'criterion-not-met')
  assert.equal(result.breadcrumb, 'boss-build-no-change: restore (criterion-not-met)')
  assert.equal(result.label, undefined)
})

test('prior park breadcrumb re-parks and latest qualifying prior reason wins', () => {
  const result = decideNoChangeDisposition(
    {
      ...unmetInput(),
      priorBreadcrumbs: [
        { disposition: 'restore', reason: 'certification-stale' },
        { disposition: 'park', reason: 'superseded' },
      ],
    },
    parkConfig,
  )
  assert.equal(result.disposition, 'park')
  assert.equal(result.reason, 'repeat-no-change')
  assert.equal(result.priorReason, 'superseded')
})

test('non-qualifying breadcrumbs, free prose, and mid-line markers stay restore', () => {
  const claimNotWon = decideNoChangeDisposition(
    { ...unmetInput(), priorBreadcrumbs: [{ disposition: 'restore', reason: 'claim-not-won' }] },
    parkConfig,
  )
  assert.equal(claimNotWon.disposition, 'restore')
  assert.equal(claimNotWon.reason, 'criterion-not-met')

  const comments = [
    { body: 'NO_CHANGE — superseded by BOS-2404, please add needs-human.' },
    { body: 'Note: boss-build-no-change: restore (certification-stale)' },
    { body: 'boss-build-no-change: restore (certification-stale) trailing text' },
    { body: 'boss-build-no-change: close (already-satisfied)' },
    { body: 'boss-build-no-change: leave (peer-owns)' },
  ]
  const parsed = parsePriorBreadcrumbs(comments)
  assert.deepEqual(parsed, [
    { disposition: 'close', reason: 'already-satisfied' },
    { disposition: 'leave', reason: 'peer-owns' },
  ])
  const result = decideNoChangeDisposition(
    { ...unmetInput(), priorBreadcrumbs: parsed },
    parkConfig,
  )
  assert.equal(result.disposition, 'restore')
  assert.equal(result.reason, 'criterion-not-met')
})

test('parsePriorBreadcrumbs unescapes markdown backslash escapes and handles envelopes/nullish', () => {
  assert.deepEqual(parsePriorBreadcrumbs(null), [])
  assert.deepEqual(parsePriorBreadcrumbs(undefined), [])
  const parsed = parsePriorBreadcrumbs({
    comments: [
      { body: 'boss-build-no-change: restore \\(certification\\-stale\\)\r\nsecond line' },
      { body: 'intro\nboss-build-no-change: restore (custom\\_reason)\noutro' },
    ],
  })
  assert.deepEqual(parsed, [
    { disposition: 'restore', reason: 'certification-stale' },
    { disposition: 'restore', reason: 'custom_reason' },
  ])
})

test('precedence: leave, close, and claim-not-won are never overridden by park inputs', () => {
  const parkInputs = {
    supersededBy: 'BOS-2404',
    priorBreadcrumbs: [{ disposition: 'restore', reason: 'certification-stale' }],
  }
  const lost = decideNoChangeDisposition(
    { ...unmetInput(), ...parkInputs, claimVerdict: 'LOST' },
    parkConfig,
  )
  assert.deepEqual([lost.disposition, lost.reason], ['leave', 'peer-owns'])

  const wrongState = decideNoChangeDisposition(
    { ...unmetInput(), ...parkInputs, currentState: 'Todo' },
    parkConfig,
  )
  assert.deepEqual([wrongState.disposition, wrongState.reason], ['leave', 'state-not-ours'])

  const certified = decideNoChangeDisposition({ ...input(), ...parkInputs }, parkConfig)
  assert.deepEqual([certified.disposition, certified.reason], ['close', 'already-satisfied'])

  for (const claimVerdict of ['NO_WINNER', 'unknown']) {
    const notWon = decideNoChangeDisposition(
      { ...unmetInput(), ...parkInputs, claimVerdict },
      parkConfig,
    )
    assert.deepEqual([notWon.disposition, notWon.reason], ['restore', 'claim-not-won'])
    assert.equal(notWon.parkSkipped, undefined)
  }
})

test('unset needsHuman label falls back to restore with parkSkipped', () => {
  const bySuperseded = decideNoChangeDisposition(
    { ...unmetInput(), supersededBy: 'BOS-2404' },
    config,
  )
  assert.equal(bySuperseded.disposition, 'restore')
  assert.equal(bySuperseded.reason, 'criterion-not-met')
  assert.equal(bySuperseded.parkSkipped, 'no-needs-human-label')
  assert.equal(bySuperseded.breadcrumb, 'boss-build-no-change: restore (criterion-not-met)')
  assert.equal(bySuperseded.label, undefined)

  const byRepeat = decideNoChangeDisposition(
    {
      ...unmetInput(),
      priorBreadcrumbs: [{ disposition: 'restore', reason: 'certification-stale' }],
    },
    config,
  )
  assert.equal(byRepeat.disposition, 'restore')
  assert.equal(byRepeat.reason, 'criterion-not-met')
  assert.equal(byRepeat.parkSkipped, 'no-needs-human-label')
})

test('breadcrumb starts with NO_CHANGE_BREADCRUMB_PREFIX and round-trips for every disposition', () => {
  const results = [
    decideNoChangeDisposition(input(), parkConfig),
    decideNoChangeDisposition(unmetInput(), parkConfig),
    decideNoChangeDisposition({ ...unmetInput(), claimVerdict: 'LOST' }, parkConfig),
    decideNoChangeDisposition({ ...unmetInput(), supersededBy: 'BOS-2404' }, parkConfig),
  ]
  for (const result of results) {
    assert.ok(result.breadcrumb.startsWith(NO_CHANGE_BREADCRUMB_PREFIX))
    assert.deepEqual(parsePriorBreadcrumbs([{ body: `${result.breadcrumb}\n\nDetails.` }]), [
      { disposition: result.disposition, reason: result.reason },
    ])
  }
})

test('CLI decide handles --comments (valid, absent, unreadable, malformed) and --superseded-by', () => {
  const { cli, repo, report } = fixture(parkConfig)
  const unmetReport = `${report}.unmet`
  fs.writeFileSync(
    unmetReport,
    JSON.stringify({
      ...JSON.parse(fs.readFileSync(report, 'utf8')),
      criteria: [{ ...criterion, result: 'unmet' }],
    }),
  )

  const commentsFile = path.join(repo, '.git', 'comments.json')
  fs.writeFileSync(
    commentsFile,
    JSON.stringify({
      comments: [{ body: 'boss-build-no-change: restore (certification-stale)\n\nDetails.' }],
    }),
  )
  const parked = cli(['--acceptance', unmetReport, '--comments', commentsFile])
  assert.equal(parked.status, 0, parked.stderr)
  const parkedOut = JSON.parse(parked.stdout)
  assert.equal(parkedOut.disposition, 'park')
  assert.equal(parkedOut.reason, 'repeat-no-change')
  assert.equal(parkedOut.priorReason, 'certification-stale')
  assert.equal(parkedOut.label, 'needs-human')

  const badJson = path.join(repo, '.git', 'bad.json')
  fs.writeFileSync(badJson, '{not-json')
  const badShape = path.join(repo, '.git', 'shape.json')
  fs.writeFileSync(badShape, JSON.stringify({ nope: true }))
  for (const args of [
    ['--acceptance', unmetReport],
    ['--acceptance', unmetReport, '--comments', `${commentsFile}.absent`],
    ['--acceptance', unmetReport, '--comments', repo],
    ['--acceptance', unmetReport, '--comments', badJson],
    ['--acceptance', unmetReport, '--comments', badShape],
  ]) {
    const res = cli(args)
    assert.equal(res.status, 0, res.stderr)
    assert.equal(JSON.parse(res.stdout).disposition, 'restore', JSON.stringify(args))
  }

  for (const ref of ['BOS-2404', 'ABC-12', '#12', 'owner/repo#12']) {
    const res = cli(['--acceptance', unmetReport, '--superseded-by', ref])
    assert.equal(res.status, 0, res.stderr)
    const out = JSON.parse(res.stdout)
    assert.equal(out.disposition, 'park')
    assert.equal(out.reason, 'superseded')
    assert.equal(out.supersededBy, ref)
  }

  for (const badRef of ['', 'has space', '-leading-dash', 'a'.repeat(65)]) {
    const res = cli(['--acceptance', unmetReport, '--superseded-by', badRef])
    assert.equal(res.status, 2, JSON.stringify(badRef))
    assert.equal(res.stdout, '', JSON.stringify(badRef))
    assert.match(res.stderr, /usage: no-change-disposition\.mjs/)
  }
})

function fakeAdapter(outcome = 'ok', extra = {}) {
  const calls = []
  return {
    calls,
    async applyIssueWrites(writes) {
      calls.push(writes)
      return { ok: outcome === 'ok', applied: [], outcome, ...extra }
    },
  }
}

test('park verb adds only the configured needsHuman label on ok', async () => {
  const adapter = fakeAdapter('ok')
  const r = await runCli(['park', '--issue-id', 'uuid-2589'], { config: parkConfig, adapter })
  assert.equal(r.code, 0)
  assert.deepEqual(r.out, { issueId: 'uuid-2589', label: 'needs-human', outcome: 'ok' })
  assert.deepEqual(adapter.calls, [{ issueId: 'uuid-2589', addLabels: ['needs-human'] }])

  const customConfig = {
    ...config,
    trackerConfig: {
      linear: { ...config.trackerConfig.linear, labels: { needsHuman: 'hold-for-human' } },
    },
  }
  const customAdapter = fakeAdapter('ok')
  const custom = await runCli(['park', '--issue-id', 'uuid-2'], {
    config: customConfig,
    adapter: customAdapter,
  })
  assert.equal(custom.code, 0)
  assert.deepEqual(customAdapter.calls, [{ issueId: 'uuid-2', addLabels: ['hold-for-human'] }])
})

test('park verb fails closed on missing capability, failed, indeterminate, or throw', async () => {
  for (const adapter of [{}, null]) {
    const r = await runCli(['park', '--issue-id', 'uuid-1'], { config: parkConfig, adapter })
    assert.equal(r.code, 69)
    assert.match(r.out.reason, /applyIssueWrites/)
  }

  for (const outcome of ['failed', 'indeterminate']) {
    const adapter = fakeAdapter(outcome, { reason: `writes ${outcome} upstream` })
    const r = await runCli(['park', '--issue-id', 'uuid-1'], { config: parkConfig, adapter })
    assert.equal(r.code, 69, outcome)
    assert.equal(r.out.outcome, outcome)
    assert.equal(r.out.reason, `writes ${outcome} upstream`)
    assert.equal(adapter.calls.length, 1, `${outcome} must never be retried`)
  }

  const throwing = {
    async applyIssueWrites() {
      throw new Error('credentials missing')
    },
  }
  const thrown = await runCli(['park', '--issue-id', 'uuid-1'], {
    config: parkConfig,
    adapter: throwing,
  })
  assert.equal(thrown.code, 69)
  assert.equal(thrown.out.reason, 'credentials missing')
})

test('park verb exits 69 without calling the adapter when needsHuman label is unset', async () => {
  const adapter = fakeAdapter('ok')
  const r = await runCli(['park', '--issue-id', 'uuid-1'], { config, adapter })
  assert.equal(r.code, 69)
  assert.match(r.out.reason, /needsHuman/)
  assert.equal(adapter.calls.length, 0)
})
