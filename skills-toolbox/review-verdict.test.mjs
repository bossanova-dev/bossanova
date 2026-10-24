import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { sections } from './epic/merge-eligibility.mjs'
import { renderFollowUpSections, routeFollowUps } from './build/followup-route.mjs'
import * as reviewVerdict from './review-verdict.mjs'
import { REPORT_MARKER, main, verdictStatus } from './review-verdict.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const BASE = 'a'.repeat(40)
const HEAD = 'b'.repeat(40)

const finding = (over = {}) => ({
  id: 'f1',
  severity: 'P1',
  introduced: true,
  file: 'src/a.go',
  line: 12,
  title: 'nil map write',
  evidence: 'm is never initialised before m[k] = v on line 12',
  source: 'correctness',
  verified: true,
  fixed: false,
  ...over,
})

const verdict = (findings = [], over = {}) => ({
  version: 1,
  status: 'pass',
  base: BASE,
  head: HEAD,
  findings,
  fixPass: { ran: false },
  secondModel: { ran: false, reason: 'codex not installed' },
  ...over,
})

function run(argv, files = {}, overrides = {}) {
  const out = []
  const err = []
  const memFs = {
    writeFileSync: (p, data) => {
      files[p] = String(data)
    },
    readFileSync: (p) => {
      if (!(p in files)) throw Object.assign(new Error('ENOENT'), { code: 'ENOENT' })
      return files[p]
    },
  }
  const code = main(argv, {
    fs: { ...memFs, ...overrides },
    stdout: (s) => out.push(s),
    stderr: (s) => err.push(s),
  })
  return { code, out: out.join(''), err: err.join('') }
}

const withVerdict = (v) => ({ '/v.json': JSON.stringify(v) })

test('only a verified, introduced, unfixed P0/P1 finding blocks', () => {
  const cases = [
    [finding(), 'blocked'],
    [finding({ severity: 'P0' }), 'blocked'],
    [finding({ severity: 'P2' }), 'pass'],
    [finding({ severity: 'P3' }), 'pass'],
    [finding({ verified: false }), 'pass'],
    [finding({ introduced: false }), 'pass'],
    [finding({ fixed: true, fixCommit: 'c'.repeat(40) }), 'pass'],
  ]
  for (const [f, want] of cases)
    assert.equal(verdictStatus(verdict([f], { status: want })).status, want, JSON.stringify(f))
})

test('a stated status that contradicts the findings is corrected and warned, never trusted', () => {
  const lying = verdictStatus(verdict([finding()], { status: 'pass' }))
  assert.equal(lying.status, 'blocked')
  assert.ok(lying.warnings.some((w) => w.includes('contradicts')))
  const pessimist = verdictStatus(verdict([finding({ severity: 'P3' })], { status: 'blocked' }))
  assert.equal(pessimist.status, 'pass')
})

test('an error verdict stays an error whatever its findings say', () => {
  const r = verdictStatus({ version: 1, status: 'error', error: 'finder dispatch failed' })
  assert.equal(r.status, 'error')
  assert.deepEqual(r.errors, [])
})

test('cosmetic slips are normalised toward blocking; unsafe gaps are errors', () => {
  const sloppy = verdictStatus(
    verdict([
      {
        ...finding({ severity: 'p1', line: '12' }),
        introduced: undefined,
        fixed: undefined,
        source: undefined,
      },
    ]),
  )
  assert.equal(sloppy.status, 'blocked')
  assert.ok(sloppy.warnings.length >= 4)
  assert.equal(sloppy.verdict.findings[0].source, 'unknown')

  const noVerified = finding()
  delete noVerified.verified
  assert.equal(verdictStatus(verdict([noVerified])).status, 'error')
  assert.equal(verdictStatus(verdict([], { head: 'not-a-sha' })).status, 'error')
  assert.equal(verdictStatus(verdict([finding(), finding()])).status, 'error') // duplicate id
  assert.equal(verdictStatus(verdict([], { version: 2 })).status, 'error')
})

test('status CLI prints the recomputed word; an unreadable file is error with exit 2', () => {
  assert.deepEqual(run(['status', '/v.json'], withVerdict(verdict([finding()]))).out, 'blocked\n')
  const missing = run(['status', '/missing.json'])
  assert.equal(missing.out, 'error\n')
  assert.equal(missing.code, 2)
  const garbled = run(['status', '/v.json'], { '/v.json': '{not json' })
  assert.equal(garbled.out, 'error\n')
  assert.equal(garbled.code, 2)
})

test('validate exits 0 for a valid verdict and 2 for an invalid one', () => {
  assert.equal(run(['validate', '/v.json'], withVerdict(verdict())).code, 0)
  assert.equal(run(['validate', '/v.json'], withVerdict({ status: 'pass' })).code, 2)
})

test('usage errors exit 2', () => {
  assert.equal(run(['status']).code, 2)
  assert.equal(run(['bogus', '/v.json']).code, 2)
  assert.equal(run(['status', '/v.json', '--report']).code, 2)
  assert.equal(run(['render', '/v.json', '--pr-section', '--report']).code, 2)
  assert.equal(run(['render', '/v.json', '--unknown']).code, 2)
})

test('a passing PR section carries no Review findings heading; a blocked one names file:line', () => {
  const pass = run(
    ['render', '/v.json', '--pr-section'],
    withVerdict(verdict([finding({ severity: 'P2' })])),
  )
  const passSections = sections(pass.out)
  assert.equal(passSections.has('review findings'), false)
  assert.ok(passSections.get('unapplied review findings').some((l) => l.includes('src/a.go:12')))

  const blocked = run(['render', '/v.json', '--pr-section'], withVerdict(verdict([finding()])))
  assert.ok(
    sections(blocked.out)
      .get('review findings')
      .some((l) => l.includes('src/a.go:12')),
  )

  const error = run(['render', '/v.json', '--pr-section'], { '/v.json': '[]' })
  assert.equal(error.code, 2)
  assert.ok(sections(error.out).has('review findings'))
})

test('PR sections always carry the coverage and cross-model tokens the review earned', () => {
  const ran = verdict([], { secondModel: { ran: true, agent: 'codex' } })
  const ok = sections(run(['render', '/v.json', '--pr-section'], withVerdict(ran)).out)
  assert.deepEqual(ok.get('review coverage').filter(Boolean), ['full'])
  assert.deepEqual(ok.get('cross-model review').filter(Boolean), ['ran (codex)'])
  const skipped = sections(run(['render', '/v.json', '--pr-section'], withVerdict(verdict())).out)
  assert.match(skipped.get('cross-model review').find(Boolean), /^skipped: codex not installed/)
  const broken = sections(run(['render', '/v.json', '--pr-section'], {}).out)
  assert.match(broken.get('review coverage').find(Boolean), /^none: /)
  assert.match(broken.get('cross-model review').find(Boolean), /^error: /)
})

test('merge pools a chain into one verdict; any unusable member makes it an error', () => {
  const files = {
    '/a.json': JSON.stringify(verdict([finding({ severity: 'P3' })])),
    '/b.json': JSON.stringify(verdict([finding()], { base: 'c'.repeat(40), head: 'd'.repeat(40) })),
  }
  const merged = run(['merge', '--out', '/m.json', '/a.json', '/b.json'], files)
  assert.equal(merged.out, 'blocked\n')
  const pooled = JSON.parse(files['/m.json'])
  assert.equal(pooled.findings.length, 2) // same id in both members stays distinct
  assert.equal(pooled.base, BASE)
  assert.equal(pooled.head, 'd'.repeat(40))
  assert.equal(run(['status', '/m.json'], files).out, 'blocked\n')

  const withMissing = run(['merge', '--out', '/m.json', '/a.json', '/gone.json'], files)
  assert.equal(withMissing.code, 2)
  assert.equal(run(['status', '/m.json'], files).out, 'error\n')
  assert.equal(run(['merge', '/a.json'], files).code, 2) // --out is required
})

test('the report leads with the comment marker so PR upserts find it, in every status', () => {
  for (const files of [withVerdict(verdict([finding()])), withVerdict(verdict()), {}]) {
    const { out } = run(['render', '/v.json', '--report'], files)
    assert.equal(out.split('\n')[0], REPORT_MARKER)
  }
})

test('followups emit only serious pre-existing findings, routed to ## Review follow-ups', () => {
  const v = verdict([
    finding({ id: 'blocker' }),
    finding({ id: 'old-p1', introduced: false, file: 'old.go', line: 4 }),
    finding({ id: 'old-p3', introduced: false, severity: 'P3' }),
    finding({ id: 'unverified', introduced: false, verified: false }),
    finding({ id: 'fixed', introduced: false, fixed: true }),
  ])
  const { out, code } = run(['followups', '/v.json'], withVerdict(v))
  assert.equal(code, 0)
  const candidates = JSON.parse(out)
  assert.equal(candidates.length, 1)
  assert.ok(candidates[0].text.includes('old.go:4'))

  const routed = routeFollowUps(candidates)
  assert.deepEqual(routed.warnings, [])
  const body = renderFollowUpSections(routed)
  assert.deepEqual(routed.humanFollowUp, [])
  assert.ok(sections(body).has('review follow-ups'))
})

test('followups on an unusable verdict exit 2 rather than print an empty list', () => {
  const r = run(['followups', '/missing.json'])
  assert.equal(r.code, 2)
  assert.equal(r.out, '')
})

test('the CLI runs as an entry point', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'review-verdict-'))
  const file = path.join(dir, 'review-verdict.json')
  fs.writeFileSync(file, JSON.stringify(verdict([finding()])))
  const r = spawnSync(process.execPath, [path.join(HERE, 'review-verdict.mjs'), 'status', file], {
    encoding: 'utf8',
  })
  fs.rmSync(dir, { recursive: true, force: true })
  assert.equal(r.status, 0)
  assert.equal(r.stdout, 'blocked\n')
})

const directory = (name, isDirectory = true) => ({ name, isDirectory: () => isDirectory })
const poolArgs = ['pool', '--root', '/reviews', '--out', '/pooled.json', '--note', '/note']
function poolRun(entries, files = {}, overrides = {}) {
  return run(poolArgs, files, {
    readdirSync: (dir, options) => {
      assert.deepEqual(options, { withFileTypes: true })
      if (!(dir in entries)) throw new Error(`unreadable directory ${dir}`)
      return entries[dir]
    },
    ...overrides,
  })
}

test('pool orders passes and members numerically and ignores other entries', () => {
  const entries = [
    { pass: '10', member: '1' },
    { pass: '2', member: '10' },
    { pass: '1', member: '1' },
    { pass: '2', member: '2' },
    { pass: 'notes', member: '1' },
    { pass: '2', member: 'notes' },
  ]
  assert.deepEqual(reviewVerdict.poolVerdictFiles(entries), [
    '1/1/review-verdict.json',
    '2/2/review-verdict.json',
    '2/10/review-verdict.json',
    '10/1/review-verdict.json',
  ])
  const files = {}
  for (const [pass, head] of [
    ['1', BASE],
    ['2', 'c'.repeat(40)],
    ['10', HEAD],
  ])
    files[`/reviews/${pass}/1/review-verdict.json`] = JSON.stringify(
      verdict([], { base: pass === '1' ? BASE : 'd'.repeat(40), head }),
    )
  const result = poolRun(
    {
      '/reviews': [
        directory('10'),
        directory('skills.txt', false),
        directory('notes'),
        directory('2'),
        directory('1'),
      ],
      '/reviews/1': [directory('1'), directory('notes'), directory('2', false)],
      '/reviews/2': [directory('1')],
      '/reviews/10': [directory('1')],
    },
    files,
  )
  assert.equal(result.code, 0)
  assert.equal(result.out, 'clean\n')
  assert.equal(files['/note'], 'REVIEW_VERDICT=clean\n')
  const pooled = JSON.parse(files['/pooled.json'])
  assert.equal(pooled.base, BASE)
  assert.equal(pooled.head, HEAD)
})

test('pool writes error/none for a missing member verdict, an empty root or an unreadable directory', () => {
  for (const entries of [
    { '/reviews': [directory('1')], '/reviews/1': [directory('1')] },
    { '/reviews': [] },
    {},
  ]) {
    const files = {}
    const result = poolRun(entries, files)
    assert.equal(result.code, 0)
    assert.equal(result.out, 'none\n')
    assert.equal(files['/note'], 'REVIEW_VERDICT=none\n')
    assert.equal(JSON.parse(files['/pooled.json']).status, 'error')
  }
})

test('pool rejects bad arguments and failed writes with exit 2 and stderr only', () => {
  for (const args of [
    ['pool'],
    [...poolArgs, 'extra'],
    poolArgs.slice(0, -2),
    [...poolArgs, '--report'],
    ['status', '/v.json', '--root', '/reviews'],
    ['merge', '--out', '/m.json', '/v.json', '--note', '/note'],
  ]) {
    const result = run(args)
    assert.equal(result.code, 2)
    assert.equal(result.out, '')
    assert.ok(result.err)
  }
  for (const failingPath of ['/pooled.json', '/note']) {
    const result = poolRun(
      { '/reviews': [] },
      {},
      {
        writeFileSync: (file) => {
          if (file === failingPath) throw new Error('write denied')
        },
      },
    )
    assert.equal(result.code, 2)
    assert.equal(result.out, '')
    assert.match(result.err, /write denied/)
  }
})

test('appending a passing review never improves the pooled note for sequences up to three passes', () => {
  const ranks = { clean: 0, capped: 1, none: 2 }
  const outcomes = {
    pass: verdictStatus(verdict()),
    blocked: verdictStatus(verdict([finding()])),
    error: verdictStatus({ version: 1, status: 'error', error: 'dispatch failed' }),
  }
  let sequences = [[]]
  for (let length = 1; length <= 3; length++) {
    sequences = sequences.flatMap((sequence) =>
      Object.values(outcomes).map((result) => [...sequence, result]),
    )
    for (const sequence of sequences) {
      const before = reviewVerdict.noteVerdict(reviewVerdict.mergeVerdicts(sequence).status)
      const after = reviewVerdict.noteVerdict(
        reviewVerdict.mergeVerdicts([...sequence, outcomes.pass]).status,
      )
      assert.ok(ranks[after] >= ranks[before])
    }
  }
  assert.equal(reviewVerdict.noteVerdict('unexpected'), 'none')
})

test('pool CLI discovers real directories and writes the capped note for a blocking review', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'review-verdict-pool-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const root = path.join(dir, 'reviews')
  const member = path.join(root, '1', '1')
  fs.mkdirSync(member, { recursive: true })
  fs.writeFileSync(path.join(member, 'review-verdict.json'), JSON.stringify(verdict([finding()])))
  const out = path.join(dir, 'verdict.json')
  const note = path.join(dir, 'note')
  const result = spawnSync(
    process.execPath,
    [path.join(HERE, 'review-verdict.mjs'), 'pool', '--root', root, '--out', out, '--note', note],
    { encoding: 'utf8' },
  )
  assert.equal(result.status, 0)
  assert.equal(result.stdout, 'capped\n')
  assert.equal(fs.readFileSync(note, 'utf8'), 'REVIEW_VERDICT=capped\n')
  assert.equal(JSON.parse(fs.readFileSync(out, 'utf8')).status, 'blocked')
  assert.equal(reviewVerdict.noteVerdict('pass'), 'clean')
  assert.equal(reviewVerdict.noteVerdict('blocked'), 'capped')
  assert.equal(reviewVerdict.noteVerdict('error'), 'none')
})
