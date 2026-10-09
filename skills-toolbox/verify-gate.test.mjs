import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import {
  CLAIM_STALE_AFTER_MS,
  HUMAN_REASONS,
  REPAIR_EXHAUSTED_AFTER,
  UsageError,
  VERIFY_DESCRIPTIONS,
  aggregateExtensions,
  approval,
  candidates,
  claimState,
  classifyApproval,
  judge,
  main,
  merge,
  parseLinearIssueLine,
  post,
  rearm,
  rearmArgv,
  verifyMarker,
} from './verify-gate.mjs'
import { DEFAULT_CONFIG, mergeConfig, validateConfig } from './skill-config.mjs'

const HEAD = 'a'.repeat(40)
const OTHER = 'd'.repeat(40)
const BASE = 'b'.repeat(40)
const MERGED_SHA = 'c'.repeat(40)
const NOW = Date.parse('2026-10-06T12:00:00Z')
const iso = (msAgo) => new Date(NOW - msAgo).toISOString()
const REPO = 'o/r'
const PR_URL = 'https://github.com/o/r/pull/7'

const BODY = [
  'Linear issue: https://linear.app/acme/issue/BOS-1/some-title',
  '',
  '## Acceptance criteria',
  '',
  '- [x] it works',
  '',
  '## Human follow-up',
  '',
  '- none',
  '',
  '## Open questions',
  '',
  '- none',
  '',
  '## Review coverage',
  '',
  'full (3 lenses)',
  '',
].join('\n')

const green = [{ name: 'ci', status: 'COMPLETED', conclusion: 'SUCCESS' }]

function prView(overrides = {}) {
  return {
    state: 'OPEN',
    number: 7,
    url: PR_URL,
    title: 'feat: a thing',
    body: BODY,
    isDraft: false,
    headRefOid: HEAD,
    baseRefName: 'main',
    mergeStateStatus: 'CLEAN',
    statusCheckRollup: green,
    reviewDecision: '',
    mergeCommit: null,
    ...overrides,
  }
}

const receipt = () => ({ context: 'boss/build', state: 'success', updated_at: iso(60_000) })

// A scripted world behind one fake reader. Every call is recorded; statuses posted through
// `gh api --method POST …/statuses/<sha>` are prepended to that SHA's list, so read-backs are real.
function world(opts = {}) {
  // `null` is a meaningful value here (an unreadable source), so only an ABSENT key takes the default.
  const pick = (key, fallback) => (key in opts ? opts[key] : fallback)
  const w = {
    pr: prView(opts.pr),
    prAfterMerge: opts.prAfterMerge ?? null,
    statuses: { [HEAD]: opts.statuses ?? [receipt()] },
    statusReadFails: opts.statusReadFails ?? false,
    base: pick('base', {
      runs: [
        {
          total_count: 1,
          check_runs: [{ name: 'ci', status: 'completed', conclusion: 'success' }],
        },
      ],
      statuses: [],
    }),
    diff: pick('diff', ['src/a.go']),
    sessions: opts.sessions ?? [{ id: 'sess-1', pr_url: PR_URL }],
    show: pick('show', { repo_can_auto_repair: true, last_repair: null }),
    merge: opts.merge ?? { ok: true },
    race: opts.race ?? null,
    calls: [],
  }
  w.read = (command, args, { json } = {}) => {
    w.calls.push({ command, args: [...args] })
    const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
    const fail = (error = 'exit-1') => ({ ok: false, error, exit: 1, output: '' })
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'view') {
      if (w.pr === null) return fail()
      const merged = w.calls.some((c) => c.command === 'boss' && c.args[0] === 'merge')
      return ok(merged && w.prAfterMerge ? w.prAfterMerge : w.pr)
    }
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'diff') {
      if (w.diff === null) return fail()
      return { ok: true, output: w.diff.join('\n'), exit: 0 }
    }
    if (command === 'gh' && args[0] === 'repo') return ok({ nameWithOwner: REPO })
    if (command === 'gh' && args[0] === 'api') {
      if (args[1] === '--method' && args[2] === 'POST') {
        const path = args[3]
        const fields = Object.fromEntries(
          args
            .slice(4)
            .filter((_, i) => i % 2 === 1)
            .map((kv) => kv.match(/^([^=]*)=(.*)$/s).slice(1)),
        )
        const statusMatch = path.match(/\/statuses\/([0-9a-f]{40})$/)
        if (statusMatch) {
          const list = (w.statuses[statusMatch[1]] ??= [])
          list.unshift({
            context: fields.context,
            state: fields.state,
            description: fields.description ?? '',
            target_url: fields.target_url ?? '',
            updated_at: iso(0),
          })
          if (w.race)
            list.unshift({
              context: 'boss/verify',
              state: 'pending',
              description: `${VERIFY_DESCRIPTIONS.verifying} ${w.race}`,
              updated_at: new Date(NOW + 1000).toISOString(),
            })
          return ok({})
        }
        if (/\/issues\/\d+\/comments$/.test(path))
          return ok({ html_url: `${PR_URL}#issuecomment-99` })
        return fail()
      }
      const path = args[1]
      const statusList = path.match(/commits\/([0-9a-f]{40})\/statuses/)
      if (statusList) {
        if (w.statusReadFails) return fail()
        return { ok: true, output: JSON.stringify([w.statuses[statusList[1]] ?? []]), exit: 0 }
      }
      if (path === 'repos/o/r/commits/main') {
        if (w.base === null) return fail()
        return ok({ sha: BASE })
      }
      if (path.includes(`commits/${BASE}/check-runs`)) return ok(w.base.runs)
      if (path.includes(`commits/${BASE}/status`)) return ok({ statuses: w.base.statuses })
      return fail()
    }
    if (command === 'boss' && args[0] === 'ls') return ok({ sessions: w.sessions })
    if (command === 'boss' && args[0] === 'show') {
      if (w.show === null) return fail()
      return ok({ session: w.show })
    }
    if (command === 'boss' && args[0] === 'merge')
      return w.merge.ok ? ok({ merged: true }) : { ...fail(w.merge.error), payload: {} }
    if (command === 'boss' && args[0] === 'callback') return ok({ id: 'cb-1' })
    if (command === 'boss' && args[0] === 'notes') return ok({ id: 'note-1' })
    return fail('unexpected')
  }
  w.mutating = () =>
    w.calls.filter(
      ({ command, args }) =>
        (command === 'gh' && args[0] === 'api' && args[1] === '--method') ||
        (command === 'gh' && args[0] === 'pr' && ['comment', 'merge'].includes(args[1])) ||
        (command === 'boss' && ['merge', 'callback', 'notes'].includes(args[0])),
    )
  w.statusPosts = () =>
    w
      .mutating()
      .filter((c) => c.command === 'gh' && /\/statuses\//.test(c.args[3]))
      .map((c) => {
        const get = (k) => c.args.find((a) => a.startsWith(`${k}=`))?.slice(k.length + 1)
        return { state: get('state'), description: get('description'), target: get('target_url') }
      })
  w.notes = () => w.calls.filter((c) => c.command === 'boss' && c.args[0] === 'notes')
  return w
}

function fakeAdapter({ labels = ['needs-human'], outcome = 'ok', missingKey = false } = {}) {
  const calls = []
  return {
    calls,
    writeCalls: () =>
      calls.filter((w) =>
        ['addLabels', 'removeLabels', 'comment', 'stateName'].some((k) => k in w),
      ),
    applyIssueWrites: async (writes) => {
      calls.push(writes)
      if (missingKey)
        throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
          code: 'TRACKER_CREDENTIALS_MISSING',
        })
      const isWrite = ['addLabels', 'removeLabels', 'comment', 'stateName'].some((k) => k in writes)
      return {
        ok: !isWrite || outcome === 'ok',
        outcome: isWrite ? outcome : 'ok',
        applied: [],
        issue: { id: 'uuid', identifier: 'BOS-1', labels },
      }
    },
  }
}

function deps(w, extra = {}) {
  return {
    read: w.read,
    now: () => NOW,
    randomToken: () => 'feedf00d',
    discover: () => ({ extensions: [], skipped: [] }),
    loadConfig: () => ({}),
    adapter: fakeAdapter(),
    exists: () => false,
    ...extra,
  }
}

const tmp = () => mkdtempSync(join(tmpdir(), 'verify-gate-'))
function jsonFile(dir, name, value) {
  const path = join(dir, name)
  writeFileSync(path, JSON.stringify(value))
  return path
}

// ---------------------------------------------------------------------------
// Pure helpers.

test('VERIFY_DESCRIPTIONS is frozen and carries the BOS-1382 prefixes', () => {
  assert.ok(Object.isFrozen(VERIFY_DESCRIPTIONS))
  assert.equal(VERIFY_DESCRIPTIONS.verifying, 'verifying…')
  assert.equal(VERIFY_DESCRIPTIONS.waiting, 'waiting:')
  assert.equal(VERIFY_DESCRIPTIONS.needsHuman, 'needs human:')
  assert.equal(VERIFY_DESCRIPTIONS.verified, 'verified')
  assert.equal(VERIFY_DESCRIPTIONS.verifiedApproved, 'verified (approved)')
  assert.equal(VERIFY_DESCRIPTIONS.defect, 'defect:')
  assert.equal(REPAIR_EXHAUSTED_AFTER, 5)
  assert.deepEqual(HUMAN_REASONS, [
    'no-receipt',
    'repair-exhausted',
    'always-human-path',
    'ledger-open',
    'extension-failed',
    'no-session',
  ])
})

test('parseLinearIssueLine reads only the first non-blank line', () => {
  assert.deepEqual(parseLinearIssueLine(BODY), {
    url: 'https://linear.app/acme/issue/BOS-1/some-title',
    id: 'BOS-1',
  })
  assert.deepEqual(parseLinearIssueLine('\n\nLinear issue: https://linear.app/a/issue/ab-12'), {
    url: 'https://linear.app/a/issue/ab-12',
    id: 'AB-12',
  })
  assert.equal(parseLinearIssueLine('Summary\nLinear issue: https://linear.app/a/issue/A-1'), null)
  assert.equal(parseLinearIssueLine('Linear issue: none'), null)
  assert.equal(parseLinearIssueLine(undefined), null)
})

test('claimState tells mine, a fresh foreign claim, a stale one, waiting and terminal apart', () => {
  const claim = (token, msAgo) => ({
    state: 'pending',
    description: `verifying… ${token}`,
    updatedAt: iso(msAgo),
  })
  assert.equal(claimState(null).kind, 'none')
  assert.equal(claimState(claim('t1', 1000), { token: 't1', now: NOW }).kind, 'mine')
  assert.equal(claimState(claim('t2', 1000), { token: 't1', now: NOW }).kind, 'foreign')
  assert.equal(
    claimState(claim('t2', CLAIM_STALE_AFTER_MS + 1), { token: 't1', now: NOW }).kind,
    'stale',
  )
  assert.equal(claimState({ ...claim('t2', 0), updatedAt: 'garbage' }, { now: NOW }).kind, 'stale')
  assert.equal(claimState({ state: 'pending', description: 'waiting: ci' }).kind, 'waiting')
  assert.equal(claimState({ state: 'success', description: 'verified' }).kind, 'terminal')
})

// ---------------------------------------------------------------------------
// judge.

test('judge passes a green, receipted, clean-ledger head and writes nothing', async () => {
  const w = world()
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'pass')
  assert.equal(r.reason, 'verified')
  assert.equal(r.headSha, HEAD)
  assert.equal(r.baseSha, BASE)
  assert.deepEqual(r.ticket, { url: 'https://linear.app/acme/issue/BOS-1/some-title', id: 'BOS-1' })
  assert.equal(w.mutating().length, 0)
  assert.equal(
    w.calls.filter((c) => c.args[0] === 'pr' && c.args[1] === 'diff').length,
    0,
    'the default empty alwaysHumanPaths reads no diff',
  )
})

test('judge excludes boss/* contexts from the head CI judgment', async () => {
  const w = world({
    pr: {
      statusCheckRollup: [
        ...green,
        { context: 'boss/build', state: 'FAILURE' },
        { context: 'boss/verify', state: 'PENDING' },
      ],
    },
  })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'pass')
  assert.deepEqual(r.facts.ci.excluded, ['boss/build', 'boss/verify'])
})

test('judge waits, never defects, on unsettled or red head CI, a draft and a closed PR', async () => {
  for (const [pr, reason] of [
    [{ statusCheckRollup: [{ name: 'ci', status: 'IN_PROGRESS' }] }, 'ci-not-settled'],
    [
      { statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'FAILURE' }] },
      'ci-not-green-on-head',
    ],
    [{ isDraft: true }, 'pr-draft'],
    [{ state: 'CLOSED' }, 'pr-not-open'],
    [{ mergeStateStatus: 'DIRTY' }, 'merge-state-dirty'],
    [{ mergeStateStatus: 'UNKNOWN' }, 'merge-state-unknown'],
  ]) {
    const w = world({ pr })
    const r = await judge({ pr: 7, repo: REPO }, deps(w))
    assert.equal(r.verdict, 'wait', reason)
    assert.equal(r.reason, reason)
    assert.equal(w.mutating().length, 0)
  }
})

test('judge base-red guard: red waits, unreadable waits with a note, pending and green proceed', async () => {
  const red = world({
    base: {
      runs: [{ check_runs: [{ name: 'ci', status: 'completed', conclusion: 'failure' }] }],
      statuses: [{ context: 'boss/build', state: 'success' }],
    },
  })
  const r1 = await judge({ pr: 7, repo: REPO }, deps(red))
  assert.equal(r1.verdict, 'wait')
  assert.equal(r1.reason, 'base-ci-red')

  const unreadable = world({ base: null })
  const r2 = await judge({ pr: 7, repo: REPO }, deps(unreadable))
  assert.equal(r2.reason, 'base-ci-unknown')
  assert.deepEqual(
    r2.notes.map((n) => n.trigger),
    ['evidence-unknown'],
  )

  const pending = world({
    base: { runs: [{ check_runs: [{ name: 'ci', status: 'in_progress' }] }], statuses: [] },
  })
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(pending))).verdict, 'pass')

  const bossOnlyRed = world({
    base: {
      runs: [{ check_runs: [{ name: 'ci', status: 'completed', conclusion: 'success' }] }],
      statuses: [{ context: 'boss/verify', state: 'failure' }],
    },
  })
  assert.equal(
    (await judge({ pr: 7, repo: REPO }, deps(bossOnlyRed))).verdict,
    'pass',
    'a red boss/* status on the base is not base CI',
  )
})

test('judge routes a missing receipt to a human and an unreadable one to a wait', async () => {
  const none = world({ statuses: [] })
  const r1 = await judge({ pr: 7, repo: REPO }, deps(none))
  assert.equal(r1.verdict, 'human')
  assert.equal(r1.reason, 'no-receipt')
  const failed = world({ statuses: [{ ...receipt(), state: 'failure' }] })
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(failed))).reason, 'no-receipt')
  const unknown = world({ statusReadFails: true })
  const r3 = await judge({ pr: 7, repo: REPO }, deps(unknown))
  assert.equal(r3.verdict, 'wait')
  assert.equal(r3.reason, 'receipt-unknown')
})

test('judge routes an open ledger or a do-not-merge marker to a human ledger-open', async () => {
  for (const body of [
    BODY.replace('- [x] it works', '- [ ] it works'),
    BODY.replace('## Human follow-up\n\n- none', '## Human follow-up\n\n- [ ] check prod'),
    BODY.replace('## Open questions\n\n- none\n', ''),
    `${BODY}\nDO NOT MERGE\n`,
  ]) {
    const w = world({ pr: { body } })
    const r = await judge({ pr: 7, repo: REPO }, deps(w))
    assert.equal(r.verdict, 'human')
    assert.equal(r.reason, 'ledger-open')
  }
})

test('judge alwaysHumanPaths: an invalid config waits instead of dropping the policy', async () => {
  const w = world({ diff: ['migrations/001.sql'] })
  const r = await judge(
    { pr: 7, repo: REPO },
    deps(w, {
      loadConfig: () => {
        throw new Error(
          'verifyDefaults.alwaysHumanPaths must be an array of non-empty glob strings',
        )
      },
    }),
  )
  assert.equal(r.verdict, 'wait')
  assert.equal(r.reason, 'config-unreadable')
  assert.deepEqual(
    r.notes.map((n) => n.trigger),
    ['evidence-unknown'],
  )
})

test('judge alwaysHumanPaths: a hit is human, an unreadable diff waits with a note', async () => {
  const config = { verifyDefaults: { alwaysHumanPaths: ['migrations/**'] } }
  const hit = world({ diff: ['src/a.go', 'migrations/001.sql'] })
  const r1 = await judge({ pr: 7, repo: REPO }, deps(hit, { loadConfig: () => config }))
  assert.equal(r1.verdict, 'human')
  assert.equal(r1.reason, 'always-human-path')
  assert.deepEqual(r1.facts.alwaysHumanHits, ['migrations/001.sql'])

  const miss = world({ diff: ['src/a.go'] })
  assert.equal(
    (await judge({ pr: 7, repo: REPO }, deps(miss, { loadConfig: () => config }))).verdict,
    'pass',
  )

  const unreadable = world({ diff: null })
  const r3 = await judge({ pr: 7, repo: REPO }, deps(unreadable, { loadConfig: () => config }))
  assert.equal(r3.verdict, 'wait')
  assert.equal(r3.reason, 'changed-paths-unknown')
  assert.deepEqual(
    r3.notes.map((n) => n.trigger),
    ['evidence-unknown'],
  )
})

const EXT = { name: 'boss-verify-proof', order: 10 }
const envelope = (fields) => ({ ok: true, extension: EXT.name, role: 'verify', ...fields })
const passResult = envelope({
  verdict: 'pass',
  evidence: [{ kind: 'test', ref: 'go test' }],
  findings: [],
})
const failResult = envelope({ verdict: 'fail', evidence: [], findings: [{ title: 'nil deref' }] })

test('judge asks for extensions only when built-ins pass and extensions are installed', async () => {
  const w = world()
  const r = await judge({ pr: 7, repo: REPO }, deps(w, { discover: () => ({ extensions: [EXT] }) }))
  assert.equal(r.verdict, 'extensions-required')
  assert.deepEqual(r.extensionEnvelope.changedPaths, ['src/a.go'])
  assert.equal(r.extensionEnvelope.headSha, HEAD)
  assert.equal(r.extensionEnvelope.baseSha, BASE)
  assert.deepEqual(r.extensionEnvelope.ticket, {
    id: 'BOS-1',
    url: 'https://linear.app/acme/issue/BOS-1/some-title',
  })
  assert.equal(w.mutating().length, 0, 'judge never claims; the caller does')

  const waiting = world({ pr: { isDraft: true } })
  const r2 = await judge(
    { pr: 7, repo: REPO },
    deps(waiting, { discover: () => ({ extensions: [EXT] }) }),
  )
  assert.equal(r2.verdict, 'wait', 'a head that waits never reaches the extension step')
  assert.equal(r2.extensionEnvelope, undefined)
})

test('judge maps every extension malfunction to human extension-failed with a note', async () => {
  const dir = tmp()
  try {
    for (const [label, entry] of [
      ['invalid result', { extension: EXT.name, result: { ok: true, verdict: 'maybe' } }],
      ['crash', { extension: EXT.name, crashed: true }],
      [
        'pass with empty evidence',
        { extension: EXT.name, result: envelope({ verdict: 'pass', evidence: [], findings: [] }) },
      ],
      ['non-optional timeout', { extension: EXT.name, timedOut: true }],
      ['missing result', null],
    ]) {
      const file = jsonFile(dir, 'r.json', entry ? [entry] : [])
      const w = world()
      const r = await judge(
        { pr: 7, repo: REPO, extensionResults: file },
        deps(w, { discover: () => ({ extensions: [EXT] }) }),
      )
      assert.equal(r.verdict, 'human', label)
      assert.equal(r.reason, 'extension-failed', label)
      assert.deepEqual(
        r.notes.map((n) => n.trigger),
        ['extension-failed'],
        label,
      )
    }
    const garbage = join(dir, 'garbage.json')
    writeFileSync(garbage, 'not json')
    const r = await judge(
      { pr: 7, repo: REPO, extensionResults: garbage },
      deps(world(), { discover: () => ({ extensions: [EXT] }) }),
    )
    assert.equal(r.reason, 'extension-failed')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('judge: optional timeout abstains, a valid fail is a defect, clean review records a note', async () => {
  const dir = tmp()
  try {
    const optional = { ...EXT, name: 'opt', optional: true }
    const discover = () => ({ extensions: [EXT, optional] })
    const ok = jsonFile(dir, 'ok.json', [
      { extension: EXT.name, result: passResult },
      { extension: 'opt', optional: true, timedOut: true },
    ])
    const r1 = await judge({ pr: 7, repo: REPO, extensionResults: ok }, deps(world(), { discover }))
    assert.equal(r1.verdict, 'pass')

    const bad = jsonFile(dir, 'bad.json', [
      { extension: EXT.name, result: failResult },
      {
        extension: 'opt',
        optional: true,
        result: envelope({ verdict: 'abstain', evidence: [], findings: [] }),
      },
    ])
    const r2 = await judge(
      { pr: 7, repo: REPO, extensionResults: bad },
      deps(world(), { discover }),
    )
    assert.equal(r2.verdict, 'defect')
    assert.deepEqual(r2.findings, [{ extension: EXT.name, title: 'nil deref' }])
    assert.deepEqual(
      r2.notes.map((n) => n.trigger),
      ['verify-failed-after-clean-review'],
    )

    const reviewed = world({ pr: { body: `${BODY}\n## Review findings\n\n- one\n` } })
    const r3 = await judge(
      { pr: 7, repo: REPO, extensionResults: bad },
      deps(reviewed, { discover }),
    )
    assert.equal(r3.verdict, 'defect')
    assert.deepEqual(r3.notes, [], 'a review that already had findings is not a clean review')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('aggregateExtensions treats an unreadable results list as a malfunction', () => {
  assert.equal(aggregateExtensions(null).reason, 'extension-failed')
  assert.equal(aggregateExtensions([], []).verdict, 'pass')
})

test('judge --waive lifts only that code and still refuses on red CI', async () => {
  const none = world({ statuses: [] })
  const r1 = await judge({ pr: 7, repo: REPO, waive: 'no-receipt' }, deps(none))
  assert.equal(r1.verdict, 'pass')
  assert.equal(r1.reason, 'approved')

  const two = world({
    statuses: [],
    pr: { body: BODY.replace('- [x] it works', '- [ ] it works') },
  })
  const r2 = await judge({ pr: 7, repo: REPO, waive: 'no-receipt' }, deps(two))
  assert.equal(r2.verdict, 'human')
  assert.equal(r2.reason, 'ledger-open', 'only the waived code is lifted')

  const red = world({
    statuses: [],
    pr: { statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'FAILURE' }] },
  })
  const r3 = await judge({ pr: 7, repo: REPO, waive: 'no-receipt' }, deps(red))
  assert.equal(r3.verdict, 'wait')
  assert.equal(r3.reason, 'ci-not-green-on-head')

  await assert.rejects(
    judge({ pr: 7, repo: REPO, waive: 'ci-not-settled' }, deps(world())),
    UsageError,
  )
})

// ---------------------------------------------------------------------------
// post: claims.

test('post claim posts a verifying status, reads it back and wins', async () => {
  const w = world()
  const r = await post({ pr: 7, head: HEAD, verdict: 'claim', repo: REPO }, deps(w))
  assert.equal(r.won, true)
  assert.equal(r.token, 'feedf00d')
  assert.deepEqual(w.statusPosts(), [
    { state: 'pending', description: 'verifying… feedf00d', target: undefined },
  ])
  assert.equal(w.notes().length, 0)
})

test('post claim loses to a later token, and a lost claim abandons the terminal post', async () => {
  const w = world({ race: 'cafe0001' })
  const r = await post({ pr: 7, head: HEAD, verdict: 'claim', repo: REPO }, deps(w))
  assert.equal(r.won, false)

  const before = w.statusPosts().length
  const p = await post(
    { pr: 7, head: HEAD, verdict: 'pass', token: 'feedf00d', repo: REPO },
    deps(w),
  )
  assert.equal(p.abandoned, 'claim-lost')
  assert.equal(w.statusPosts().length, before, 'an abandoned run posts nothing')
})

test('post claim takes over a stale claim with a note but not a fresh foreign one', async () => {
  const stale = world({
    statuses: [
      {
        context: 'boss/verify',
        state: 'pending',
        description: 'verifying… 0ld0ld00',
        updated_at: iso(CLAIM_STALE_AFTER_MS + 60_000),
      },
      receipt(),
    ],
  })
  const r1 = await post({ pr: 7, head: HEAD, verdict: 'claim', repo: REPO }, deps(stale))
  assert.equal(r1.won, true)
  assert.equal(stale.notes().length, 1)
  const noteArgs = stale.notes()[0].args
  assert.ok(noteArgs.includes('--idempotency-key'))
  assert.equal(
    noteArgs[noteArgs.indexOf('--idempotency-key') + 1],
    `verify:stale-claim-taken-over:7:${HEAD.slice(0, 12)}`,
  )
  assert.equal(noteArgs.at(-2), '--', 'the body follows -- so a leading -- is never a flag')

  const fresh = world({
    statuses: [
      {
        context: 'boss/verify',
        state: 'pending',
        description: 'verifying… 0ld0ld00',
        updated_at: iso(60_000),
      },
      receipt(),
    ],
  })
  const r2 = await post({ pr: 7, head: HEAD, verdict: 'claim', repo: REPO }, deps(fresh))
  assert.equal(r2.won, false)
  assert.equal(r2.reason, 'claim-held')
  assert.equal(fresh.mutating().length, 0)
})

// ---------------------------------------------------------------------------
// post: wait and pass.

test('post wait without a claim writes nothing; after a claim it posts waiting', async () => {
  const w = world()
  const r = await post(
    { pr: 7, head: HEAD, verdict: 'wait', reason: 'ci-not-settled', repo: REPO },
    deps(w),
  )
  assert.equal(r.posted, false)
  assert.equal(w.mutating().length, 0)

  const claimed = world({
    statuses: [
      {
        context: 'boss/verify',
        state: 'pending',
        description: 'verifying… feedf00d',
        updated_at: iso(1000),
      },
      receipt(),
    ],
  })
  const r2 = await post(
    { pr: 7, head: HEAD, verdict: 'wait', reason: 'ci-not-settled', token: 'feedf00d', repo: REPO },
    deps(claimed),
  )
  assert.equal(r2.posted, true)
  assert.deepEqual(claimed.statusPosts(), [
    { state: 'pending', description: 'waiting: ci-not-settled', target: undefined },
  ])
})

test('post wait for an unreadable base records an evidence-unknown note', async () => {
  const w = world()
  await post({ pr: 7, head: HEAD, verdict: 'wait', reason: 'base-ci-unknown', repo: REPO }, deps(w))
  assert.equal(w.statusPosts().length, 0)
  assert.equal(w.notes().length, 1)
  assert.ok(w.notes()[0].args.includes(`verify:evidence-unknown:7:${HEAD.slice(0, 12)}`))
})

test('post pass writes verified, and verified (approved) under a waiver', async () => {
  const w = world()
  const r = await post({ pr: 7, head: HEAD, verdict: 'pass', repo: REPO }, deps(w))
  assert.equal(r.posted, true)
  const approved = world()
  await post({ pr: 7, head: HEAD, verdict: 'pass', reason: 'approved', repo: REPO }, deps(approved))
  assert.deepEqual(
    [...w.statusPosts(), ...approved.statusPosts()].map((s) => [s.state, s.description]),
    [
      ['success', 'verified'],
      ['success', 'verified (approved)'],
    ],
  )
})

// ---------------------------------------------------------------------------
// post: defect.

function findingsFile(dir) {
  return jsonFile(dir, 'findings.json', [{ title: 'nil deref', file: 'a.go', line: 3 }])
}

test('post defect with auto-repair on: findings comment, then failure targeting it, no tracker writes', async () => {
  const dir = tmp()
  try {
    const w = world()
    const adapter = fakeAdapter()
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'defect', findings: findingsFile(dir), repo: REPO },
      deps(w, { adapter }),
    )
    assert.equal(r.verdict, 'defect')
    const order = w
      .mutating()
      .filter((c) => c.command === 'gh')
      .map((c) => c.args[3])
    assert.deepEqual(order, ['repos/o/r/issues/7/comments', `repos/o/r/statuses/${HEAD}`])
    assert.deepEqual(w.statusPosts(), [
      {
        state: 'failure',
        description: 'defect: 1 finding(s)',
        target: `${PR_URL}#issuecomment-99`,
      },
    ])
    assert.equal(adapter.writeCalls().length, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('post defect with auto-repair off or unreadable also parks to a human', async () => {
  const dir = tmp()
  try {
    for (const w of [
      world({ show: { repo_can_auto_repair: false, last_repair: null } }),
      world({ show: { repo_can_auto_repair: null } }),
      world({ show: null }),
      world({ sessions: [] }),
    ]) {
      const adapter = fakeAdapter()
      const r = await post(
        { pr: 7, head: HEAD, verdict: 'defect', findings: findingsFile(dir), repo: REPO },
        deps(w, { adapter }),
      )
      assert.equal(r.verdict, 'defect')
      assert.equal(adapter.writeCalls().length, 1)
      const write = adapter.writeCalls()[0]
      assert.deepEqual(write.addLabels, ['needs-human'])
      assert.equal(write.mentionCreator, true)
      assert.equal(write.marker, verifyMarker(HEAD))
      assert.match(write.comment, /issuecomment-99/)
      assert.deepEqual(w.statusPosts(), [
        {
          state: 'failure',
          description: 'defect: 1 finding(s), needs human',
          target: `${PR_URL}#issuecomment-99`,
        },
      ])
    }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('post defect after repair is exhausted becomes human repair-exhausted, never failure', async () => {
  const dir = tmp()
  try {
    const w = world({
      show: { repo_can_auto_repair: true, last_repair: { attempt_count: REPAIR_EXHAUSTED_AFTER } },
    })
    const adapter = fakeAdapter()
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'defect', findings: findingsFile(dir), repo: REPO },
      deps(w, { adapter }),
    )
    assert.equal(r.verdict, 'human')
    assert.equal(r.reason, 'repair-exhausted')
    assert.deepEqual(w.statusPosts(), [
      { state: 'pending', description: 'needs human: repair-exhausted', target: undefined },
    ])
    assert.ok(w.statusPosts().every((s) => s.state !== 'failure'))
    assert.equal(adapter.writeCalls().length, 1)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// post: human.

test('every human reason posts needs human, adds the label and writes exactly one marked comment', async () => {
  for (const code of HUMAN_REASONS) {
    const w = world()
    const adapter = fakeAdapter()
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'human', reason: code, repo: REPO },
      deps(w, { adapter }),
    )
    assert.equal(r.posted, true, code)
    assert.deepEqual(w.statusPosts(), [
      { state: 'pending', description: `needs human: ${code}`, target: undefined },
    ])
    const writes = adapter.writeCalls()
    assert.equal(writes.length, 1, code)
    assert.equal(writes[0].issueId, 'BOS-1')
    assert.deepEqual(writes[0].addLabels, ['needs-human'])
    assert.equal(writes[0].mentionCreator, true)
    assert.equal(writes[0].marker, `boss-verify head ${HEAD}`)
    assert.match(writes[0].comment, new RegExp(code))
    assert.equal(
      w.notes().length,
      code === 'extension-failed' ? 1 : 0,
      `${code}: only an extension malfunction records a note`,
    )
  }
})

test('post never writes a human or defect status without its tracker writes', async () => {
  const dir = tmp()
  try {
    for (const adapter of [null, {}, fakeAdapter({ missingKey: true })]) {
      for (const options of [
        { verdict: 'human', reason: 'no-receipt' },
        { verdict: 'defect', findings: findingsFile(dir) },
      ]) {
        const w = world({ show: { repo_can_auto_repair: false } })
        const r = await post({ pr: 7, head: HEAD, repo: REPO, ...options }, deps(w, { adapter }))
        assert.equal(r.trackerWrites, 'unavailable')
        assert.equal(r.posted, false)
        assert.equal(w.mutating().length, 0, 'no status, no PR comment')
        assert.ok(r.writes.some((write) => write.kind === 'tracker'))
      }
    }
    const noTicket = world({ pr: { body: BODY.replace(/^Linear issue:.*\n/, '') } })
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'human', reason: 'no-receipt', repo: REPO },
      deps(noTicket),
    )
    assert.equal(r.trackerWrites, 'unavailable')
    assert.equal(noTicket.statusPosts().length, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('post holds the status when the tracker write fails or is indeterminate', async () => {
  for (const outcome of ['failed', 'indeterminate']) {
    const w = world()
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'human', reason: 'no-receipt', repo: REPO },
      deps(w, { adapter: fakeAdapter({ outcome }) }),
    )
    assert.equal(r.trackerWrites, outcome)
    assert.equal(w.statusPosts().length, 0)
  }
})

test('post --dry-run plans every verdict and calls nothing mutating', async () => {
  const dir = tmp()
  try {
    for (const options of [
      { verdict: 'claim' },
      { verdict: 'wait', reason: 'base-ci-unknown' },
      { verdict: 'pass' },
      { verdict: 'defect', findings: findingsFile(dir) },
      { verdict: 'human', reason: 'ledger-open' },
    ]) {
      const w = world({ show: { repo_can_auto_repair: false } })
      const adapter = fakeAdapter()
      const r = await post(
        { pr: 7, head: HEAD, repo: REPO, dryRun: true, ...options },
        deps(w, { adapter }),
      )
      assert.equal(r.dryRun, true, options.verdict)
      assert.equal(w.mutating().length, 0, options.verdict)
      assert.equal(adapter.writeCalls().length, 0, options.verdict)
      assert.ok(r.writes.length > 0, `${options.verdict} lists its planned writes`)
    }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('post rejects malformed input before any read', async () => {
  const w = world()
  for (const options of [
    { verdict: 'pass', head: 'abc' },
    { verdict: 'nope', head: HEAD },
    { verdict: 'human', head: HEAD, reason: 'ci-not-settled' },
    { verdict: 'wait', head: HEAD },
    { verdict: 'defect', head: HEAD, findings: '/nonexistent/findings.json' },
  ]) {
    await assert.rejects(post({ pr: 7, repo: REPO, ...options }, deps(w)), UsageError)
  }
  assert.equal(w.calls.length, 0)
})

// ---------------------------------------------------------------------------
// merge.

const verified = () => [
  { context: 'boss/verify', state: 'success', description: 'verified', updated_at: iso(1000) },
  receipt(),
]

test('merge runs the one pinned argv, observes MERGED, then moves the ticket to Done', async () => {
  const w = world({
    statuses: verified(),
    prAfterMerge: prView({ state: 'MERGED', mergeCommit: { oid: MERGED_SHA } }),
  })
  const adapter = fakeAdapter()
  const r = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(w, { adapter }))
  assert.equal(r.merged, true)
  assert.equal(r.mergeSha, MERGED_SHA)
  const mergeCall = w.calls.find((c) => c.command === 'boss' && c.args[0] === 'merge')
  assert.deepEqual(mergeCall.args, ['merge', 'sess-1', '--yes', '--json', '--match-head', HEAD])
  assert.deepEqual(adapter.writeCalls(), [
    { issueId: 'BOS-1', stateName: 'Done', removeLabels: ['needs-human'] },
  ])
  assert.equal(r.trackerWrites, 'ok')
})

test('merge returns reverify on HEAD_MISMATCH and on a moved head without calling boss merge', async () => {
  const mismatch = world({ statuses: verified(), merge: { ok: false, error: 'HEAD_MISMATCH' } })
  const r1 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(mismatch))
  assert.equal(r1.verdict, 'reverify')
  assert.equal(r1.merged, false)

  const moved = world({ statuses: verified(), pr: { headRefOid: OTHER } })
  const r2 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(moved))
  assert.equal(r2.verdict, 'reverify')
  assert.equal(moved.calls.filter((c) => c.command === 'boss' && c.args[0] === 'merge').length, 0)
})

test('merge with no session row is human no-session and never falls back to gh pr merge', async () => {
  const w = world({
    statuses: verified(),
    sessions: [{ id: 'other', pr_url: 'https://github.com/o/r/pull/8' }],
  })
  const r = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'human')
  assert.equal(r.reason, 'no-session')
  assert.equal(w.mutating().length, 0)
  assert.equal(w.calls.filter((c) => c.args[0] === 'pr' && c.args[1] === 'merge').length, 0)
})

test('merge re-checks eligibility and the verified status before merging', async () => {
  const blocked = world({ statuses: verified(), pr: { mergeStateStatus: 'BLOCKED' } })
  const r1 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(blocked))
  assert.equal(r1.verdict, 'wait')
  assert.ok(r1.reasons.includes('merge-state-blocked'))
  assert.equal(blocked.mutating().length, 0)

  const unverified = world()
  const r2 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(unverified))
  assert.equal(r2.reason, 'not-verified')
  assert.equal(unverified.mutating().length, 0)

  const claimed = world({
    statuses: [
      {
        context: 'boss/verify',
        state: 'pending',
        description: 'verifying… 11112222',
        updated_at: iso(1000),
      },
    ],
  })
  const r3 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(claimed))
  assert.equal(r3.abandoned, 'claim-lost')
})

test('merge honours an approved ledger waiver but still enforces merge state', async () => {
  const openBody = BODY.replace('- [x] it works', '- [ ] it works')
  const approvedStatuses = () => [
    { ...verified()[0], description: 'verified (approved)' },
    ...verified().slice(1),
  ]
  const waived = world({
    statuses: approvedStatuses(),
    pr: { body: openBody },
    prAfterMerge: prView({ state: 'MERGED', mergeCommit: { oid: MERGED_SHA } }),
  })
  const r1 = await merge(
    { pr: 7, head: HEAD, repo: REPO },
    deps(waived, { adapter: fakeAdapter() }),
  )
  assert.equal(r1.merged, true)

  const unwaived = world({ statuses: verified(), pr: { body: openBody } })
  const r2 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(unwaived))
  assert.equal(r2.verdict, 'wait')
  assert.ok(r2.reasons.includes('criteria-unmet'))
  assert.equal(unwaived.mutating().length, 0)

  const blocked = world({
    statuses: approvedStatuses(),
    pr: { body: openBody, mergeStateStatus: 'BLOCKED' },
  })
  const r3 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(blocked))
  assert.equal(r3.verdict, 'wait')
  assert.deepEqual(r3.reasons, ['merge-state-blocked'])
})

test('merge --dry-run plans the merge and the Done move and calls nothing mutating', async () => {
  const w = world({ statuses: verified() })
  const adapter = fakeAdapter()
  const r = await merge({ pr: 7, head: HEAD, repo: REPO, dryRun: true }, deps(w, { adapter }))
  assert.equal(r.dryRun, true)
  assert.equal(w.mutating().length, 0)
  assert.equal(adapter.writeCalls().length, 0)
  assert.deepEqual(
    r.writes.map((write) => write.kind),
    ['merge', 'tracker'],
  )
  assert.deepEqual(r.writes[0].argv, ['merge', 'sess-1', '--yes', '--json', '--match-head', HEAD])
})

// ---------------------------------------------------------------------------
// approval.

const parkedHuman = (code) => [
  {
    context: 'boss/verify',
    state: 'pending',
    description: `needs human: ${code}`,
    updated_at: iso(1000),
  },
  receipt(),
]

test('approval: label removal on a needs-human head waives only that code', async () => {
  const w = world({ statuses: parkedHuman('no-receipt') })
  const r = await approval(
    { pr: 7, repo: REPO },
    deps(w, { adapter: fakeAdapter({ labels: ['agent-build'] }) }),
  )
  assert.deepEqual(
    { approval: r.approval, waive: r.waive, via: r.via },
    { approval: 'policy-park-approved', waive: 'no-receipt', via: 'label-removed' },
  )
  assert.equal(w.mutating().length, 0)

  // The waived judge still refuses on red CI.
  const red = world({
    statuses: [],
    pr: { statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'FAILURE' }] },
  })
  const j = await judge({ pr: 7, repo: REPO, waive: r.waive }, deps(red))
  assert.equal(j.verdict, 'wait')
})

test('approval: a review approval counts, a human merge counts, otherwise none', async () => {
  const approved = world({
    statuses: parkedHuman('ledger-open'),
    pr: { reviewDecision: 'APPROVED' },
  })
  const r1 = await approval({ pr: 7, repo: REPO }, deps(approved))
  assert.equal(r1.approval, 'policy-park-approved')
  assert.equal(r1.via, 'review-approved')
  assert.equal(r1.waive, 'ledger-open')

  const merged = world({ pr: { state: 'MERGED', mergeCommit: { oid: MERGED_SHA } } })
  assert.equal((await approval({ pr: 7, repo: REPO }, deps(merged))).approval, 'merged-by-human')

  const still = world({ statuses: parkedHuman('ledger-open') })
  assert.equal((await approval({ pr: 7, repo: REPO }, deps(still))).approval, 'none')
})

test('approval: label removal on a parked defect head means reverify, not merge', async () => {
  const w = world({
    statuses: [
      {
        context: 'boss/verify',
        state: 'failure',
        description: 'defect: 2 finding(s), needs human',
        updated_at: iso(1000),
      },
      receipt(),
    ],
  })
  const r = await approval({ pr: 7, repo: REPO }, deps(w, { adapter: fakeAdapter({ labels: [] }) }))
  assert.equal(r.approval, 'reverify')
  assert.equal(r.waive, undefined)
})

test('classifyApproval: a repair-path defect has no label to remove; unreadable labels never approve', () => {
  const repairDefect = { state: 'failure', description: 'defect: 1 finding(s)' }
  assert.equal(
    classifyApproval({ prState: 'OPEN', status: repairDefect, labels: [] }).approval,
    'none',
  )
  assert.equal(
    classifyApproval({ prState: 'OPEN', status: repairDefect, reviewDecision: 'APPROVED' })
      .approval,
    'reverify',
  )
  const park = { state: 'pending', description: 'needs human: no-receipt' }
  assert.equal(classifyApproval({ prState: 'OPEN', status: park, labels: null }).approval, 'none')
  assert.equal(
    classifyApproval({
      prState: 'OPEN',
      status: { state: 'success', description: 'verified' },
      labels: [],
    }).approval,
    'none',
  )
})

// ---------------------------------------------------------------------------
// rearm and CLI.

test('rearm arms exactly the checks_passed_ready on-transition callback', async () => {
  const expected = [
    'callback',
    'add',
    '7',
    'checks_passed_ready',
    '--on-transition',
    '--chat',
    'chat-1',
    '--repo',
    'o/r',
    '--message',
    '/boss-verify 7',
    '--json',
  ]
  assert.deepEqual(rearmArgv({ pr: 7, chat: 'chat-1', repo: REPO }), expected)
  const w = world()
  const r = await rearm({ pr: 7, chat: 'chat-1', repo: REPO }, deps(w))
  assert.equal(r.armed, true)
  assert.deepEqual(w.mutating()[0].args, expected)

  const dry = world()
  const d = await rearm({ pr: 7, chat: 'chat-1', repo: REPO, dryRun: true }, deps(dry))
  assert.equal(d.dryRun, true)
  assert.equal(dry.mutating().length, 0)
  assert.deepEqual(d.writes, [{ kind: 'callback', argv: expected }])
})

test('main dispatches verbs and rejects usage errors before any read', async () => {
  const w = world()
  const r = await main(['judge', '--pr', '7', '--repo', REPO], deps(w))
  assert.equal(r.verdict, 'pass')
  const dry = await main(
    ['post', '--pr', '7', '--head', HEAD, '--verdict', 'pass', '--repo', REPO, '--dry-run'],
    deps(world()),
  )
  assert.equal(dry.dryRun, true)
  const fresh = world()
  for (const argv of [
    [],
    ['nope', '--pr', '7'],
    ['judge'],
    ['judge', '--pr', 'x'],
    ['judge', '--pr', '7', '--bogus', '1'],
    ['rearm', '--pr', '7'],
  ]) {
    await assert.rejects(main(argv, deps(fresh)), UsageError, JSON.stringify(argv))
  }
  assert.equal(fresh.calls.length, 0)
})

// ---------------------------------------------------------------------------
// candidates.

const SHA_OF = (n) => n.toString(16).padStart(40, '0')
const ticketBody = (id) => `Linear issue: https://linear.app/acme/issue/${id}/t\n\nbody`

/** A validated config whose tracker adapter carries the given `selection` value (or none). */
function candidateConfig(selection) {
  const config = mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        states: { inReview: 'In Review' },
        labels: { agentBuild: 'agent-build', needsHuman: 'needs-human' },
        ...(selection === undefined ? {} : { selection }),
      },
    },
  })
  validateConfig(config, 'test')
  return config
}

// One open PR per ticket by default: PR n carries ticket BOS-n in its body, head SHA_OF(n).
function candidateWorld(opts = {}) {
  const tickets = opts.tickets ?? [
    { identifier: 'BOS-1', labels: ['agent-build'], attachments: [] },
  ]
  const prs =
    opts.prs ??
    tickets.map((t) => {
      const n = Number(t.identifier.split('-')[1])
      return {
        number: n,
        url: `https://github.com/o/r/pull/${n}`,
        body: ticketBody(t.identifier),
        headRefOid: SHA_OF(n),
        isDraft: false,
      }
    })
  const w = { calls: [], queries: [], probes: [] }
  w.adapter = {
    selectPlanned: async (query) => {
      w.queries.push(query)
      if (opts.trackerThrows) throw new Error('linear unreachable')
      return tickets
    },
    applyIssueWrites: async (writes) => {
      w.probes.push(writes)
      return { outcome: 'ok', issue: { labels: opts.labels?.[writes.issueId] ?? ['agent-build'] } }
    },
  }
  w.read = (command, args) => {
    w.calls.push({ command, args: [...args] })
    const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
    const fail = () => ({ ok: false, error: 'exit-1', exit: 1, output: '' })
    if (command !== 'gh') return fail()
    if (args[0] === 'repo') return ok({ nameWithOwner: REPO })
    if (args[0] === 'pr' && args[1] === 'list') return opts.listFails ? fail() : ok(prs)
    if (args[0] === 'pr' && args[1] === 'view') {
      const n = Number(args[2])
      const pr = prs.find((p) => p.number === n)
      if (!pr || opts.viewFails?.includes(n)) return fail()
      return ok({
        ...prView({ number: n, url: pr.url, body: pr.body, headRefOid: pr.headRefOid }),
        statusCheckRollup: opts.rollups?.[n] ?? green,
        reviewDecision: opts.reviews?.[n] ?? '',
      })
    }
    if (args[0] === 'api') {
      const sha = args[1].match(/commits\/([0-9a-f]{40})\/statuses/)?.[1]
      if (!sha) return fail()
      if (opts.statusFails?.includes(sha)) return fail()
      return { ok: true, output: JSON.stringify([opts.statuses?.[sha] ?? []]), exit: 0 }
    }
    return fail()
  }
  w.statusReads = () => w.calls.filter((c) => c.args[0] === 'api').map((c) => c.args[1])
  return w
}

const candDeps = (w, config = candidateConfig()) => ({
  read: w.read,
  now: () => NOW,
  loadConfig: () => config,
  adapter: w.adapter,
})
const verifyStatus = (state, description, msAgo = 1000) => [
  { context: 'boss/verify', state, description, updated_at: iso(msAgo) },
]

test('candidates: the selection matrix pins state to inReview, requires the build label and re-admits needs-human', async () => {
  const cases = [
    [
      'config only',
      { labels: { include: ['backend'] }, stages: { verify: { labels: { exclude: ['infra'] } } } },
      {},
      { include: ['backend'], exclude: ['infra'] },
    ],
    [
      'flag only',
      undefined,
      { labels: { exclude: ['infra'] } },
      { include: [], exclude: ['infra'] },
    ],
    [
      'flag over config',
      { labels: { include: ['backend'], exclude: ['shared-out'] } },
      { labels: { exclude: ['flag-out'] } },
      { include: ['backend'], exclude: ['flag-out'] },
    ],
  ]
  for (const [name, selection, flags, labels] of cases) {
    const w = candidateWorld()
    await candidates({ repo: REPO, selection: flags }, candDeps(w, candidateConfig(selection)))
    assert.equal(w.queries.length, 1, name)
    const q = w.queries[0]
    assert.equal(q.state, 'In Review', name)
    assert.deepEqual(q.requireLabels, ['agent-build'], name)
    assert.deepEqual(q.excludeLabels, [], name)
    assert.deepEqual(q.selection.labels, labels, name)
  }

  // --ticket bypasses the selection (flags and config) but keeps the core rules.
  const w = candidateWorld({
    tickets: [
      { identifier: 'BOS-1', labels: ['agent-build'], attachments: [] },
      { identifier: 'BOS-2', labels: ['agent-build'], attachments: [] },
    ],
  })
  const r = await candidates(
    { repo: REPO, ticket: 'bos-2', selection: { labels: { include: ['x'] } } },
    candDeps(w, candidateConfig({ labels: { include: ['backend'] } })),
  )
  assert.deepEqual(w.queries[0].selection.labels, { include: [], exclude: [] })
  assert.deepEqual(w.queries[0].requireLabels, ['agent-build'])
  assert.deepEqual(
    r.candidates.map((c) => c.pr),
    [2],
  )
  const missing = await candidates({ repo: REPO, ticket: 'BOS-9' }, candDeps(candidateWorld()))
  assert.deepEqual(missing, {
    candidates: [],
    skipped: [{ ticket: 'BOS-9', reason: 'not-in-review' }],
  })
})

test('candidates: a clean head is a candidate with its PR, head, repo and claim none', async () => {
  const w = candidateWorld()
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(r, {
    candidates: [
      {
        ticket: 'BOS-1',
        pr: 1,
        url: 'https://github.com/o/r/pull/1',
        headSha: SHA_OF(1),
        repo: REPO,
        claim: 'none',
      },
    ],
    skipped: [],
  })
})

test('candidates: the skip rules — verified, claimed, stale and waiting admitted, CI, draft, no PR, ambiguous', async () => {
  const ids = [1, 2, 3, 4, 5, 6]
  const tickets = [...ids, 7, 8, 9, 10].map((n) => ({
    identifier: `BOS-${n}`,
    labels: ['agent-build'],
    attachments: [],
  }))
  const prs = ids.map((n) => ({
    number: n,
    url: `https://github.com/o/r/pull/${n}`,
    body: ticketBody(`BOS-${n}`),
    headRefOid: SHA_OF(n),
    isDraft: false,
  }))
  // BOS-7: draft. BOS-8: no PR. BOS-9: two open PRs. BOS-10: attached PR whose body names BOS-1.
  prs.push({
    number: 7,
    url: 'u7',
    body: ticketBody('BOS-7'),
    headRefOid: SHA_OF(7),
    isDraft: true,
  })
  prs.push({
    number: 90,
    url: 'u90',
    body: ticketBody('BOS-9'),
    headRefOid: SHA_OF(90),
    isDraft: false,
  })
  prs.push({
    number: 91,
    url: 'u91',
    body: ticketBody('BOS-9'),
    headRefOid: SHA_OF(91),
    isDraft: false,
  })
  tickets[9].attachments = [{ url: 'https://github.com/o/r/pull/1' }]
  const w = candidateWorld({
    tickets,
    prs,
    statuses: {
      [SHA_OF(1)]: verifyStatus('success', 'verified'),
      [SHA_OF(2)]: verifyStatus('pending', 'verifying… t1', 60_000),
      [SHA_OF(3)]: verifyStatus('pending', 'verifying… t1', CLAIM_STALE_AFTER_MS + 1),
      [SHA_OF(4)]: verifyStatus('pending', 'waiting: ci-not-settled'),
    },
    rollups: {
      5: [{ name: 'ci', status: 'IN_PROGRESS', conclusion: '' }],
      6: [{ name: 'ci', status: 'COMPLETED', conclusion: 'FAILURE' }],
    },
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => [c.pr, c.claim]),
    [
      [3, 'stale'],
      [4, 'waiting'],
    ],
  )
  const reasons = Object.fromEntries(r.skipped.map((s) => [s.ticket, s.reason]))
  assert.deepEqual(reasons, {
    'BOS-1': 'verified',
    'BOS-2': 'claimed',
    'BOS-5': 'ci-unsettled',
    'BOS-6': 'ci-red',
    'BOS-7': 'draft',
    'BOS-8': 'no-open-pr',
    'BOS-9': 'ambiguous-pr',
    'BOS-10': 'ambiguous-pr',
  })
})

test('candidates: an attachment links a PR whose body carries no ticket line', async () => {
  const w = candidateWorld({
    tickets: [
      {
        identifier: 'BOS-4',
        labels: ['agent-build'],
        attachments: [{ url: 'https://github.com/O/R/pull/12' }],
      },
    ],
    prs: [
      { number: 12, url: 'u12', body: 'no ticket line', headRefOid: SHA_OF(12), isDraft: false },
    ],
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => [c.ticket, c.pr]),
    [['BOS-4', 12]],
  )
})

test('candidates: needs-human is excluded unless approval hands it back', async () => {
  const nh = (n) => ({
    identifier: `BOS-${n}`,
    labels: ['agent-build', 'needs-human'],
    attachments: [],
  })
  const w = candidateWorld({
    tickets: [nh(1), nh(2), nh(3), nh(4)],
    statuses: {
      [SHA_OF(2)]: verifyStatus('pending', 'needs human: ledger-open'),
      [SHA_OF(3)]: verifyStatus('failure', 'defect: 2 finding(s), needs human'),
      [SHA_OF(4)]: verifyStatus('pending', 'needs human: no-receipt'),
    },
    // A review approval hands BOS-2 and BOS-3 back; BOS-4 is still parked.
    reviews: { 2: 'APPROVED', 3: 'APPROVED' },
    labels: { 'BOS-2': ['needs-human'], 'BOS-3': ['needs-human'], 'BOS-4': ['needs-human'] },
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => ({ pr: c.pr, approval: c.approval, waive: c.waive })),
    [
      { pr: 2, approval: 'policy-park-approved', waive: 'ledger-open' },
      { pr: 3, approval: 'reverify', waive: undefined },
    ],
  )
  assert.deepEqual(
    r.skipped.map((s) => [s.ticket, s.reason]),
    [
      ['BOS-1', 'needs-human'],
      ['BOS-4', 'parked'],
    ],
  )
})

test('candidates: unreadable per-head evidence is unknown:<what>, never a candidate', async () => {
  const w = candidateWorld({
    tickets: [1, 2].map((n) => ({
      identifier: `BOS-${n}`,
      labels: ['agent-build'],
      attachments: [],
    })),
    statusFails: [SHA_OF(1)],
    viewFails: [2],
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(r.candidates, [])
  assert.deepEqual(
    r.skipped.map((s) => s.reason),
    ['unknown:status', 'unknown:rollup'],
  )
})

test('candidates: a failed tracker read or gh pr list throws, never an empty list', async () => {
  await assert.rejects(
    candidates({ repo: REPO }, candDeps(candidateWorld({ trackerThrows: true }))),
    /linear unreachable/,
  )
  await assert.rejects(
    candidates({ repo: REPO }, candDeps(candidateWorld({ listFails: true }))),
    /gh pr list failed/,
  )
  // Through the CLI: an ordinary Error (exit 1), not a UsageError (exit 2).
  const err = await main(
    ['candidates', '--repo', REPO],
    candDeps(candidateWorld({ listFails: true })),
  ).catch((e) => e)
  assert.ok(err instanceof Error && !(err instanceof UsageError))
  await assert.rejects(main(['candidates', 'BOS-1'], candDeps(candidateWorld())), UsageError)
  await assert.rejects(main(['candidates', '--limit', '0'], candDeps(candidateWorld())), UsageError)
})

test('candidates: --limit stops the oldest-first walk once enough candidates are found', async () => {
  const w = candidateWorld({
    tickets: [3, 1, 2].map((n) => ({
      identifier: `BOS-${n}`,
      labels: ['agent-build'],
      attachments: [],
    })),
  })
  const r = await main(['candidates', '--repo', REPO, '--limit', '2'], candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => c.pr),
    [1, 2],
  )
  assert.equal(
    w.statusReads().some((path) => path.includes(SHA_OF(3))),
    false,
  )
})
