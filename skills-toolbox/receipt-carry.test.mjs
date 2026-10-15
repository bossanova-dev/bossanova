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

function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'receipt-carry-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const git = (...args) => {
    const result = spawnSync(
      'git',
      [
        '-c',
        'user.name=T',
        '-c',
        'user.email=t@example.invalid',
        '-c',
        'commit.gpgsign=false',
        ...args,
      ],
      { cwd: dir, encoding: 'utf8' },
    )
    assert.equal(result.status, 0, result.stderr)
    return result.stdout.trim()
  }
  const commit = (file, content, message) => {
    writeFileSync(join(dir, file), content)
    git('add', file)
    git('commit', '-m', message)
    return git('rev-parse', 'HEAD')
  }
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
  const spawn = (command, args, options) =>
    command === 'gh' ? gh(args) : spawnSync(command, args, options)
  const run = (head, reviewVerdicts = [], config = {}) =>
    carryReport(
      { startSha: start, head, reviewVerdicts },
      { spawn, cwd: dir, loadConfig: () => config },
    )
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
  return { dir, git, commit, onMain, start, receipts, posts, run, verdict }
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
