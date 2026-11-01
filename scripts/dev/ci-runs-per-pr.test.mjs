#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'

import {
  bucketRuns,
  classifyAll,
  classifyPush,
  fetchCapped,
  localGit,
  measure,
  parseCliArgs,
  summarize,
} from './ci-runs-per-pr.mjs'

test('parses a window and rejects unknown flags and malformed dates', () => {
  const options = parseCliArgs(['--since', '2026-10-31', '--until', '2026-11-07'])
  assert.equal(options.base, 'main')
  assert.equal(options.workflow, 'bazel.yml')
  assert.equal(options.classify, false)
  assert.throws(() =>
    parseCliArgs(['--since', '2026-10-31', '--until', '2026-11-07', '--days', '7']),
  )
  assert.throws(() => parseCliArgs(['--since', '31/10/2026', '--until', '2026-11-07']), /--since/)
  assert.throws(() => parseCliArgs(['--since', '2026-11-08', '--until', '2026-11-07']), /after/)
})

test('reads a date as the whole UTC day and a timestamp as that instant', () => {
  const days = parseCliArgs(['--since', '2026-10-31', '--until', '2026-11-07'])
  assert.deepEqual(
    [days.from, days.to, days.createdAfter],
    ['2026-10-31T00:00:00Z', '2026-11-07T23:59:59Z', null],
  )
  const exact = parseCliArgs([
    '--since',
    '2026-10-31T12:37Z',
    '--until',
    '2026-11-07T12:37:30Z',
    '--created-after',
    '2026-10-31T12:37:00Z',
  ])
  assert.deepEqual(
    [exact.from, exact.to, exact.createdAfter],
    ['2026-10-31T12:37:00Z', '2026-11-07T12:37:30Z', '2026-10-31T12:37:00Z'],
  )
  assert.throws(
    () => parseCliArgs(['--since', '2026-10-31T12:37+09:00', '--until', '2026-11-07']),
    /UTC/,
  )
  assert.throws(
    () =>
      parseCliArgs(['--since', '2026-10-31', '--until', '2026-11-07', '--created-after', 'monday']),
    /--created-after/,
  )
})

test('splits a window until every piece fits under the result cap', () => {
  // One item per second across ten seconds, with a cap of three per query.
  const items = Array.from({ length: 10 }, (_, i) => Date.parse('2026-10-01T00:00:00Z') + i * 1000)
  const within = (a, b) => items.filter((t) => t >= Date.parse(a) && t <= Date.parse(b))
  const fetched = fetchCapped({
    from: '2026-10-01T00:00:00Z',
    to: '2026-10-01T00:00:09Z',
    count: (a, b) => within(a, b).length,
    fetch: (a, b) => within(a, b).slice(0, 3),
    cap: 3,
  })
  assert.deepEqual(fetched, items)
})

test('refuses a window whose fetch returned fewer results than it counted', () => {
  assert.throws(
    () =>
      fetchCapped({
        from: '2026-10-01T00:00:00Z',
        to: '2026-10-02T00:00:00Z',
        count: () => 5,
        fetch: () => [1, 2],
      }),
    /expected 5/,
  )
})

const PR = {
  number: 7,
  branch: 'feature',
  author: 'someone',
  createdAt: '2026-10-01T10:00:00Z',
  // Ready, back to draft, ready again: only the last ready counts.
  readyAts: ['2026-10-01T11:00:00Z', '2026-10-01T12:00:00Z'],
  mergedAt: '2026-10-01T14:00:00Z',
}

function run(id, sha, createdAt, extra = {}) {
  return {
    id,
    branch: 'feature',
    sha,
    event: 'pull_request',
    createdAt,
    startedAt: createdAt,
    updatedAt: new Date(Date.parse(createdAt) + 10 * 60000).toISOString(),
    conclusion: 'success',
    ...extra,
  }
}

test('assigns runs to the phase they started in and links each post-ready run to the head it replaced', () => {
  const runs = [
    run(1, 'a', '2026-10-01T09:00:00Z'), // before the PR
    run(2, 'b', '2026-10-01T10:30:00Z'), // draft
    run(3, 'c', '2026-10-01T11:30:00Z'), // after the first ready, but the PR went back to draft
    run(4, 'd', '2026-10-01T12:30:00Z', { conclusion: 'cancelled' }), // post-ready
    run(5, 'd', '2026-10-01T13:00:00Z'), // post-ready re-run of the same SHA
    run(6, 'e', '2026-10-01T15:00:00Z'), // after merge: not this PR's cost
    { ...run(8, 'z', '2026-10-01T12:30:00Z'), branch: 'other' },
  ]
  const bucketed = bucketRuns([PR], runs)
  assert.deepEqual(
    bucketed.perPr.map(({ prePr, draft, postReady }) => ({ prePr, draft, postReady })),
    [{ prePr: 1, draft: 2, postReady: 2 }],
  )
  assert.deepEqual(
    bucketed.postReady.map(({ run, prev, firstOfSha, neverDraft }) => ({
      run,
      prev,
      firstOfSha,
      neverDraft,
    })),
    [
      { run: 4, prev: 'c', firstOfSha: true, neverDraft: false },
      { run: 5, prev: 'c', firstOfSha: false, neverDraft: false },
    ],
  )
  bucketed.postReady[0].class = 'rebase-clean'
  const summary = summarize(bucketed)
  assert.equal(summary.runsPerPr, 5)
  assert.deepEqual(summary.phasesPerPr, { prePr: 1, draft: 2, postReady: 2 })
  assert.deepEqual(summary.conclusions, { success: 4, cancelled: 1 })
  assert.deepEqual(summary.postReadyClasses, { 'rebase-clean': { runs: 1, wallMinutes: 10 } })
})

test('treats a PR with no ready event as ready from creation', () => {
  const bucketed = bucketRuns([{ ...PR, readyAts: [] }], [run(1, 'a', '2026-10-01T10:05:00Z')])
  assert.deepEqual(
    bucketed.postReady.map((r) => [r.prev, r.neverDraft]),
    [[null, true]],
  )
  assert.equal(classifyPush(bucketed.postReady[0], null, 'main'), 'first-sha')
})

function repo(t) {
  const dir = fs.mkdtempSync(join(tmpdir(), 'ci-runs-per-pr-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const git = (...args) => execFileSync('git', ['-C', dir, ...args], { encoding: 'utf8' }).trim()
  git('init', '-q', '-b', 'main')
  git('config', 'user.email', 'test@example.com')
  git('config', 'user.name', 'test')
  git('config', 'commit.gpgsign', 'false')
  const commit = (file, text, message) => {
    fs.writeFileSync(join(dir, file), text)
    git('add', file)
    git('commit', '-q', '-m', message)
    return git('rev-parse', 'HEAD')
  }
  return { dir, git, commit }
}

test('classifies each kind of push from real git history', (t) => {
  const { dir, git, commit } = repo(t)
  commit('shared.txt', 'one\ntwo\nthree\n', 'base')
  git('checkout', '-q', '-b', 'feature')
  const head = commit('feature.txt', 'feature\n', 'feat: add feature')
  const ops = localGit(dir)
  const classify = (sha, prev) => classifyPush({ sha, prev, firstOfSha: true }, ops, 'main')

  git('commit', '-q', '--amend', '-m', 'feat: [#7] add feature')
  assert.equal(classify(git('rev-parse', 'HEAD'), head), 'same-tree')

  git('reset', '-q', '--hard', head)
  assert.equal(classify(commit('feature.txt', 'feature v2\n', 'fix: tweak'), head), 'fast-forward')

  git('reset', '-q', '--hard', head)
  fs.writeFileSync(join(dir, 'feature.txt'), 'amended\n')
  git('commit', '-q', '-a', '--amend', '-m', 'feat: amended')
  assert.equal(classify(git('rev-parse', 'HEAD'), head), 'rewrite-same-base')

  // main moves on an unrelated file: replaying the same patch is a clean rebase.
  git('checkout', '-q', 'main')
  commit('other.txt', 'other\n', 'unrelated')
  git('checkout', '-q', '-b', 'clean', head)
  git('rebase', '-q', 'main')
  assert.equal(classify(git('rev-parse', 'HEAD'), head), 'rebase-clean')

  // A rebase that also changes the patch, onto a base the old head merges with cleanly.
  fs.writeFileSync(join(dir, 'feature.txt'), 'feature plus fix\n')
  git('commit', '-q', '-a', '--amend', '-m', 'feat: add feature')
  assert.equal(classify(git('rev-parse', 'HEAD'), head), 'rebase-edit-clean')

  // main edits the line the branch edits: the old head conflicts, and the resolution
  // changes the branch's patch.
  git('checkout', '-q', '-b', 'conflicting', head)
  const touch = commit('shared.txt', 'one\nbranch\nthree\n', 'feat: touch shared')
  git('checkout', '-q', 'main')
  commit('shared.txt', 'one\nmain\nthree\n', 'main edits shared')
  git('checkout', '-q', '-b', 'resolved', 'main')
  git('cherry-pick', head)
  commit('shared.txt', 'one\nmain and branch\nthree\n', 'feat: touch shared')
  assert.equal(classify(git('rev-parse', 'HEAD'), touch), 'rebase-edit-conflict')
})

test('fetches missing commits once and marks runs it still cannot read as unavailable', () => {
  const present = new Set(['a', 'b'])
  const fetched = []
  const git = {
    has: (sha) => present.has(sha),
    fetch: (shas) => {
      fetched.push(...shas)
      present.add('c')
    },
    tree: (sha) => (sha === 'c' ? 'T' : 'T'),
  }
  const runs = classifyAll(
    [
      { sha: 'c', prev: 'b', firstOfSha: true },
      { sha: 'd', prev: 'a', firstOfSha: true },
    ],
    git,
    'main',
  )
  assert.deepEqual(fetched, ['c', 'd'])
  assert.deepEqual(
    runs.map((r) => r.class),
    ['same-tree', 'unavailable'],
  )
})

function fakeGh(calls, prs) {
  return (args) => {
    calls.push(args)
    if (args[1] === 'graphql') {
      return JSON.stringify({
        data: {
          search: {
            issueCount: prs.length,
            pageInfo: { hasNextPage: false, endCursor: null },
            nodes: prs.map((pr) => ({
              number: pr.number,
              headRefName: pr.branch,
              createdAt: pr.createdAt,
              mergedAt: pr.mergedAt,
              author: { login: pr.author },
              timelineItems: { nodes: pr.readyAts.map((createdAt) => ({ createdAt })) },
            })),
          },
        },
      })
    }
    const runs = [run(1, 'a', '2026-10-01T10:30:00Z'), run(2, 'b', '2026-10-01T12:30:00Z')].map(
      (r) => ({
        id: r.id,
        head_branch: r.branch,
        head_sha: r.sha,
        event: r.event,
        created_at: r.createdAt,
        run_started_at: r.startedAt,
        updated_at: r.updatedAt,
        conclusion: r.conclusion,
      }),
    )
    return JSON.stringify({ total_count: runs.length, workflow_runs: runs })
  }
}

test('counts only PRs created at or after --created-after', () => {
  const older = { ...PR, number: 6, branch: 'older', createdAt: '2026-09-29T10:00:00Z' }
  const options = parseCliArgs([
    '--since',
    '2026-10-01',
    '--until',
    '2026-10-01',
    '--created-after',
    '2026-10-01T00:00Z',
  ])
  const { summary } = measure(
    { ...options, repo: 'o/r' },
    { gh: fakeGh([], [older, { ...PR, readyAts: ['2026-10-01T12:00:00Z'] }]), git: null },
  )
  assert.equal(summary.prs, 1)
  assert.equal(summary.runsPerPr, 2)
})

test('measures runs per merged PR from the GitHub responses', () => {
  const calls = []
  const gh = fakeGh(calls, [{ ...PR, readyAts: ['2026-10-01T12:00:00Z'] }])
  const { summary } = measure(
    { ...parseCliArgs(['--since', '2026-10-01', '--until', '2026-10-01']), repo: 'o/r' },
    { gh, git: null },
  )
  assert.equal(summary.prs, 1)
  assert.equal(summary.runsPerPr, 2)
  assert.deepEqual(summary.phasesPerPr, { prePr: 0, draft: 1, postReady: 1 })
  const runQuery = calls.find((args) => args[1].includes('/actions/workflows/bazel.yml/runs'))
  // Runs are read from a day before the earliest merged PR was created.
  assert.match(runQuery[1], /created=2026-09-30T10:00:00Z\.\.2026-10-01T23:59:59Z/)
})
