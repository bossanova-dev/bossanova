import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { sections } from './epic/merge-eligibility.mjs'
import { renderFollowUpSections, routeFollowUps } from './build/followup-route.mjs'
import { describeProbe } from './review/cross-review-lib.mjs'
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

for (const agent of ['claude', 'codex']) {
  for (const condition of ['not_authed', 'not_installed']) {
    test(`followups route ${agent} ${condition} as one canonical host note`, () => {
      const reason = `${agent}-review: ${describeProbe(agent, condition)}`
      const v = verdict([finding({ introduced: false })], { secondModel: { ran: false, reason } })
      const { out, code } = run(['followups', '/v.json'], withVerdict(v))
      assert.equal(code, 0)
      const candidates = JSON.parse(out)
      assert.equal(candidates.filter((c) => c.source === 'review-finding').length, 1)
      const hosts = candidates.filter((c) => c.source === 'host')
      assert.deepEqual(hosts, [
        { source: 'host', where: `cross-model review: ${agent} ${condition}`, text: reason },
      ])
      assert.equal(routeFollowUps(hosts).hostNotes[0].where, hosts[0].where)
    })
  }
}
test('host candidates fall back to the verdict agent or second-model', () => {
  for (const agent of ['codex', undefined]) {
    const v = verdict([], {
      secondModel: { ran: false, agent, reason: 'probe returned not_authed: login needed' },
    })
    const candidates = JSON.parse(run(['followups', '/v.json'], withVerdict(v)).out)
    assert.equal(candidates[0].where, `cross-model review: ${agent ?? 'second-model'} not_authed`)
  }
})
test('successful or transient second-model results do not produce host notes', () => {
  for (const secondModel of [
    { ran: true, agent: 'claude', reason: 'not_authed' },
    { ran: false, reason: 'error: readiness check failed' },
    { ran: false, reason: 'timed out' },
  ]) {
    assert.deepEqual(
      JSON.parse(run(['followups', '/v.json'], withVerdict(verdict([], { secondModel }))).out),
      [],
    )
  }
  assert.deepEqual(reviewVerdict.followUpCandidates({ status: 'error' }), [])
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

function resolutionPool(verdicts) {
  const files = {}
  const entries = { '/reviews': [] }
  for (const { pass, member = '1', value } of verdicts) {
    if (!entries[`/reviews/${pass}`]) {
      entries['/reviews'].push(directory(pass))
      entries[`/reviews/${pass}`] = []
    }
    entries[`/reviews/${pass}`].push(directory(member))
    files[`/reviews/${pass}/${member}/review-verdict.json`] = JSON.stringify(value)
  }
  const result = poolRun(entries, files)
  return { ...result, pooled: JSON.parse(files['/pooled.json']) }
}

test('later explicit resolutions retire P2/P3 findings from PR sections and report trailers', () => {
  for (const severity of ['P2', 'P3']) {
    const { pooled, err } = resolutionPool([
      { pass: '1', value: verdict([finding({ severity })]) },
      { pass: '2', value: verdict([], { resolves: ['1.1:f1'], head: 'c'.repeat(40) }) },
    ])
    const f = pooled.findings[0]
    assert.equal(f.fixed, true)
    assert.equal(f.ref, '1.1:f1')
    assert.equal(f.fixCommit, 'c'.repeat(40))
    assert.equal(f.resolvedBy, '2.1')
    assert.equal(pooled.resolves, undefined)
    const result = verdictStatus(pooled)
    assert.doesNotMatch(reviewVerdict.renderPrSection(result), /## Unapplied review findings/)
    assert.match(reviewVerdict.renderReport(result), /### Fixed in review/)
    assert.deepEqual(
      reviewVerdict.parseReportFindings(reviewVerdict.renderReport(result)).findings,
      [],
    )
    assert.equal(err, '')
  }
})

test('resolved pre-existing P0/P1 findings leave followups', () => {
  for (const severity of ['P0', 'P1']) {
    const { pooled } = resolutionPool([
      { pass: '1', value: verdict([finding({ severity, introduced: false })]) },
      { pass: '2', value: verdict([], { resolves: ['1.1:f1'] }) },
    ])
    assert.equal(run(['followups', '/v.json'], withVerdict(pooled)).out, '[]\n')
  }
})

test('pool cannot clear a blocking finding and keeps stdout to the capped note', () => {
  const result = resolutionPool([
    { pass: '1', value: verdict([finding()]) },
    { pass: '2', value: verdict([], { resolves: ['1.1:f1'] }) },
  ])
  assert.equal(result.pooled.findings[0].fixed, false)
  assert.equal(result.pooled.status, 'blocked')
  assert.equal(result.out, 'capped\n')
  assert.match(result.err, /1\.1:f1.*blocking/)
})

test('pool warns for unknown, same-pass and later-pass resolution refs', () => {
  const result = resolutionPool([
    {
      pass: '1',
      value: verdict([finding({ severity: 'P3' })], {
        resolves: ['1.1:f1', '2.1:f1', '9.1:missing'],
      }),
    },
    { pass: '2', value: verdict([finding({ severity: 'P3' })]) },
  ])
  assert.ok(result.pooled.findings.every((f) => !f.fixed))
  for (const ref of ['1.1:f1', '2.1:f1', '9.1:missing']) assert.ok(result.err.includes(ref))
  assert.equal(result.err.trim().split('\n').length, 3)
})

test('resolves validation drops malformed values and deduplicates without invalidating', () => {
  for (const resolves of ['1.1:f1', ['f1', 1]]) {
    const result = run(['validate', '/v.json'], withVerdict(verdict([], { resolves })))
    assert.equal(result.code, 0)
    assert.match(result.err, /resolves/)
    assert.deepEqual(reviewVerdict.validateVerdict(verdict([], { resolves })).verdict.resolves, [])
  }
  assert.deepEqual(reviewVerdict.validateVerdict(verdict()).verdict.resolves, [])
  assert.deepEqual(
    reviewVerdict.validateVerdict(verdict([], { resolves: ['1.1:f1', '1.1:f1'] })).verdict.resolves,
    ['1.1:f1'],
  )
  const result = resolutionPool([{ pass: '1', value: verdict([], { resolves: ['bad'] }) }])
  assert.match(result.err, /resolves/)
  assert.equal(result.out, 'clean\n')
})

test('pool stamps stable refs without changing merge ids and disambiguates chain members', () => {
  const first = verdict([finding({ severity: 'P3' })])
  const single = resolutionPool([{ pass: '1', value: first }]).pooled
  assert.equal(single.findings[0].ref, '1.1:f1')
  assert.equal(
    single.findings[0].id,
    reviewVerdict.mergeVerdicts([verdictStatus(first)]).findings[0].id,
  )
  const values = [first, first, verdict([], { resolves: ['1.2:f1'] })]
  const { pooled } = resolutionPool([
    { pass: '1', member: '1', value: values[0] },
    { pass: '1', member: '2', value: values[1] },
    { pass: '2', value: values[2] },
  ])
  assert.deepEqual(
    pooled.findings.map((f) => f.id),
    reviewVerdict.mergeVerdicts(values.map(verdictStatus)).findings.map((f) => f.id),
  )
  assert.deepEqual(
    pooled.findings.map((f) => f.fixed),
    [false, true],
  )
})

test('appending a passing review resolving every earlier ref never improves the pooled note', () => {
  const ranks = { clean: 0, capped: 1, none: 2 }
  const outcomes = [
    verdict([finding({ severity: 'P3' })]),
    verdict([finding()]),
    { version: 1, status: 'error', error: 'dispatch failed' },
  ]
  let sequences = [[]]
  for (let length = 1; length <= 3; length++) {
    sequences = sequences.flatMap((sequence) => outcomes.map((value) => [...sequence, value]))
    for (const sequence of sequences) {
      const entries = sequence.map((value, i) => ({ pass: String(i + 1), value }))
      const before = resolutionPool(entries).out.trim()
      const after = resolutionPool([
        ...entries,
        {
          pass: String(length + 1),
          value: verdict([], { resolves: sequence.map((_, i) => `${i + 1}.1:f1`) }),
        },
      ]).out.trim()
      assert.ok(ranks[after] >= ranks[before])
    }
  }
})

test('resolvable lists only verified unfixed nonblocking ref-bearing findings', () => {
  const findings = [
    finding({ id: 'p2', severity: 'P2', ref: '1.1:p2' }),
    finding({ id: 'old', introduced: false, ref: '1.1:old' }),
    finding({ id: 'block', ref: '1.1:block' }),
    finding({ id: 'unverified', severity: 'P3', verified: false, ref: '1.1:unverified' }),
    finding({ id: 'fixed', severity: 'P3', fixed: true, fixCommit: HEAD, ref: '1.1:fixed' }),
    finding({ id: 'no-ref', severity: 'P3' }),
  ]
  const result = run(['resolvable', '/v.json'], withVerdict(verdict(findings)))
  assert.equal(result.code, 0)
  assert.deepEqual(
    JSON.parse(result.out),
    findings.slice(0, 2).map(({ ref, severity, introduced, file, line, title }) => ({
      ref,
      severity,
      introduced,
      file,
      line,
      title,
    })),
  )
  assert.equal(run(['resolvable', '/missing']).code, 2)
  assert.equal(
    run(['resolvable', '/v.json'], withVerdict({ version: 1, status: 'error', error: 'failed' }))
      .code,
    2,
  )
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

test('standing report findings round trip and reject malformed trailers', () => {
  const open = finding({ severity: 'P2', evidence: 'details --> safe' })
  const result = verdictStatus(
    verdict([open, finding({ id: 'fixed', fixed: true }), finding({ id: 'raw', verified: false })]),
  )
  const parsed = reviewVerdict.parseReportFindings(reviewVerdict.renderReport(result))
  assert.deepEqual(parsed, {
    base: BASE,
    head: HEAD,
    findings: [
      Object.fromEntries(
        ['id', 'severity', 'file', 'line', 'title', 'evidence', 'introduced'].map((k) => [
          k,
          open[k],
        ]),
      ),
    ],
  })
  for (const body of ['', '<!-- bs-review-findings v1 base=x head=y eA -->'])
    assert.equal(reviewVerdict.parseReportFindings(body), null)
})
test('standing report bounds large evidence while preserving titles', () => {
  const body = reviewVerdict.renderReport(
    verdictStatus(verdict([finding({ evidence: 'x'.repeat(100000), title: 'keep title' })])),
  )
  const parsed = reviewVerdict.parseReportFindings(body)
  assert.equal(parsed.findings[0].title, 'keep title')
  assert.ok(parsed.findings[0].evidence.endsWith('…'))
  assert.ok(body.length < 65536)
})

test('report trailers preserve category-style notes for the shared note predicate', () => {
  const result = verdictStatus(verdict([finding({ severity: 'P2', category: 'style' })]))
  const parsed = reviewVerdict.parseReportFindings(reviewVerdict.renderReport(result))
  assert.equal(parsed.findings[0].severity, 'style')
})
