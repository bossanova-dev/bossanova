import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { incidentalWorkSumPaths } from './check-go-work-sum-churn.mjs'

const script = fileURLToPath(new URL('./check-go-work-sum-churn.mjs', import.meta.url))

function fixture(t) {
  const cwd = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'go-work-sum-churn-')))
  t.after(() => fs.rmSync(cwd, { recursive: true, force: true, maxRetries: 3 }))
  const env = {
    ...process.env,
    GIT_CONFIG_GLOBAL: '/dev/null',
    GIT_CONFIG_NOSYSTEM: '1',
    GIT_AUTHOR_NAME: 'Fixture',
    GIT_AUTHOR_EMAIL: 'fixture@example.com',
    GIT_COMMITTER_NAME: 'Fixture',
    GIT_COMMITTER_EMAIL: 'fixture@example.com',
    GO_WORK_SUM_BASE_BRANCH: '',
    GITHUB_BASE_REF: '',
  }
  const git = (...args) => execFileSync('git', args, { cwd, env, encoding: 'utf8' }).trim()
  const write = (files) => {
    for (const [file, text] of Object.entries(files)) {
      fs.mkdirSync(path.dirname(path.join(cwd, file)), { recursive: true })
      fs.writeFileSync(path.join(cwd, file), text)
    }
  }
  const commit = (files) => {
    write(files)
    git('add', '--', ...Object.keys(files))
    git('commit', '-qm', 'fixture')
  }
  git('init', '-q', '-b', 'main')
  git('config', 'commit.gpgsign', 'false')
  commit({ 'go.work': 'go 1.25\n', 'go.work.sum': 'base\n', 'svc/go.mod': 'module fixture\n' })
  git('update-ref', 'refs/remotes/origin/main', 'HEAD')
  const run = (args = [], overrides = {}) =>
    spawnSync(process.execPath, [script, ...args], {
      cwd,
      env: { ...env, ...overrides },
      encoding: 'utf8',
    })
  return { git, write, commit, run }
}

test('go-work-sum-churn rejects a branch carrying go.work.sum beside unrelated work (the b9e1b5e31d shape)', (t) => {
  const repo = fixture(t)
  repo.commit({ 'go.work.sum': 'changed\n', 'docs/x.md': 'unrelated\n' })
  const result = repo.run()
  assert.equal(result.status, 1, result.stderr)
  assert.match(result.stderr, /go\.work\.sum/)
})

for (const companion of ['svc/go.mod', 'svc/go.sum', 'go.work']) {
  test(`go.work.sum with ${companion} passes`, (t) => {
    const repo = fixture(t)
    repo.commit({ 'go.work.sum': 'changed\n', [companion]: 'changed\n' })
    const result = repo.run()
    assert.equal(result.status, 0, result.stderr)
  })
}

test('a go.work.sum-only refresh passes', (t) => {
  const repo = fixture(t)
  repo.commit({ 'go.work.sum': 'changed\n' })
  const result = repo.run()
  assert.equal(result.status, 0, result.stderr)
})

test('unrelated work without a go.work.sum change passes', (t) => {
  const repo = fixture(t)
  repo.commit({ 'docs/x.md': 'unrelated\n' })
  const result = repo.run()
  assert.equal(result.status, 0, result.stderr)
})

test('working-tree-only go.work.sum dirt does not fail a committed branch', (t) => {
  const repo = fixture(t)
  repo.commit({ 'docs/x.md': 'unrelated\n' })
  repo.write({ 'go.work.sum': 'dirty\n' })
  const result = repo.run()
  assert.equal(result.status, 0, result.stderr)
})

for (const overrides of [
  { GITHUB_BASE_REF: 'staging' },
  { GO_WORK_SUM_BASE_BRANCH: 'production' },
  { GO_WORK_SUM_BASE_BRANCH: 'production', GITHUB_BASE_REF: 'main' },
]) {
  test(`release base skips with ${JSON.stringify(overrides)}`, (t) => {
    const repo = fixture(t)
    repo.commit({ 'go.work.sum': 'changed\n', 'docs/x.md': 'unrelated\n' })
    const result = repo.run([], overrides)
    assert.equal(result.status, 0, result.stderr)
  })
}

test('an explicit feature base overrides release environment defaults', (t) => {
  const repo = fixture(t)
  repo.commit({ 'go.work.sum': 'changed\n', 'docs/x.md': 'unrelated\n' })
  const result = repo.run(['--base', 'origin/main'], { GITHUB_BASE_REF: 'staging' })
  assert.equal(result.status, 1, result.stderr)
})

test('an unresolvable base fails closed', (t) => {
  const result = fixture(t).run(['--base', 'absent'])
  assert.equal(result.status, 2, result.stderr)
})

test('an explicit head restricts the committed range', (t) => {
  const repo = fixture(t)
  repo.commit({ 'docs/x.md': 'unrelated\n' })
  const head = repo.git('rev-parse', 'HEAD')
  repo.commit({ 'go.work.sum': 'changed\n' })
  const result = repo.run(['--head', head])
  assert.equal(result.status, 0, result.stderr)
})

test('the suggested recovery clears churn after the base advances', (t) => {
  const repo = fixture(t)
  repo.git('branch', 'feature')
  repo.commit({ 'go.work.sum': 'upstream refresh\n' })
  repo.git('update-ref', 'refs/remotes/origin/main', 'HEAD')
  repo.git('switch', '-q', 'feature')
  repo.commit({ 'go.work.sum': 'incidental\n', 'docs/x.md': 'unrelated\n' })

  const result = repo.run()
  assert.equal(result.status, 1, result.stderr)
  const command = result.stderr.match(/git [^,\n]+/)
  assert.ok(command, result.stderr)
  repo.git(...command[0].split(' ').slice(1))
  repo.git('commit', '-qam', 'apply recovery')
  const recovered = repo.run()
  assert.equal(recovered.status, 0, recovered.stderr)
})

test('the suggested recovery removes an incidental sum absent from the base', (t) => {
  const repo = fixture(t)
  repo.git('rm', 'go.work.sum')
  repo.git('commit', '-qm', 'remove workspace sum')
  repo.git('update-ref', 'refs/remotes/origin/main', 'HEAD')
  repo.commit({ 'go.work.sum': 'incidental\n', 'docs/x.md': 'unrelated\n' })

  const result = repo.run()
  assert.equal(result.status, 1, result.stderr)
  const command = result.stderr.match(/git [^,\n]+/)
  assert.ok(command, result.stderr)
  repo.git(...command[0].split(' ').slice(1))
  repo.git('commit', '-qam', 'apply recovery')
  const recovered = repo.run()
  assert.equal(recovered.status, 0, recovered.stderr)
})

test('incidentalWorkSumPaths handles nested workspace sums and module companions', () => {
  assert.deepEqual(incidentalWorkSumPaths(['x/go.work.sum', 'docs/x.md']), ['x/go.work.sum'])
  assert.deepEqual(incidentalWorkSumPaths(['x/go.work.sum']), [])
  assert.deepEqual(incidentalWorkSumPaths(['go.work.sum', 'x/go.work.sum']), [])
  assert.deepEqual(incidentalWorkSumPaths(['x/go.work.sum', 'x/go.work']), [])
  assert.deepEqual(incidentalWorkSumPaths([]), [])
})
