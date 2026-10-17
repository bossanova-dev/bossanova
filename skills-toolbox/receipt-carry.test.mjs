import test from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import {
  DECISIONS,
  UsageError,
  carryReport,
  decideCarry,
  exitCodeFor,
  parseCli,
} from './receipt-carry.mjs'

const RANGE = { base: 'a'.repeat(40), head: 'b'.repeat(40) }

// ---------------------------------------------------------------------------
// decideCarry: the decision table

for (const [label, facts, decision, reasons] of [
  [
    'start head with no receipt, even on an unchanged tree',
    { startReceipt: 'absent', gateReady: true, drift: 'none' },
    DECISIONS.REFUSED,
    ['start-head-not-receipted'],
  ],
  [
    'start receipt unreadable',
    { startReceipt: 'unknown', drift: 'none' },
    DECISIONS.REFUSED,
    ['start-receipt-unknown'],
  ],
  [
    'review state calls the head ready',
    { startReceipt: 'success', gateReady: true },
    DECISIONS.CARRY,
    [],
  ],
  [
    'tree unchanged from the receipted head',
    { startReceipt: 'success', gateReady: false, drift: 'none' },
    DECISIONS.CARRY,
    [],
  ],
  [
    'drift unreadable',
    { startReceipt: 'success', gateReady: false, drift: 'unknown' },
    DECISIONS.REFUSED,
    ['tree-drift-unknown'],
  ],
  [
    'tree changed, delta not computable',
    { startReceipt: 'success', drift: 'changed', range: null },
    DECISIONS.REFUSED,
    ['review-range-unknown'],
  ],
  [
    'tree changed, no review yet',
    { startReceipt: 'success', drift: 'changed', range: RANGE },
    DECISIONS.RE_REVIEW,
    ['tree-changed-since-review'],
  ],
  [
    'clean review of the delta',
    {
      startReceipt: 'success',
      drift: 'changed',
      range: RANGE,
      review: { status: 'pass', rangeMatches: true },
    },
    DECISIONS.POST,
    [],
  ],
  [
    'blocking review of the delta',
    {
      startReceipt: 'success',
      drift: 'changed',
      range: RANGE,
      review: { status: 'blocked', rangeMatches: true },
    },
    DECISIONS.BLOCKED,
    ['review-blocked'],
  ],
  [
    'clean review of another range',
    {
      startReceipt: 'success',
      drift: 'changed',
      range: RANGE,
      review: { status: 'pass', rangeMatches: false },
    },
    DECISIONS.REFUSED,
    ['review-range-mismatch'],
  ],
  [
    'review that produced no usable verdict',
    {
      startReceipt: 'success',
      drift: 'changed',
      range: RANGE,
      review: { status: 'error', rangeMatches: false },
    },
    DECISIONS.REFUSED,
    ['review-unusable'],
  ],
])
  test(`decideCarry: ${label}`, () => {
    const got = decideCarry(facts)
    assert.equal(got.decision, decision)
    assert.deepEqual(got.reasons, reasons)
  })

// ---------------------------------------------------------------------------
// CLI surface

test('parseCli requires both SHAs and collects every verdict file', () => {
  assert.throws(() => parseCli(['--head', 'a'.repeat(40)]), UsageError)
  assert.throws(() => parseCli(['--start-sha', 'abc', '--head', 'a'.repeat(40)]), UsageError)
  assert.throws(() => parseCli(['--start-sha', 'a'.repeat(40), '--bogus', 'x']), UsageError)
  const parsed = parseCli([
    '--start-sha',
    'a'.repeat(40),
    '--head',
    'b'.repeat(40),
    '--review-verdict',
    '/1.json',
    '--review-verdict',
    '/2.json',
    '--target-url',
    '',
  ])
  assert.deepEqual(parsed.reviewVerdicts, ['/1.json', '/2.json'])
  assert.equal(parsed.targetUrl, undefined)
})

for (const [report, code] of [
  [{ decision: 're-review' }, 4],
  [{ decision: 'carry', posted: { verdict: 'posted' } }, 0],
  [{ decision: 'post', posted: { verdict: 'posted' } }, 0],
  [{ decision: 'post', posted: { verdict: 'refused' } }, 3],
  [{ decision: 'post', posted: { verdict: 'failed' } }, 1],
  [{ decision: 'blocked' }, 3],
  [{ decision: 'refused' }, 3],
])
  test(`exitCodeFor(${JSON.stringify(report)}) is ${code}`, () =>
    assert.equal(exitCodeFor(report), code))

// ---------------------------------------------------------------------------
// carryReport against a real repository and a fake gh

const LINES = Array.from({ length: 12 }, (_, i) => `l${i + 1}`).join('\n') + '\n'

const IDENT = [
  '-c',
  'user.name=T',
  '-c',
  'user.email=t@example.invalid',
  '-c',
  'commit.gpgsign=false',
]

function gitIn(dir) {
  const git = (...args) => {
    const result = spawnSync('git', [...IDENT, ...args], { cwd: dir, encoding: 'utf8' })
    assert.equal(result.status, 0, result.stderr)
    return result.stdout.trim()
  }
  const commit = (file, content, message) => {
    writeFileSync(join(dir, file), content)
    git('add', file)
    git('commit', '-m', message)
    return git('rev-parse', 'HEAD')
  }
  return { git, commit }
}

// git runs for real; gh answers `boss/build` reads from `receipts` and records each post.
function fakeSpawn(receipts, posts) {
  const gh = (args) => {
    if (args[0] === 'pr') return { status: 0, stdout: '{"baseRefName":"main"}' }
    if (args.includes('POST')) {
      posts.push(args)
      return { status: 0, stdout: '{}' }
    }
    const shaArg = args[1].match(/commits\/([0-9a-f]{40})\/statuses/)?.[1]
    const statuses = receipts.has(shaArg)
      ? [{ context: 'boss/build', state: 'success', updated_at: '2026-10-15T00:00:00Z' }]
      : []
    return { status: 0, stdout: JSON.stringify([statuses]) }
  }
  return (command, args, options) =>
    command === 'gh' ? gh(args) : spawnSync(command, args, options)
}

function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'receipt-carry-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const { git, commit } = gitIn(dir)
  git('init', '--initial-branch=main')
  git('config', 'gc.auto', '0')
  commit('a.txt', LINES, 'base')
  git('update-ref', 'refs/remotes/origin/main', 'HEAD')
  git('checkout', '-b', 'feature')
  const start = commit('a.txt', LINES.replace('l5', 'feature five'), 'feature')
  const onMain = (file, content, message) => {
    git('checkout', 'main')
    commit(file, content, message)
    git('update-ref', 'refs/remotes/origin/main', 'HEAD')
    git('checkout', 'feature')
  }

  const receipts = new Set([start])
  const posts = []
  const spawn = fakeSpawn(receipts, posts)
  const run = (head, reviewVerdicts = [], config = {}, startSha = start) =>
    carryReport({ startSha, head, reviewVerdicts }, { spawn, cwd: dir, loadConfig: () => config })
  // A push from this repository moves the remote-tracking ref and logs it.
  const pushed = (sha) =>
    git('update-ref', '--create-reflog', 'refs/remotes/origin/feature', sha, '-m', 'update by push')
  const verdict = (file, { base, head, findings = [] }) => {
    writeFileSync(
      file,
      JSON.stringify({
        version: 1,
        status: findings.length ? 'blocked' : 'pass',
        base,
        head,
        findings,
        fixPass: { ran: false },
        secondModel: { ran: true, agent: 'codex' },
      }),
    )
    return file
  }
  return { dir, git, commit, onMain, start, receipts, posts, run, pushed, verdict }
}

const BLOCKER = {
  id: 'f1',
  severity: 'P1',
  introduced: true,
  file: 'a.txt',
  line: 5,
  title: 'resolution dropped the feature line',
  evidence: 'a.txt:5 lost the change',
  source: 'correctness',
  verified: true,
}

const postedOn = (posts, sha) =>
  posts.filter((args) => args.some((a) => a.endsWith(`/statuses/${sha}`)))

test('a pure rebase keeps the reviewed tree and carries the receipt', (t) => {
  const f = fixture(t)
  f.onMain('b.txt', 'unrelated\n', 'unrelated main change')
  f.git('rebase', 'main')
  const head = f.git('rev-parse', 'HEAD')
  const report = f.run(head)
  assert.equal(report.decision, DECISIONS.CARRY)
  assert.equal(report.posted.verdict, 'posted')
  assert.equal(postedOn(f.posts, head).length, 1)
})

test('a repair commit on a receipted head needs a review of exactly that commit', (t) => {
  const f = fixture(t)
  const head = f.commit('c.txt', 'fix\n', 'repair fix')
  const report = f.run(head)
  assert.equal(report.decision, DECISIONS.RE_REVIEW)
  assert.equal(exitCodeFor(report), 4)
  assert.equal(report.review.base, f.start)
  assert.equal(report.review.head, head)
  assert.deepEqual(report.review.skills, ['boss-review'])
  assert.equal(report.review.verdictFiles.length, 1)
  assert.equal(f.posts.length, 0)
})

test('a clean review of the repair delta posts the receipt on the new head', (t) => {
  const f = fixture(t)
  const head = f.commit('c.txt', 'fix\n', 'repair fix')
  const [file] = f.run(head).review.verdictFiles
  f.verdict(file, { base: f.start, head })
  const report = f.run(head, [file])
  assert.equal(report.decision, DECISIONS.POST)
  assert.equal(report.posted.verdict, 'posted')
  assert.equal(postedOn(f.posts, head).length, 1)
})

test('a blocking review of the repair delta posts nothing and writes the report', (t) => {
  const f = fixture(t)
  const head = f.commit('c.txt', 'fix\n', 'repair fix')
  const [file] = f.run(head).review.verdictFiles
  f.verdict(file, { base: f.start, head, findings: [BLOCKER] })
  const report = f.run(head, [file])
  assert.equal(report.decision, DECISIONS.BLOCKED)
  assert.equal(exitCodeFor(report), 3)
  assert.equal(report.findings.length, 1)
  assert.match(readFileSync(report.reportFile, 'utf8'), /^<!-- bs-review -->/)
  assert.equal(f.posts.length, 0)
})

for (const [label, range] of [
  ['the start head (the wrong head)', (f) => ({ base: f.start, head: f.start })],
  ['a narrower range (the wrong base)', (f, head) => ({ base: head, head })],
])
  test(`a clean review of ${label} posts nothing`, (t) => {
    const f = fixture(t)
    f.commit('c.txt', 'fix\n', 'repair fix')
    const head = f.commit('d.txt', 'fix two\n', 'second repair fix')
    const [file] = f.run(head).review.verdictFiles
    f.verdict(file, range(f, head))
    const report = f.run(head, [file])
    assert.equal(report.decision, DECISIONS.REFUSED)
    assert.deepEqual(report.reasons, ['review-range-mismatch'])
    assert.equal(f.posts.length, 0)
  })

const CHAIN = { review: { skill: ['first-review', 'second-review'] } }

for (const [label, ranges, decision] of [
  [
    'two reviews that each covered half the delta',
    (f, mid, head) => [
      { base: f.start, head: mid },
      { base: mid, head },
    ],
    DECISIONS.REFUSED,
  ],
  [
    'one full-range review and one of half the delta',
    (f, mid, head) => [
      { base: f.start, head },
      { base: mid, head },
    ],
    DECISIONS.REFUSED,
  ],
  [
    'two reviews that each covered the whole delta',
    (f, mid, head) => [
      { base: f.start, head },
      { base: f.start, head },
    ],
    DECISIONS.POST,
  ],
])
  test(`a review chain of ${label}: ${decision}`, (t) => {
    const f = fixture(t)
    const mid = f.commit('c.txt', 'fix\n', 'repair fix')
    const head = f.commit('d.txt', 'fix two\n', 'second repair fix')
    const first = f.run(head, [], CHAIN)
    assert.deepEqual(first.review.skills, CHAIN.review.skill)
    const files = first.review.verdictFiles
    assert.equal(files.length, 2)
    ranges(f, mid, head).forEach((range, i) => f.verdict(files[i], range))
    const report = f.run(head, files, CHAIN)
    assert.equal(report.decision, decision, JSON.stringify(report))
    if (decision === DECISIONS.POST) {
      assert.equal(report.posted.verdict, 'posted')
      assert.equal(f.posts.length, 1)
      assert.equal(postedOn(f.posts, head).length, 1)
    } else {
      assert.deepEqual(report.reasons, ['review-range-mismatch'])
      assert.equal(f.posts.length, 0)
    }
  })

test('a review that never wrote its verdict posts nothing', (t) => {
  const f = fixture(t)
  const head = f.commit('c.txt', 'fix\n', 'repair fix')
  const [file] = f.run(head).review.verdictFiles
  const report = f.run(head, [file])
  assert.equal(report.decision, DECISIONS.REFUSED)
  assert.deepEqual(report.reasons, ['review-unusable'])
  assert.equal(f.posts.length, 0)
})

test('a start head with no receipt is refused without a review', (t) => {
  const f = fixture(t)
  f.receipts.clear()
  const head = f.commit('c.txt', 'fix\n', 'repair fix')
  const report = f.run(head)
  assert.equal(report.decision, DECISIONS.REFUSED)
  assert.deepEqual(report.reasons, ['start-head-not-receipted'])
  assert.equal(report.review, undefined)
  assert.equal(f.posts.length, 0)
})

test('a conflict resolution is reviewed as the delta from the replayed conflict', (t) => {
  const f = fixture(t)
  f.onMain('a.txt', LINES.replace('l5', 'main five'), 'conflicting main change')
  const ident = [
    '-c',
    'user.name=T',
    '-c',
    'user.email=t@example.invalid',
    '-c',
    'commit.gpgsign=false',
  ]
  const rebase = spawnSync('git', [...ident, 'rebase', 'main'], { cwd: f.dir, encoding: 'utf8' })
  assert.notEqual(rebase.status, 0, 'the rebase conflicts')
  assert.match(`${rebase.stdout}${rebase.stderr}`, /CONFLICT/, 'the rebase stops on the conflict')
  writeFileSync(join(f.dir, 'a.txt'), LINES.replace('l5', 'main five and feature five'))
  f.git('add', 'a.txt')
  const resumed = spawnSync('git', [...ident, '-c', 'core.editor=true', 'rebase', '--continue'], {
    cwd: f.dir,
    encoding: 'utf8',
    env: { ...process.env, GIT_EDITOR: 'true' },
  })
  assert.equal(resumed.status, 0, `the rebase resumes: ${resumed.stderr}`)
  const head = f.git('rev-parse', 'HEAD')
  assert.notEqual(head, f.start)

  const first = f.run(head)
  assert.equal(first.decision, DECISIONS.RE_REVIEW, JSON.stringify(first))
  assert.equal(first.review.replayed, true)
  assert.equal(first.review.conflicted, true)
  assert.equal(
    f.git('rev-parse', `${first.review.head}^{tree}`),
    f.git('rev-parse', `${head}^{tree}`),
  )
  const delta = f.git('diff', `${first.review.base}...${first.review.head}`)
  assert.match(delta, /^-<<<<<<< /m, 'the delta removes the conflict markers')
  assert.match(delta, /^\+main five and feature five$/m, 'the delta adds the resolution')
  assert.equal(f.run(head).review.base, first.review.base, 'the delta commits are deterministic')

  const [file] = first.review.verdictFiles
  f.verdict(file, { base: first.review.base, head: first.review.head })
  const report = f.run(head, [file])
  assert.equal(report.decision, DECISIONS.POST)
  assert.equal(report.posted.verdict, 'posted', JSON.stringify(report.posted))
  assert.equal(postedOn(f.posts, head).length, 1)
  assert.equal(existsSync(join(f.dir, '.git', 'boss-repair', 'receipt')), true)
})

// PR 3162: the receipted head was followed by an operator-requested change pushed with no receipt,
// then by a repair pass's conflict-resolution rebase. That pass starts on the unreceipted head, so
// the receipt is re-earned from the last receipted head this branch pushed, over a delta that
// includes the unreviewed change.
test('a pass starting on a head that lost the receipt re-earns it from the last receipted push', (t) => {
  const f = fixture(t)
  f.pushed(f.start)
  const followUp = f.commit('c.txt', 'layout tweak\n', 'operator follow-up')
  f.pushed(followUp)
  f.onMain('a.txt', LINES.replace('l5', 'main five'), 'conflicting main change')
  const ident = [
    '-c',
    'user.name=T',
    '-c',
    'user.email=t@example.invalid',
    '-c',
    'commit.gpgsign=false',
  ]
  const rebase = spawnSync('git', [...ident, 'rebase', 'main'], { cwd: f.dir, encoding: 'utf8' })
  assert.notEqual(rebase.status, 0, 'the rebase conflicts')
  writeFileSync(join(f.dir, 'a.txt'), LINES.replace('l5', 'main five and feature five'))
  f.git('add', 'a.txt')
  const resumed = spawnSync('git', [...ident, 'rebase', '--continue'], {
    cwd: f.dir,
    encoding: 'utf8',
    env: { ...process.env, GIT_EDITOR: 'true' },
  })
  assert.equal(resumed.status, 0, `the rebase resumes: ${resumed.stderr}`)
  const head = f.git('rev-parse', 'HEAD')
  f.pushed(head)

  const first = f.run(head, [], {}, followUp)
  assert.equal(first.decision, DECISIONS.RE_REVIEW, JSON.stringify(first))
  assert.equal(first.baselineSha, f.start)
  assert.equal(first.review.replayed, true)
  const delta = f.git('diff', `${first.review.base}...${first.review.head}`)
  assert.match(delta, /^\+layout tweak$/m, 'the delta includes the unreceipted change')
  assert.match(delta, /^\+main five and feature five$/m, 'the delta includes the resolution')

  const [file] = first.review.verdictFiles
  f.verdict(file, { base: first.review.base, head: first.review.head })
  const report = f.run(head, [file], {}, followUp)
  assert.equal(report.decision, DECISIONS.POST, JSON.stringify(report))
  assert.equal(report.posted.verdict, 'posted', JSON.stringify(report.posted))
  const [post] = postedOn(f.posts, head)
  assert.ok(post, 'the receipt is posted on the pushed head')
  assert.ok(
    post.some((arg) => arg.includes(`re-reviewed from ${f.start.slice(0, 12)}`)),
    'the receipt names the baseline it was re-earned from',
  )
})

test('a branch that never pushed a receipted head is refused', (t) => {
  const f = fixture(t)
  f.receipts.clear()
  f.pushed(f.start)
  const followUp = f.commit('c.txt', 'layout tweak\n', 'operator follow-up')
  f.pushed(followUp)
  const head = f.commit('d.txt', 'fix\n', 'repair fix')
  f.pushed(head)
  const report = f.run(head, [], {}, followUp)
  assert.equal(report.decision, DECISIONS.REFUSED)
  assert.deepEqual(report.reasons, ['start-head-not-receipted'])
  assert.equal(report.baselineSha, undefined)
  assert.equal(f.posts.length, 0)
})

test('a head fetched from someone else ends the lookback before the receipted push', (t) => {
  const f = fixture(t)
  f.pushed(f.start)
  const humanPush = f.commit('c.txt', 'hand edit\n', 'pushed from another machine')
  f.git('update-ref', 'refs/remotes/origin/feature', humanPush, '-m', 'fetch: fast-forward')
  const head = f.commit('d.txt', 'fix\n', 'repair fix')
  f.pushed(head)
  const report = f.run(head, [], {}, humanPush)
  assert.equal(report.decision, DECISIONS.REFUSED)
  assert.deepEqual(report.reasons, ['start-head-not-receipted'])
  assert.equal(report.baselineSha, undefined)
  assert.equal(f.posts.length, 0)
})

// ---------------------------------------------------------------------------
// The baseline lookup against real pushes: a bare remote, the session's clone, and the other
// writers a branch has (a peer worktree of that clone, another clone).

function remoteFixture(t) {
  const root = mkdtempSync(join(tmpdir(), 'receipt-carry-remote-'))
  t.after(() => rmSync(root, { recursive: true, force: true }))
  const remote = join(root, 'remote.git')
  const session = join(root, 'session')
  gitIn(root).git('init', '--bare', '--initial-branch=main', remote)
  gitIn(root).git('init', '--initial-branch=main', session)
  const { git, commit } = gitIn(session)
  git('config', 'gc.auto', '0')
  git('remote', 'add', 'origin', remote)
  commit('a.txt', LINES, 'base')
  git('push', '-q', 'origin', 'main')
  git('checkout', '-q', '-b', 'feature')
  const receipted = commit('a.txt', LINES.replace('l5', 'feature five'), 'feature')
  git('push', '-q', '-u', 'origin', 'feature')
  const receipts = new Set([receipted])
  const posts = []
  const spawn = fakeSpawn(receipts, posts)
  const clone = (name) => {
    const dir = join(root, name)
    gitIn(root).git('clone', '-q', '--branch', 'feature', remote, dir)
    return { dir, ...gitIn(dir) }
  }
  const worktree = (name) => {
    const dir = join(root, name)
    git('worktree', 'add', '-q', '-b', name, dir, 'origin/feature')
    return { dir, ...gitIn(dir) }
  }
  const run = (cwd, startSha, head, reviewVerdicts = []) =>
    carryReport({ startSha, head, reviewVerdicts }, { spawn, cwd, loadConfig: () => ({}) })
  const trackingLog = (dir) =>
    gitIn(dir).git('reflog', 'show', '--format=%gs', 'refs/remotes/origin/feature').split('\n')
  return {
    remote,
    session,
    git,
    commit,
    receipted,
    receipts,
    posts,
    clone,
    worktree,
    run,
    trackingLog,
  }
}

test('pushes this worktree made chain back to the receipted push', (t) => {
  const f = remoteFixture(t)
  const followUp = f.commit('c.txt', 'layout tweak\n', 'follow-up')
  f.git('push', '-q')
  const head = f.commit('d.txt', 'fix\n', 'repair fix')
  f.git('push', '-q')
  const report = f.run(f.session, followUp, head)
  assert.equal(report.decision, DECISIONS.RE_REVIEW, JSON.stringify(report))
  assert.equal(report.baselineSha, f.receipted)
  assert.equal(report.review.base, f.receipted)
})

test('a push from a peer worktree of the same clone ends the lookback', (t) => {
  const f = remoteFixture(t)
  const peer = f.worktree('peer')
  const peerPush = peer.commit('c.txt', 'hand edit\n', 'pushed from a peer worktree')
  peer.git('push', '-q', 'origin', 'HEAD:feature')
  f.git('merge', '-q', '--ff-only', 'origin/feature')
  const head = f.commit('d.txt', 'fix\n', 'repair fix')
  f.git('push', '-q')
  assert.deepEqual(
    f.trackingLog(f.session).slice(0, 3),
    ['update by push', 'update by push', 'update by push'],
    'the shared remote-tracking reflog records the peer push as a push',
  )
  const report = f.run(f.session, peerPush, head)
  assert.equal(report.decision, DECISIONS.REFUSED, JSON.stringify(report))
  assert.deepEqual(report.reasons, ['start-head-not-receipted'])
  assert.equal(report.baselineSha, undefined)
  assert.equal(f.posts.length, 0)
})

test('a receipted push from a peer worktree is the oldest baseline the lookback reaches', (t) => {
  const f = remoteFixture(t)
  const peer = f.worktree('peer')
  const peerPush = peer.commit('c.txt', 'peer change\n', 'receipted peer push')
  peer.git('push', '-q', 'origin', 'HEAD:feature')
  f.receipts.add(peerPush)
  f.git('merge', '-q', '--ff-only', 'origin/feature')
  const followUp = f.commit('d.txt', 'layout tweak\n', 'follow-up')
  f.git('push', '-q')
  const head = f.commit('e.txt', 'fix\n', 'repair fix')
  f.git('push', '-q')
  const report = f.run(f.session, followUp, head)
  assert.equal(report.decision, DECISIONS.RE_REVIEW, JSON.stringify(report))
  assert.equal(report.baselineSha, peerPush)
})

test('a commit merged in from FETCH_HEAD stays inside the reviewed delta', (t) => {
  const f = remoteFixture(t)
  const other = f.clone('other-machine')
  const foreign = other.commit('c.txt', 'foreign change\n', 'pushed from another machine')
  other.git('push', '-q')
  f.git('fetch', '-q', f.remote, 'feature')
  f.git('merge', '-q', '--ff-only', 'FETCH_HEAD')
  const head = f.commit('d.txt', 'fix\n', 'repair fix')
  f.git('push', '-q')
  assert.deepEqual(f.trackingLog(f.session).slice(0, 2), ['update by push', 'update by push'])
  const report = f.run(f.session, foreign, head)
  assert.equal(report.decision, DECISIONS.RE_REVIEW, JSON.stringify(report))
  assert.equal(report.baselineSha, f.receipted)
  const delta = f.git('diff', `${report.review.base}...${report.review.head}`)
  assert.match(delta, /^\+foreign change$/m, 'the foreign commit is reviewed before any post')
  assert.equal(f.posts.length, 0)
})

test('a cherry-pick that fast-forwards onto a peer push ends the lookback at that push', (t) => {
  const f = remoteFixture(t)
  const peer = f.worktree('peer')
  const peerPush = peer.commit('c.txt', 'hand edit\n', 'pushed from a peer worktree')
  peer.git('push', '-q', 'origin', 'HEAD:feature')
  f.git('cherry-pick', '--ff', peerPush)
  assert.equal(
    f.git('rev-parse', 'HEAD'),
    peerPush,
    'the cherry-pick moved HEAD onto the peer commit',
  )
  assert.match(f.git('reflog', 'show', '--format=%gs', '-1', 'HEAD'), /^cherry-pick: fast-forward/)
  const followUp = f.commit('f.txt', 'layout tweak\n', 'follow-up')
  f.git('push', '-q')
  const head = f.commit('d.txt', 'fix\n', 'repair fix')
  f.git('push', '-q')
  const refused = f.run(f.session, followUp, head)
  assert.equal(refused.decision, DECISIONS.REFUSED, JSON.stringify(refused))
  assert.deepEqual(refused.reasons, ['start-head-not-receipted'])
  assert.equal(refused.baselineSha, undefined)
  f.receipts.add(peerPush)
  const report = f.run(f.session, followUp, head)
  assert.equal(report.baselineSha, peerPush, 'the peer push is the oldest baseline reached')
})

test('rebasing local work onto a peer push ends the lookback at that push', (t) => {
  const f = remoteFixture(t)
  const peer = f.worktree('peer')
  const peerPush = peer.commit('c.txt', 'hand edit\n', 'pushed from a peer worktree')
  peer.git('push', '-q', 'origin', 'HEAD:feature')
  f.commit('e.txt', 'local work\n', 'local work')
  f.git('rebase', peerPush)
  assert.ok(
    f
      .git('reflog', 'show', '--format=%H %gs', 'HEAD')
      .split('\n')
      .some((line) => line.startsWith(`${peerPush} rebase (start)`)),
    'HEAD reached the peer commit through the start of the rebase',
  )
  const followUp = f.git('rev-parse', 'HEAD')
  f.git('push', '-q')
  const head = f.commit('d.txt', 'fix\n', 'repair fix')
  f.git('push', '-q')
  const refused = f.run(f.session, followUp, head)
  assert.equal(refused.decision, DECISIONS.REFUSED, JSON.stringify(refused))
  assert.deepEqual(refused.reasons, ['start-head-not-receipted'])
  assert.equal(refused.baselineSha, undefined)
  f.receipts.add(peerPush)
  const report = f.run(f.session, followUp, head)
  assert.equal(report.baselineSha, peerPush, 'the peer push is the oldest baseline reached')
})

test('a follow-up commit whose message mentions fast-forward still re-earns the receipt', (t) => {
  for (const message of ['fix: handle fast-forward pushes', 'fast-forward']) {
    const f = remoteFixture(t)
    const followUp = f.commit('c.txt', 'layout tweak\n', message)
    f.git('push', '-q')
    const head = f.commit('d.txt', 'fix\n', 'repair fix')
    f.git('push', '-q')
    assert.match(
      f.git('reflog', 'show', '--format=%H %gs', 'HEAD'),
      new RegExp(`${followUp} commit: .*fast-forward`),
    )
    const report = f.run(f.session, followUp, head)
    assert.equal(report.decision, DECISIONS.RE_REVIEW, `${message}: ${JSON.stringify(report)}`)
    assert.equal(report.baselineSha, f.receipted, message)
  }
})

test('an arrival logged as a rebase fast-forward, as older git writes it, ends the lookback', (t) => {
  const f = remoteFixture(t)
  const peer = f.worktree('peer')
  const peerPush = peer.commit('c.txt', 'hand edit\n', 'pushed from a peer worktree')
  peer.git('push', '-q', 'origin', 'HEAD:feature')
  f.git('update-ref', '-m', 'rebase: fast-forward', 'HEAD', peerPush)
  f.git('reset', '-q', '--hard')
  const followUp = f.commit('f.txt', 'layout tweak\n', 'follow-up')
  f.git('push', '-q')
  const head = f.commit('d.txt', 'fix\n', 'repair fix')
  f.git('push', '-q')
  const refused = f.run(f.session, followUp, head)
  assert.equal(refused.decision, DECISIONS.REFUSED, JSON.stringify(refused))
  assert.deepEqual(refused.reasons, ['start-head-not-receipted'])
  f.receipts.add(peerPush)
  assert.equal(f.run(f.session, followUp, head).baselineSha, peerPush)
})

test('a revert commit made here counts as made here', (t) => {
  const f = remoteFixture(t)
  const mistake = f.commit('c.txt', 'mistake\n', 'add c')
  f.git('revert', '--no-edit', mistake)
  const followUp = f.git('rev-parse', 'HEAD')
  assert.match(f.git('reflog', 'show', '--format=%gs', '-1', 'HEAD'), /^revert:/)
  f.git('push', '-q')
  const head = f.commit('d.txt', 'fix\n', 'repair fix')
  f.git('push', '-q')
  const report = f.run(f.session, followUp, head)
  assert.equal(report.decision, DECISIONS.RE_REVIEW, JSON.stringify(report))
  assert.equal(report.baselineSha, f.receipted)
})

// Each sync moves origin/feature onto a receipted head someone else pushed, under a reflog subject
// other than `update by push`; the repair pushes on top must not reach past it.
for (const [label, sync, subject] of [
  ['git pull --ff-only', (f) => f.git('pull', '-q', '--ff-only'), /^pull .*--ff-only/],
  ['git pull --rebase', (f) => f.git('pull', '-q', '--rebase'), /^pull .*--rebase/],
  [
    'git remote update',
    (f) => {
      f.git('remote', 'update')
      f.git('merge', '-q', '--ff-only', 'origin/feature')
    },
    /^fetch .*--all/,
  ],
  [
    'git fetch',
    (f) => {
      f.git('fetch', '-q')
      f.git('merge', '-q', '--ff-only', 'origin/feature')
    },
    /^fetch/,
  ],
])
  test(`a receipted head brought in by ${label} ends the lookback`, (t) => {
    const f = remoteFixture(t)
    const other = f.clone('other-machine')
    const foreign = other.commit('c.txt', 'foreign change\n', 'pushed from another machine')
    other.git('push', '-q')
    f.receipts.add(foreign)
    sync(f)
    const followUp = f.commit('d.txt', 'layout tweak\n', 'follow-up')
    f.git('push', '-q')
    const head = f.commit('e.txt', 'fix\n', 'repair fix')
    f.git('push', '-q')
    assert.match(f.trackingLog(f.session)[2], subject)
    const report = f.run(f.session, followUp, head)
    assert.equal(report.decision, DECISIONS.REFUSED, JSON.stringify(report))
    assert.deepEqual(report.reasons, ['start-head-not-receipted'])
    assert.equal(report.baselineSha, undefined)
  })

test('a receipted head a fresh clone started from is outside the lookback', (t) => {
  const f = remoteFixture(t)
  const fresh = f.clone('fresh-clone')
  const followUp = fresh.commit('c.txt', 'layout tweak\n', 'follow-up')
  fresh.git('push', '-q')
  const head = fresh.commit('d.txt', 'fix\n', 'repair fix')
  fresh.git('push', '-q')
  const entries = fresh.git('reflog', 'show', '--format=%H %gs', 'refs/remotes/origin/feature')
  assert.ok(
    !entries.split('\n').includes(`${f.receipted} update by push`),
    'the clone did not log the receipted head as a push',
  )
  const report = f.run(fresh.dir, followUp, head)
  assert.equal(report.decision, DECISIONS.REFUSED, JSON.stringify(report))
  assert.deepEqual(report.reasons, ['start-head-not-receipted'])
  assert.equal(report.baselineSha, undefined)
})

// An entry written with no message, as some clones write one, has an empty subject.
test('an entry with an empty subject ends the lookback', (t) => {
  const f = fixture(t)
  f.git('update-ref', '--create-reflog', 'refs/remotes/origin/feature', f.start)
  const followUp = f.commit('c.txt', 'layout tweak\n', 'follow-up')
  f.pushed(followUp)
  const head = f.commit('d.txt', 'fix\n', 'repair fix')
  f.pushed(head)
  const report = f.run(head, [], {}, followUp)
  assert.equal(report.decision, DECISIONS.REFUSED, JSON.stringify(report))
  assert.deepEqual(report.reasons, ['start-head-not-receipted'])
  assert.equal(report.baselineSha, undefined)
})
