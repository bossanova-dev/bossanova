import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { decideNoChangeDisposition } from './no-change-disposition.mjs'

const helper = fileURLToPath(new URL('./no-change-disposition.mjs', import.meta.url))
const config = {
  adapters: { tracker: 'linear' },
  trackerConfig: { linear: { states: { inProgress: 'Building', done: 'Delivered' } } },
  acceptance: { fullGateCommands: ['make lint', 'make test'] },
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

function fixture() {
  const repo = tempDir('no-change-disposition-')
  const git = (...args) => execFileSync('git', ['-C', repo, ...args], { encoding: 'utf8' }).trim()
  git('init', '-q')
  git('config', 'commit.gpgsign', 'false')
  git('config', 'user.name', 'Test')
  git('config', 'user.email', 'test@example.com')
  fs.writeFileSync(path.join(repo, '.boss-skills.json'), JSON.stringify(config))
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
  for (const args of [[], ['other'], ['decide'], ['decide', '--bogus']]) {
    const result = spawnSync(process.execPath, [helper, ...args], { encoding: 'utf8' })
    assert.equal(result.status, 2)
    assert.equal(result.stdout, '')
  }
})
