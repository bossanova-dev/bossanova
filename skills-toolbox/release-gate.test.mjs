import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { pendingReleases, nextReleases, recordRelease, main } from './release-gate.mjs'

function fixture(
  t,
  { state = 'tag', environments = 'staging, production', extension = true } = {},
) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'release-gate-'))
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }))
  const origin = path.join(tmp, 'origin.git')
  const root = path.join(tmp, 'repo')
  const gitAt = (cwd, ...args) =>
    execFileSync('/usr/bin/git', args, {
      cwd,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    }).trim()
  gitAt(tmp, 'init', '--bare', '--initial-branch=main', origin)
  gitAt(tmp, 'clone', origin, root)
  const git = (...args) => gitAt(root, ...args)
  git('config', 'commit.gpgsign', 'false')
  git('config', 'user.email', 'test@example.com')
  git('config', 'user.name', 'Test')
  let n = 0
  const commit = (subject = 'change') => {
    fs.writeFileSync(path.join(root, 'file'), String(++n))
    git('add', 'file')
    git('commit', '-m', subject)
    git('push', 'origin', 'main')
    return git('rev-parse', 'HEAD')
  }
  const base = commit('baseline')
  git('tag', 'v1')
  git('push', 'origin', 'v1')
  git('symbolic-ref', 'refs/remotes/origin/HEAD', 'refs/remotes/origin/main')
  if (extension) {
    const dir = path.join(root, '.claude/skills/boss-release-test')
    fs.mkdirSync(dir, { recursive: true })
    fs.writeFileSync(
      path.join(dir, 'SKILL.md'),
      `---\nx-boss-extension:\n  extends: boss-release\n  role: release\n  environments: ${environments}\n  state: ${state}\n---\n`,
    )
  }
  const checks = new Map(),
    prs = new Map(),
    calls = []
  let failFetch = false,
    failPush = false
  const run = (command, args, opts) => {
    calls.push([command, ...args])
    if (command === 'git') {
      if (failFetch && args[0] === 'fetch') throw new Error('fetch failed')
      if (failPush && args[0] === 'push') throw new Error('push refused')
      return gitAt(opts.cwd, ...args)
    }
    if (args[0] === 'pr') return JSON.stringify(prs.get(args[args.indexOf('--base') + 1]) ?? [])
    const endpoint = args[1]
    const sha =
      /commits\/([a-f0-9]{40})/.exec(endpoint)?.[1] ?? /head_sha=([a-f0-9]{40})/.exec(endpoint)?.[1]
    const data = checks.get(sha) ?? {}
    if (endpoint.includes('check-runs'))
      return JSON.stringify([
        { total_count: (data.runs ?? []).length, check_runs: data.runs ?? [] },
      ])
    if (endpoint.includes('/status'))
      return JSON.stringify([
        { total_count: (data.statuses ?? []).length, statuses: data.statuses ?? [] },
      ])
    return JSON.stringify([
      { total_count: (data.workflows ?? []).length, workflow_runs: data.workflows ?? [] },
    ])
  }
  const green = (sha) =>
    checks.set(sha, { runs: [{ name: 'test', status: 'completed', conclusion: 'success' }] })
  const releaseTag = (env, sha) => {
    git('tag', `boss-release/${env}/20261007T000000Z`, sha)
    git('push', 'origin', '--tags')
  }
  const branch = (env, sha) => git('push', 'origin', `${sha}:refs/heads/${env}`)
  return {
    root,
    run,
    base,
    git,
    gitAt,
    origin,
    commit,
    green,
    checks,
    prs,
    calls,
    releaseTag,
    branch,
    failFetch: () => {
      failFetch = true
    },
    failPush: () => {
      failPush = true
    },
  }
}

test('tag ranges count first-parent commits and release only the newest judged green ref', (t) => {
  const f = fixture(t)
  f.releaseTag('staging', f.base)
  const green = f.commit()
  f.green(green)
  const tip = f.commit('bookkeeping [skip ci]')
  const p = pendingReleases(f)
  assert.equal(p.environments[0].count, 2)
  assert.equal(p.environments[0].verdict, 'ready')
  assert.equal(p.environments[0].toRef, green)
  assert.deepEqual(nextReleases(f).candidates[0].commits, [green])
  assert.notEqual(p.environments[0].toRef, tip)
})

test('newest judged red or pending commit blocks older green', (t) => {
  const f = fixture(t)
  const older = f.commit()
  f.green(older)
  const newest = f.commit()
  f.checks.set(newest, { runs: [{ name: 'test', status: 'completed', conclusion: 'failure' }] })
  assert.equal(pendingReleases(f).environments[0].verdict, 'ci-red')
  f.checks.set(newest, { runs: [{ name: 'test', status: 'in_progress', conclusion: null }] })
  assert.equal(pendingReleases(f).environments[0].verdict, 'ci-pending')
})

test('commit statuses and pending workflow runs participate, boss receipts are excluded', (t) => {
  const f = fixture(t)
  const sha = f.commit()
  f.checks.set(sha, {
    statuses: [
      { context: 'external', state: 'success' },
      { context: 'boss/build', state: 'failure' },
    ],
  })
  assert.equal(pendingReleases(f).environments[0].verdict, 'ready')
  f.checks.get(sha).workflows = [
    { name: 'queued', head_sha: sha, status: 'queued', conclusion: null },
  ]
  assert.equal(pendingReleases(f).environments[0].verdict, 'ci-pending')
  f.checks.set(sha, { runs: [{ name: 'test', status: 'completed', conclusion: 'unrecognized' }] })
  assert.equal(pendingReleases(f).environments[0].verdict, 'ci-unknown')
})

test('terminal workflow failures without attached job checks block older green commits', (t) => {
  const f = fixture(t)
  const older = f.commit()
  f.green(older)
  const newest = f.commit()
  for (const conclusion of ['failure', 'cancelled', 'timed_out', 'unknown', null]) {
    f.checks.set(newest, {
      workflows: [{ name: 'CI', head_sha: newest, status: 'completed', conclusion }],
    })
    assert.equal(
      pendingReleases(f).environments[0].verdict,
      ['failure', 'cancelled', 'timed_out'].includes(conclusion) ? 'ci-red' : 'ci-unknown',
    )
  }
})

test('stale tag release records cannot regress the current released point', (t) => {
  const f = fixture(t)
  const older = f.commit()
  f.green(older)
  const newer = f.commit()
  f.green(newer)
  const record = (sha, time) =>
    recordRelease({
      ...f,
      environment: 'staging',
      toRef: sha,
      result: { action: 'released', reason: 'done', ref: sha },
      now: new Date(time),
    })
  assert.equal(record(newer, '2026-10-07T01:00:00Z').verdict, 'released')
  const before = f.git('for-each-ref', 'refs/tags', 'refs/boss-release')
  assert.equal(record(older, '2026-10-07T02:00:00Z').verdict, 'rejected')
  assert.equal(f.git('for-each-ref', 'refs/tags', 'refs/boss-release'), before)
  assert.equal(pendingReleases(f).environments[0].verdict, 'up-to-date')
})

test('branch merge-base ignores environment bookkeeping and bounds promotion', (t) => {
  const f = fixture(t, { state: 'branch' })
  f.branch('production', f.base)
  const staged = f.commit()
  f.green(staged)
  f.branch('staging', staged)
  const latest = f.commit()
  f.green(latest)
  // A separate environment commit is not itself a released mainline point.
  const envClone = path.join(path.dirname(f.root), 'env')
  f.gitAt(path.dirname(f.root), 'clone', '--branch', 'staging', f.origin, envClone)
  f.gitAt(envClone, 'config', 'commit.gpgsign', 'false')
  f.gitAt(envClone, 'config', 'user.name', 'Test')
  f.gitAt(envClone, 'config', 'user.email', 'test@example.com')
  f.gitAt(envClone, 'commit', '--allow-empty', '-m', 'environment bookkeeping')
  f.gitAt(envClone, 'push')
  const p = pendingReleases(f)
  assert.equal(p.environments[0].fromRef, staged)
  assert.equal(p.environments[0].count, 1)
  assert.equal(p.environments[1].upperBound, staged)
  assert.equal(p.environments[1].toRef, staged)
  assert.equal(p.environments[1].count, 1)
  const before = f.git('for-each-ref', 'refs/tags', 'refs/boss-release')
  assert.equal(
    recordRelease({
      ...f,
      environment: 'production',
      toRef: staged,
      result: { action: 'released', reason: 'bad promotion', ref: latest },
    }).verdict,
    'rejected',
  )
  assert.equal(f.git('for-each-ref', 'refs/tags', 'refs/boss-release'), before)
})

test('tag fallback never treats another environment release tag as this environment state', (t) => {
  const f = fixture(t)
  const staged = f.commit()
  f.green(staged)
  f.releaseTag('staging', staged)
  const production = pendingReleases(f).environments[1]
  assert.equal(production.fromRef, f.base)
  assert.equal(production.count, 1)
  assert.equal(production.verdict, 'ready')
})

test('diverged release state never fires', (t) => {
  const f = fixture(t)
  const ahead = f.commit()
  f.green(ahead)
  f.releaseTag('staging', f.base)
  f.releaseTag('production', ahead)
  const p = pendingReleases(f).environments[1]
  assert.equal(p.verdict, 'diverged')
  assert.match(p.reason, new RegExp(ahead))
})

test('branch release PRs are in-flight and skipped or human answers hold exactly one ref', (t) => {
  const f = fixture(t, { state: 'branch' })
  f.branch('staging', f.base)
  const sha = f.commit()
  f.green(sha)
  f.prs.set('staging', [{ number: 42 }])
  assert.equal(pendingReleases(f).environments[0].verdict, 'in-flight')
  f.prs.clear()
  for (const action of ['skipped', 'needs-human']) {
    assert.equal(
      recordRelease({
        ...f,
        environment: 'staging',
        toRef: sha,
        result: { action, reason: 'wait', ref: '' },
      }).verdict,
      action,
    )
    assert.equal(pendingReleases(f).environments[0].verdict, 'held')
  }
  const next = f.commit()
  f.green(next)
  assert.equal(pendingReleases(f).environments[0].verdict, 'ready')
  assert.equal(
    recordRelease({
      ...f,
      environment: 'staging',
      toRef: next,
      result: { action: 'released', reason: 'dispatched', ref: next },
    }).verdict,
    'recorded-by-branch',
  )
  assert.equal(f.git('for-each-ref', 'refs/boss-release'), '')
})

test('no extension reports only and makes no CI calls', (t) => {
  const f = fixture(t, { extension: false })
  f.commit()
  const p = nextReleases(f)
  assert.equal(p.extension, null)
  assert.equal(p.candidates.length, 0)
  assert.match(p.report, /^1 merged changes unreleased since v1; no release extension installed$/)
  assert.equal(f.calls.filter(([cmd]) => cmd === 'gh').length, 0)
})

test('dry-run record writes no tags, pushes, or holds for any action', (t) => {
  const f = fixture(t)
  const sha = f.commit()
  f.green(sha)
  const snapshot = () => [
    f.git('for-each-ref', 'refs/tags', 'refs/boss-release'),
    f.gitAt(f.origin, 'for-each-ref', 'refs/tags'),
  ]
  const before = snapshot()
  for (const action of ['released', 'skipped', 'needs-human']) {
    const result = recordRelease({
      ...f,
      environment: 'staging',
      toRef: sha,
      result: { action, reason: 'dry run', ref: action === 'released' ? sha : '' },
      dryRun: true,
    })
    assert.notEqual(result.verdict, 'rejected')
    assert.deepEqual(snapshot(), before)
  }
})

test('tag releases push an annotated tag and roll back a rejected push', (t) => {
  const f = fixture(t)
  const sha = f.commit()
  f.green(sha)
  const result = { action: 'released', reason: 'done', ref: sha }
  assert.equal(
    recordRelease({
      ...f,
      environment: 'staging',
      toRef: sha,
      result,
      now: new Date('2026-10-07T01:02:03Z'),
    }).verdict,
    'released',
  )
  assert.equal(f.git('cat-file', '-t', 'refs/tags/boss-release/staging/20261007T010203Z'), 'tag')
  assert.match(
    f.gitAt(f.origin, 'for-each-ref', 'refs/tags'),
    /boss-release\/staging\/20261007T010203Z/,
  )
  const next = f.commit()
  f.green(next)
  f.failPush()
  const before = f.git('for-each-ref', 'refs/tags')
  assert.equal(
    recordRelease({
      ...f,
      environment: 'staging',
      toRef: next,
      result: { ...result, ref: next },
      now: new Date('2026-10-07T01:02:04Z'),
    }).verdict,
    'rejected',
  )
  assert.equal(f.git('for-each-ref', 'refs/tags'), before)
})

test('read failures stay failures and CLI validates usage', (t) => {
  const f = fixture(t)
  f.failFetch()
  assert.throws(() => pendingReleases(f), /fetch failed/)
  assert.equal(
    main(['pending', '--root', f.root, '--json'], {
      run: f.run,
      stdout: () => {},
      stderr: () => {},
    }),
    2,
  )
  assert.equal(main(['record'], { stdout: () => {}, stderr: () => {} }), 64)
})
