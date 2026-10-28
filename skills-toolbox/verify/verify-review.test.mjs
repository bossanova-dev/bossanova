import test from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { parsePostOptions } from './verify-gate.mjs'
import {
  DECISIONS,
  RECEIPT_DESCRIPTION_PREFIX,
  UsageError,
  exitCodeFor,
  parseCli,
  planReview,
  recordReview,
} from './verify-review.mjs'

const IDENT = [
  '-c',
  'user.name=T',
  '-c',
  'user.email=t@example.invalid',
  '-c',
  'commit.gpgsign=false',
]

// git runs for real in the fixture; gh answers the PR view from `view`, a `boss/build` read from
// `receipts`, and records every status post and every call.
function fixture(t, { baseConfig } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'verify-review-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
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
  git('init', '--initial-branch=main')
  git('config', 'gc.auto', '0')
  const base = baseConfig
    ? commit('.boss-skills.json', JSON.stringify(baseConfig), 'config')
    : commit('a.txt', 'base\n', 'base')
  git('update-ref', 'refs/remotes/origin/main', 'HEAD')
  git('checkout', '-b', 'feature')
  const head = commit('b.txt', 'human change\n', 'a human-crafted change')

  const world = {
    view: { headRefOid: head, baseRefName: 'main', isCrossRepository: false },
    receipts: new Set(),
    posts: [],
    gh: [],
  }
  const spawn = (command, args, options) => {
    if (command !== 'gh') return spawnSync(command, args, options)
    world.gh.push(args)
    if (args[0] === 'pr') return { status: 0, stdout: JSON.stringify(world.view) }
    if (args.includes('POST')) {
      world.posts.push(args)
      return { status: 0, stdout: '{}' }
    }
    const target = args[1].match(/commits\/([0-9a-f]{40})\/statuses/)?.[1]
    const statuses = world.receipts.has(target)
      ? [{ context: 'boss/build', state: 'success', updated_at: '2026-10-28T00:00:00Z' }]
      : []
    return { status: 0, stdout: JSON.stringify([statuses]) }
  }
  const plan = (options = {}) => planReview({ pr: 7, head, ...options }, { spawn, cwd: dir })
  const record = (options = {}, deps = {}) =>
    recordReview({ pr: 7, head, ...options }, { spawn, cwd: dir, ...deps })
  const verdict = (file, { base: from = base, head: to = head, findings = [], status } = {}) => {
    writeFileSync(
      file,
      JSON.stringify(
        status === 'error'
          ? { version: 1, status: 'error', error: 'reviewer crashed' }
          : {
              version: 1,
              status: findings.length ? 'blocked' : 'pass',
              base: from,
              head: to,
              findings,
              fixPass: { ran: false },
              secondModel: { ran: true, agent: 'codex' },
            },
      ),
    )
    return file
  }
  return { dir, git, commit, base, head, world, plan, record, verdict }
}

const BLOCKER = {
  id: 'f1',
  severity: 'P1',
  introduced: true,
  file: 'b.txt',
  line: 1,
  title: 'the change drops a required check',
  evidence: 'b.txt:1 removes it',
  source: 'correctness',
  verified: true,
}

const receiptPosts = (world) =>
  world.posts.filter(
    (args) => args.includes('context=boss/build') && args.includes('state=success'),
  )

// ---------------------------------------------------------------------------
// plan

test('plan names the base tip review chain, the merge base and one output dir per skill', (t) => {
  const f = fixture(t, { baseConfig: { review: { skill: ['first-review', 'second-review'] } } })
  const report = f.plan()
  assert.equal(report.decision, DECISIONS.REVIEW)
  assert.equal(exitCodeFor(report), 0)
  assert.deepEqual(report.review.skills, ['first-review', 'second-review'])
  assert.equal(report.review.base, f.base)
  assert.equal(report.review.head, f.head)
  assert.equal(report.review.outDirs.length, 2)
  for (const dir of report.review.outDirs) assert.ok(existsSync(dir))
  assert.ok(report.review.hangGuardMs > 0)
})

test('plan defaults to boss-review when the base tip has no config', (t) => {
  const f = fixture(t)
  assert.deepEqual(f.plan().review.skills, ['boss-review'])
})

test('plan ignores a review chain the head itself configures', (t) => {
  const f = fixture(t)
  const head = f.commit(
    '.boss-skills.json',
    '{"review":{"skill":["self-review"]}}',
    'pick reviewer',
  )
  f.world.view.headRefOid = head
  const report = f.plan({ head })
  assert.equal(report.decision, DECISIONS.REVIEW)
  assert.deepEqual(report.review.skills, ['boss-review'])
})

test('plan refuses a fork before reading anything else', (t) => {
  const f = fixture(t)
  f.world.view.isCrossRepository = true
  const report = f.plan()
  assert.equal(report.decision, DECISIONS.REFUSED)
  assert.deepEqual(report.reasons, ['fork'])
  assert.equal(exitCodeFor(report), 3)
  assert.equal(report.review, undefined)
  assert.deepEqual(
    f.world.gh.map((args) => args[0]),
    ['pr'],
  )
})

test('plan refuses a view that does not say the PR is not a fork', (t) => {
  const f = fixture(t)
  delete f.world.view.isCrossRepository
  assert.deepEqual(f.plan().reasons, ['fork-unknown'])
})

test('plan refuses a head the PR has moved past', (t) => {
  const f = fixture(t)
  f.world.view.headRefOid = 'c'.repeat(40)
  const report = f.plan()
  assert.equal(report.decision, DECISIONS.REFUSED)
  assert.deepEqual(report.reasons, ['head-moved'])
})

test('plan refuses a head that already carries its receipt', (t) => {
  const f = fixture(t)
  f.world.receipts.add(f.head)
  const report = f.plan()
  assert.equal(report.decision, DECISIONS.REFUSED)
  assert.deepEqual(report.reasons, ['already-receipted'])
})

// ---------------------------------------------------------------------------
// record

test('a clean review posts boss/build success through the ready gate', (t) => {
  const f = fixture(t)
  const [file] = f.plan().review.verdictFiles
  f.verdict(file)
  let gated
  const report = f.record(
    {},
    {
      readyGateReport: (options) => {
        gated = options
        return { ready: true, reasons: [] }
      },
    },
  )
  assert.equal(report.decision, DECISIONS.RECEIPTED)
  assert.equal(exitCodeFor(report), 0)
  assert.equal(gated.head, f.head)
  assert.match(readFileSync(gated.reviewVerdictFile, 'utf8'), /^REVIEW_VERDICT=clean$/m)
  assert.equal(readFileSync(gated.reviewedHeadFile, 'utf8').trim(), f.head)
  const posts = receiptPosts(f.world)
  assert.equal(posts.length, 1)
  assert.ok(posts[0].some((a) => a.endsWith(`/statuses/${f.head}`)))
  assert.ok(
    posts[0].includes(`description=${RECEIPT_DESCRIPTION_PREFIX}${f.head.slice(0, 12)}`),
    posts[0].join(' '),
  )
})

test('a clean review passes the real ready gate on the reviewed head', (t) => {
  const f = fixture(t)
  const [file] = f.plan().review.verdictFiles
  f.verdict(file, { base: f.base.slice(0, 12), head: f.head.slice(0, 12) })
  const report = f.record()
  assert.equal(report.decision, DECISIONS.RECEIPTED, JSON.stringify(report))
  assert.equal(receiptPosts(f.world).length, 1)
})

test('a blocked review writes findings verify-gate post --verdict defect accepts', (t) => {
  const f = fixture(t)
  const [file] = f.plan().review.verdictFiles
  f.verdict(file, { findings: [BLOCKER, { ...BLOCKER, id: 'f2', severity: 'P3' }] })
  const report = f.record()
  assert.equal(report.decision, DECISIONS.FINDINGS)
  assert.equal(report.verdict, 'defect')
  assert.equal(exitCodeFor(report), 3)
  const parsed = parsePostOptions({
    verdict: 'defect',
    pr: 7,
    head: f.head,
    findings: report.findingsFile,
  })
  assert.equal(parsed.findings.length, 1)
  assert.equal(parsed.findings[0].file, BLOCKER.file)
  assert.equal(f.world.posts.length, 0)
})

for (const [label, arrange, why] of [
  [
    'a review of another head',
    (f, [file]) => f.verdict(file, { head: 'd'.repeat(40) }),
    'review-range-mismatch',
  ],
  [
    'a review from another base',
    (f, [file]) => f.verdict(file, { base: f.head }),
    'review-range-mismatch',
  ],
  ['an error verdict', (f, [file]) => f.verdict(file, { status: 'error' }), 'review-error'],
  ['a review that wrote no verdict', () => {}, 'review-error'],
])
  test(`${label} is unusable and posts nothing`, (t) => {
    const f = fixture(t)
    arrange(f, f.plan().review.verdictFiles)
    const report = f.record()
    assert.equal(report.decision, DECISIONS.UNUSABLE)
    assert.equal(report.why, why)
    assert.equal(exitCodeFor(report), 3)
    assert.equal(f.world.posts.length, 0)
    // The unusable answer is a park verify-gate post accepts as it stands.
    const parsed = parsePostOptions({
      verdict: report.verdict,
      reason: report.reason,
      pr: 7,
      head: f.head,
    })
    assert.deepEqual(parsed.humanCodes, ['extension-failed'])
  })

test('a chain with a verdict missing is unusable', (t) => {
  const f = fixture(t, { baseConfig: { review: { skill: ['first-review', 'second-review'] } } })
  const [first] = f.plan().review.verdictFiles
  f.verdict(first)
  const report = f.record({ reviewVerdicts: [first] })
  assert.equal(report.decision, DECISIONS.UNUSABLE)
  assert.equal(report.why, 'review-incomplete')
  assert.equal(f.world.posts.length, 0)
})

test('a record with no plan for the head is unusable', (t) => {
  const f = fixture(t)
  const report = f.record()
  assert.equal(report.decision, DECISIONS.UNUSABLE)
  assert.equal(report.why, 'no-plan')
  assert.equal(f.world.posts.length, 0)
})

test('a ready gate refusal is unusable and posts nothing', (t) => {
  const f = fixture(t)
  const [file] = f.plan().review.verdictFiles
  f.verdict(file)
  const report = f.record(
    {},
    { readyGateReport: () => ({ ready: false, reasons: ['tree-changed-since-review'] }) },
  )
  assert.equal(report.decision, DECISIONS.UNUSABLE)
  assert.equal(report.reason, 'extension-failed')
  assert.equal(f.world.posts.length, 0)
})

test('a fresh plan drops a verdict an earlier run left behind', (t) => {
  const f = fixture(t)
  const [file] = f.plan().review.verdictFiles
  f.verdict(file)
  f.plan()
  assert.equal(existsSync(file), false)
  assert.equal(f.record().why, 'review-error')
})

// ---------------------------------------------------------------------------
// CLI surface

test('parseCli is strict about its verb and flags', () => {
  const head = 'a'.repeat(40)
  assert.throws(() => parseCli(['--pr', '7', '--head', head]), UsageError)
  assert.throws(() => parseCli(['plan', '--pr', '7', '--head', 'abc']), UsageError)
  assert.throws(() => parseCli(['plan', '--pr', 'x', '--head', head]), UsageError)
  assert.throws(() => parseCli(['plan', '--pr', '7', '--head', head, '--bogus', 'x']), UsageError)
  assert.throws(
    () => parseCli(['plan', '--pr', '7', '--head', head, '--review-verdict', '/v.json']),
    UsageError,
  )
  const parsed = parseCli([
    'record',
    '--pr',
    '7',
    '--head',
    head,
    '--review-verdict',
    '/1.json',
    '--review-verdict',
    '/2.json',
  ])
  assert.deepEqual(parsed.reviewVerdicts, ['/1.json', '/2.json'])
  assert.equal(parsed.pr, 7)
})

for (const [decision, code] of [
  ['review', 0],
  ['receipted', 0],
  ['refused', 3],
  ['findings', 3],
  ['unusable', 3],
  ['failed', 1],
])
  test(`exitCodeFor ${decision} is ${code}`, () => assert.equal(exitCodeFor({ decision }), code))
