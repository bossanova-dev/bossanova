import test from 'node:test'
import assert from 'node:assert/strict'
import {
  collect,
  main,
  MAX_RATE_PRS,
  TRACE_TIMEOUT_MS,
  LIST_TIMEOUT_MS,
  CAPS_TIMEOUT_MS,
} from './retro-outcomes-report.mjs'
import { escapeRecord, renderEscapeComment, parseEscapeComment } from '../verify/verify-escapes.mjs'
import { RECEIPT_CONTEXT } from '../commit-status.mjs'
import { VERIFY_CONTEXT, VERIFY_DESCRIPTIONS } from '../verify/verify-vocabulary.mjs'

const now = () => new Date('2026-11-01T12:00:00Z')
const ok = (value) => ({
  status: 0,
  stdout: typeof value === 'string' ? value : JSON.stringify(value),
})
function fixture({
  prs = [],
  failure = '',
  repoId = 'local',
  lastRunAt,
  more = false,
  tracer = true,
  caps = true,
  history = ok({ repo: 'owner/repo', episodes: [], unreadable: 0 }),
} = {}) {
  const calls = [],
    writes = new Map()
  const run = (cmd, args) => {
    calls.push([cmd, ...args])
    if (cmd === process.execPath)
      return args[0].endsWith('verify-area-caps-sync.mjs')
        ? history
        : ok(
            args[1] === 'trace'
              ? { links: [], skipped: [], truncated: false }
              : { escapes: [], unreadable: 0 },
          )
    if (cmd === 'git') return ok('/repo\n/common\n')
    if (args[0] === 'env') return ok({ session: { repo_id: repoId } })
    if (args[0] === 'cron')
      return ok([{ prompt: '/boss-verify', enabled: true, last_run_at: '2026-11-01T11:59:00Z' }])
    if (failure) return { status: 1, stderr: failure }
    if (args[0] === 'repo') return ok({ nameWithOwner: 'owner/repo' })
    if (args[1] === 'graphql')
      return ok({
        data: {
          search: { nodes: prs, pageInfo: { hasNextPage: more, endCursor: String(calls.length) } },
        },
      })
    return ok([])
  }
  const fs = {
    existsSync: (file) => (file.endsWith('verify-area-caps-sync.mjs') ? caps : tracer),
    readFileSync: () => JSON.stringify({ lastRunAt }),
    writeFileSync: (file, data) => writes.set(file, data),
  }
  return { calls, writes, deps: { run, fs, now, resolveBoss: () => ({ ok: true, path: '/boss' }) } }
}
const sections = ['mergesWithoutVerdict', 'unjudgedHeads', 'unnecessaryHolds', 'secondModelHealth']
test('GitHub failure preserves independently readable cron health and writes a report', () => {
  const f = fixture({ failure: 'API rate limit exceeded' })
  assert.equal(main(['report', '--out', '/out'], f.deps), 0)
  const report = JSON.parse(f.writes.get('/out'))
  for (const name of sections) assert.equal(report.sections[name].reason, 'rate-limited')
  assert.equal(report.sections.verifyCronHealth.status, 'ok')
  assert.equal(report.sections.verifyCronHealth.verifySilent, null)
})
test('missing repo id skips only cron health', () => {
  const report = collect({}, fixture({ repoId: '' }).deps)
  assert.equal(report.sections.verifyCronHealth.reason, 'no-repo-id')
  for (const name of sections) assert.equal(report.sections[name].status, 'ok')
})
test('window uses last run, seven-day fallback and thirty-one day clamp', () => {
  for (const [lastRunAt, expected] of [
    ['2026-10-30T00:00:00Z', '2026-10-30T00:00:00.000Z'],
    [undefined, '2026-10-25T12:00:00.000Z'],
    ['2020-01-01T00:00:00Z', '2026-10-01T12:00:00.000Z'],
  ])
    assert.equal(collect({}, fixture({ lastRunAt }).deps).window.start, expected)
  assert.equal(
    collect({ since: '2026-10-31T00:00:00Z' }, fixture().deps).window.start,
    '2026-10-31T00:00:00.000Z',
  )
})
function pr(number) {
  return {
    number,
    headRefOid: `head${number}`,
    commits: {
      nodes: [
        {
          commit: {
            oid: `head${number}`,
            statusCheckRollup: {
              contexts: {
                nodes: [
                  { context: RECEIPT_CONTEXT, state: 'SUCCESS', createdAt: '2026-11-01T10:00:00Z' },
                  {
                    context: VERIFY_CONTEXT,
                    description: VERIFY_DESCRIPTIONS.verifiedApproved,
                    createdAt: '2026-11-01T11:00:00Z',
                  },
                ],
              },
            },
          },
        },
      ],
    },
  }
}
test('PR and history caps mark truncated and bound reads', () => {
  const paged = fixture({ prs: Array.from({ length: 20 }, (_, i) => pr(i)), more: true })
  assert.equal(collect({}, paged.deps).truncated, true)
  assert.equal(
    paged.calls.filter((c) => c[2] === 'graphql' && c.some((a) => a.includes('updated:>='))).length,
    10,
  )
  const holds = fixture({ prs: Array.from({ length: 51 }, (_, i) => pr(i)) })
  assert.equal(collect({}, holds.deps).truncated, true)
  assert.equal(holds.calls.filter((c) => c[2]?.includes('/statuses')).length, 50)
})
test('strict CLI rejects unknown flags and invalid values before reads', () => {
  for (const args of [
    ['report', '--out', '/out', '--wat'],
    ['report'],
    ['report', '--out', '/out', '--hold-minutes', '-1'],
    ['report', '--out', '/out', '--since', 'no-date'],
    ['render'],
  ]) {
    const f = fixture()
    assert.equal(main(args, { ...f.deps, stderr: () => {} }), 64)
    assert.equal(f.calls.length, 0)
  }
})
test('all external commands are reads and history failure fails only holds', () => {
  const f = fixture({ prs: [pr(1)] })
  const base = f.deps.run
  f.deps.run = (cmd, args) =>
    args[1]?.includes('/statuses') ? { status: 1, stderr: 'rate limit' } : base(cmd, args)
  const report = collect({ dryRun: true }, f.deps)
  assert.equal(report.sections.unnecessaryHolds.status, 'failed')
  assert.equal(report.sections.mergesWithoutVerdict.status, 'ok')
  for (const call of f.calls) {
    assert.ok(!call.some((s) => /^(POST|PUT|PATCH|DELETE)$/.test(s)))
    assert.ok(['git', 'gh', '/boss', process.execPath].includes(call[0]))
    if (call[0] === '/boss') assert.ok(['env', 'cron'].includes(call[1]))
  }
})

test('earlier terminal verdicts survive later claim and wait snapshots', () => {
  for (const description of [VERIFY_DESCRIPTIONS.verifying, VERIFY_DESCRIPTIONS.waiting]) {
    const sample = pr(1)
    sample.mergedAt = '2026-11-01T11:30:00Z'
    sample.commits.nodes[0].commit.statusCheckRollup.contexts.nodes[1] = {
      context: VERIFY_CONTEXT,
      description,
      createdAt: '2026-11-01T11:00:00Z',
    }
    const f = fixture({ prs: [sample] })
    const base = f.deps.run
    f.deps.run = (cmd, args) =>
      args[1]?.includes('/statuses')
        ? ok([
            {
              context: VERIFY_CONTEXT,
              description: VERIFY_DESCRIPTIONS.verified,
              created_at: '2026-11-01T10:30:00Z',
            },
          ])
        : base(cmd, args)
    const report = collect({}, f.deps)
    assert.deepEqual(report.sections.mergesWithoutVerdict.rows, [])
    assert.deepEqual(report.sections.unjudgedHeads.rows, [])
    assert.equal(report.sections.verifyCronHealth.verifySilent, false)
  }
})

test('unreadable or truncated verdict history stays unknown without hiding other sections', () => {
  for (const response of [
    { status: 1, stderr: 'rate limit' },
    ok(Array.from({ length: 100 }, () => ({ context: 'other' }))),
  ]) {
    const sample = pr(1)
    sample.mergedAt = '2026-11-01T11:30:00Z'
    sample.commits.nodes[0].commit.statusCheckRollup.contexts.nodes.pop()
    const f = fixture({ prs: [sample] })
    const base = f.deps.run
    f.deps.run = (cmd, args) => (args[1]?.includes('/statuses') ? response : base(cmd, args))
    const report = collect({}, f.deps)
    assert.equal(report.sections.unjudgedHeads.status, 'failed')
    assert.equal(report.sections.mergesWithoutVerdict.status, 'failed')
    assert.equal(report.sections.verifyCronHealth.verifySilent, null)
    assert.equal(report.sections.secondModelHealth.status, 'ok')
    assert.equal(report.sections.unnecessaryHolds.status, 'ok')
  }
})

test('failed unjudged history does not fail an independent merge section', () => {
  const sample = pr(1)
  sample.commits.nodes[0].commit.statusCheckRollup.contexts.nodes.pop()
  const f = fixture({ prs: [sample] })
  const base = f.deps.run
  f.deps.run = (cmd, args) =>
    args[1]?.includes('/statuses') ? { status: 1, stderr: 'rate limit' } : base(cmd, args)
  const report = collect({}, f.deps)
  assert.equal(report.sections.unjudgedHeads.status, 'failed')
  assert.equal(report.sections.mergesWithoutVerdict.status, 'ok')
  assert.deepEqual(report.sections.mergesWithoutVerdict.rows, [])
})

test('verdict history reads share a bounded cache and disclose exhaustion', () => {
  const prs = Array.from({ length: 51 }, (_, i) => {
    const sample = pr(i)
    sample.commits.nodes[0].commit.statusCheckRollup.contexts.nodes.pop()
    return sample
  })
  const f = fixture({ prs })
  const report = collect({}, f.deps)
  assert.equal(f.calls.filter((c) => c[2]?.includes('/statuses')).length, 50)
  assert.equal(report.truncated, true)
  assert.equal(report.sections.unjudgedHeads.reason, 'status-history-limit')
  assert.equal(report.sections.verifyCronHealth.verifySilent, null)
})

test('an unrelated update does not reread or recount outcomes before the report window', () => {
  const sample = pr(1)
  sample.mergedAt = '2026-11-01T11:10:00Z'
  const f = fixture({ prs: [sample], lastRunAt: '2026-11-01T11:30:00Z' })
  const report = collect({}, f.deps)
  assert.deepEqual(report.sections.mergesWithoutVerdict.rows, [])
  assert.deepEqual(report.sections.unjudgedHeads.rows, [])
  assert.deepEqual(report.sections.unnecessaryHolds.rows, [])
  assert.equal(f.calls.filter((c) => c[2]?.includes('/statuses')).length, 0)
})

const escape = (original = 1, fix = 90) => {
  const record = escapeRecord({
    repo: 'owner/repo',
    fix: {
      number: fix,
      title: 'Fix',
      mergedAt: '2026-10-31T00:00:00Z',
      files: ['scripts/check.mjs'],
    },
    link: {
      original: {
        ...pr(original),
        title: 'Original',
        mergedAt: '2026-10-20T00:00:00Z',
        files: ['scripts/check.mjs'],
      },
      basis: 'explicit-reference',
      evidence: `Fixes #${original}`,
    },
    tracedAt: now().toISOString(),
  })
  const parsed = parseEscapeComment(renderEscapeComment(record))
  assert.ok(parsed)
  return parsed
}

function traced(f, trace, list = ok({ escapes: [], unreadable: 0 })) {
  const base = f.deps.run
  f.deps.run = (cmd, args, options) => {
    if (cmd !== process.execPath || !args[0].endsWith('verify-escapes-trace.mjs'))
      return base(cmd, args, options)
    f.calls.push([cmd, ...args])
    f.timeouts ??= []
    f.timeouts.push(options.timeout)
    return args[1] === 'trace' ? trace : list
  }
  return f
}
test('dry-run reaches tracer and unpublished trace links are unioned with durable records', () => {
  for (const dryRun of [true, false]) {
    const f = traced(
      fixture(),
      ok({ links: [escape(), escape(2)], skipped: ['followup'], truncated: true }),
      ok({ escapes: [escape()], unreadable: 0 }),
    )
    assert.equal(main(['report', '--out', '/out', ...(dryRun ? ['--dry-run'] : [])], f.deps), 0)
    const report = JSON.parse(f.writes.get('/out'))
    assert.equal(report.sections.escapeRows.rows.length, 2)
    assert.deepEqual(report.sections.escapeRows.skipped, ['followup'])
    assert.equal(report.truncated, true)
    const trace = f.calls.find((c) => c[2] === 'trace')
    assert.equal(trace.includes('--dry-run'), dryRun)
    assert.deepEqual(f.timeouts, [TRACE_TIMEOUT_MS, LIST_TIMEOUT_MS])
    assert.equal(trace[trace.indexOf('--since') + 1], report.window.start)
    const list = f.calls.find((c) => c[2] === 'list')
    assert.equal(list[list.indexOf('--since') + 1], '2026-10-04T12:00:00.000Z')
    for (const c of f.calls)
      assert.ok(!c.some((a) => /^(POST|PUT|PATCH|DELETE|comment|edit)$/.test(a)))
  }
})
test('tracer failures fail only the added sections while the CLI still writes successfully', () => {
  for (const [response, reason, detail] of [
    [
      {
        status: 1,
        stdout: JSON.stringify({ failures: [{ reason: 'rate-limited', operation: 'search' }] }),
      },
      'tracer-failed',
      'rate-limited',
    ],
    [{ status: null, error: { code: 'ETIMEDOUT' } }, 'timeout'],
    [{ status: null, signal: 'SIGTERM' }, 'timeout'],
    [{ status: null, error: { code: 'ENOENT' } }, 'gh-unavailable'],
    [ok('garbage'), 'tracer-failed'],
    [{ status: 1, stderr: 'first line\nsecond line' }, 'tracer-failed', 'first line'],
  ]) {
    const f = traced(fixture(), response)
    assert.equal(main(['report', '--out', '/out'], f.deps), 0)
    const report = JSON.parse(f.writes.get('/out'))
    for (const name of ['escapeRows', 'laterFixRates']) {
      assert.equal(report.sections[name].status, 'failed')
      assert.equal(report.sections[name].reason, reason)
      if (detail) assert.equal(report.sections[name].detail, detail)
    }
    for (const name of [...sections, 'verifyCronHealth'])
      assert.equal(report.sections[name].status, 'ok')
  }
  const f = fixture({ tracer: false })
  assert.equal(main(['report', '--out', '/out'], f.deps), 0)
  const report = JSON.parse(f.writes.get('/out'))
  for (const name of ['escapeRows', 'laterFixRates'])
    assert.equal(report.sections[name].reason, 'tracer-missing')
})
test('rate read slices the 28-day window, de-duplicates boundaries and uses factory merge heads', () => {
  const sample = { ...pr(1), mergedAt: '2026-10-20T00:00:00Z' }
  const foreign = { ...pr(2), mergedAt: sample.mergedAt, commits: { nodes: [] } }
  const f = fixture({ prs: [sample, foreign] })
  const report = collect({}, f.deps)
  const calls = f.calls.filter((c) => c.some((a) => a.includes('is:merged merged:')))
  assert.equal(calls.length, 4)
  const searches = calls.map((c) => c.find((a) => a.startsWith('search=')))
  assert.ok(searches[0].includes('2026-10-04T12:00:00.000Z..2026-10-11T12:00:00.000Z'))
  assert.ok(searches[3].includes('2026-10-25T12:00:00.000Z..2026-11-01T12:00:00.000Z'))
  assert.equal(report.sections.laterFixRates.rows.find((r) => r.class === 'approved').merges, 1)
})
test('rate cap sets truncation and rate read failure leaves escape rows readable', () => {
  const f = fixture()
  const base = f.deps.run
  let count = 0
  f.deps.run = (cmd, args, opts) => {
    if (!args.some((a) => a.includes('is:merged merged:'))) return base(cmd, args, opts)
    return ok({
      data: {
        search: {
          nodes: Array.from({ length: 20 }, () => ({
            ...pr(++count),
            mergedAt: '2026-10-20T00:00:00Z',
          })),
          pageInfo: { hasNextPage: true, endCursor: String(count) },
        },
      },
    })
  }
  const report = collect({}, f.deps)
  assert.equal(report.truncated, true)
  assert.equal(
    report.sections.laterFixRates.rows.reduce((n, r) => n + r.merges, 0),
    MAX_RATE_PRS,
  )
  const broken = fixture()
  const originalRun = broken.deps.run
  broken.deps.run = (cmd, args, opts) =>
    args.some((a) => a.includes('is:merged merged:'))
      ? { status: 1, stderr: 'rate limit' }
      : originalRun(cmd, args, opts)
  const failed = collect({}, broken.deps)
  assert.equal(failed.sections.laterFixRates.reason, 'rate-limited')
  assert.equal(failed.sections.escapeRows.status, 'ok')
})
test('list uses an earlier report start and unreadable published records do not become zero', () => {
  const f = traced(fixture(), ok({ links: [] }), ok({ escapes: [], unreadable: 1 }))
  const report = collect({ since: '2026-10-01T12:00:00Z' }, f.deps)
  const list = f.calls.find((c) => c[2] === 'list')
  assert.equal(list[list.indexOf('--since') + 1], report.window.start)
  assert.equal(report.sections.escapeRows.reason, 'tracer-failed')
  assert.equal(report.sections.laterFixRates.reason, 'tracer-failed')
})

test('search ceilings remain visible when GitHub stops pagination at its result limit', () => {
  const f = fixture()
  const base = f.deps.run
  const samples = Array.from({ length: 1000 }, (_, i) => ({
    ...pr(i + 1),
    mergedAt: '2026-10-20T00:00:00Z',
  }))
  f.deps.run = (cmd, args, opts) =>
    args.some((a) => a.includes('is:merged merged:'))
      ? ok({
          data: { search: { issueCount: 1001, nodes: samples, pageInfo: { hasNextPage: false } } },
        })
      : base(cmd, args, opts)
  assert.equal(collect({}, f.deps).truncated, true)
})

const capped = (key, cappedAt, cleared = {}) => ({
  key,
  label: key,
  cappedAt,
  escapes: [{ original: 11, fix: 21, fixMergedAt: cappedAt }],
  ...cleared,
})
test('area caps read the history verb once, write nothing and keep overlapping episodes', () => {
  const episodes = [
    capped('ui', '2026-10-20T00:00:00Z', {
      clearedAt: '2026-10-30T00:00:00Z',
      clearedBy: 'operator',
      reason: 'guard repaired',
    }),
    capped('api', '2026-10-01T00:00:00Z'),
    capped('old', '2026-10-01T00:00:00Z', {
      clearedAt: '2026-10-10T00:00:00Z',
      clearedBy: 'operator',
      reason: 'stale',
    }),
  ]
  const f = fixture({ history: ok({ repo: 'owner/repo', episodes, unreadable: 0 }) })
  const writes = []
  f.deps.fs = { ...f.deps.fs, writeFileSync: (...args) => writes.push(args) }
  const timeouts = []
  const base = f.deps.run
  f.deps.run = (cmd, args, options) => {
    if (args[0]?.endsWith?.('verify-area-caps-sync.mjs')) timeouts.push(options.timeout)
    return base(cmd, args, options)
  }
  const report = collect({}, f.deps)
  assert.equal(writes.length, 0)
  const spawns = f.calls.filter(
    (c) => c[0] === process.execPath && c[1].endsWith('verify-area-caps-sync.mjs'),
  )
  assert.deepEqual(
    spawns.map((c) => c.slice(2)),
    [['history', '--repo', 'owner/repo']],
  )
  assert.deepEqual(timeouts, [CAPS_TIMEOUT_MS])
  const { rows, active } = report.sections.areaCaps
  assert.deepEqual(
    rows.map((row) => [row.key, row.state]),
    [
      ['api', 'active'],
      ['ui', 'cleared'],
    ],
  )
  assert.equal(rows[1].clearedBy, 'operator')
  assert.equal(rows[1].reason, 'guard repaired')
  assert.equal(active, 1)
})
test('area-cap history failures fail only that section while the CLI still writes', () => {
  for (const [options, reason, detail] of [
    [{ caps: false }, 'caps-missing'],
    [{ history: { status: 1, stderr: 'gh failed\nmore' } }, 'caps-failed', 'gh failed'],
    [{ history: ok('garbage') }, 'caps-failed'],
    [{ history: ok({ repo: 'owner/repo' }) }, 'caps-failed', 'malformed cap history'],
    [
      { history: ok({ repo: 'owner/repo', episodes: [], unreadable: 2 }) },
      'caps-failed',
      '2 unreadable cap records',
    ],
    [{ history: { status: null, error: { code: 'ETIMEDOUT' } } }, 'timeout'],
  ]) {
    const f = fixture(options)
    assert.equal(main(['report', '--out', '/out'], f.deps), 0)
    const report = JSON.parse(f.writes.get('/out'))
    assert.equal(report.sections.areaCaps.status, 'failed')
    assert.equal(report.sections.areaCaps.reason, reason)
    if (detail) assert.equal(report.sections.areaCaps.detail, detail)
    assert.ok(
      report.sections.areaCaps.detail === undefined ||
        report.sections.areaCaps.detail.length <= 200,
    )
    for (const name of [...sections, 'verifyCronHealth', 'escapeRows', 'laterFixRates'])
      assert.equal(report.sections[name].status, 'ok')
  }
  const noRepo = collect({}, fixture({ failure: 'API rate limit exceeded' }).deps)
  assert.equal(noRepo.sections.areaCaps.reason, 'rate-limited')
})
