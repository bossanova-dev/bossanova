import test from 'node:test'
import assert from 'node:assert/strict'
import { collect, main } from './retro-outcomes-report.mjs'
import { RECEIPT_CONTEXT } from '../commit-status.mjs'
import { VERIFY_CONTEXT, VERIFY_DESCRIPTIONS } from '../verify/verify-vocabulary.mjs'

const now = () => new Date('2026-11-01T12:00:00Z')
const ok = (value) => ({
  status: 0,
  stdout: typeof value === 'string' ? value : JSON.stringify(value),
})
function fixture({ prs = [], failure = '', repoId = 'local', lastRunAt, more = false } = {}) {
  const calls = [],
    writes = new Map()
  const run = (cmd, args) => {
    calls.push([cmd, ...args])
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
  assert.equal(paged.calls.filter((c) => c[2] === 'graphql').length, 10)
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
  const report = collect({}, f.deps)
  assert.equal(report.sections.unnecessaryHolds.status, 'failed')
  assert.equal(report.sections.mergesWithoutVerdict.status, 'ok')
  for (const call of f.calls) {
    assert.ok(!call.some((s) => /^(POST|PUT|PATCH|DELETE)$/.test(s)))
    assert.ok(['git', 'gh', '/boss'].includes(call[0]))
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
