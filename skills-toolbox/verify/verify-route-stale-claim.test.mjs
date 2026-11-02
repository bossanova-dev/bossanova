import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { join } from 'node:path'

import { classifyHeadStatus, post, postOptionsFromArgv, tickFingerprint } from './verify-gate.mjs'
import { CLAIM_STALE_AFTER_MS, claimState } from './verify-vocabulary.mjs'
import {
  CLAIM_EXPIRED_REASON,
  REMOTE_ORPHAN_AFTER_MS,
  claimForVerdict,
  claimWake,
  earliestClaimExpiry,
  remoteClaimFields,
  remoteOrphan,
  verdictClaimLost,
  wakeAtOf,
} from './verify-stale-claim.mjs'
import {
  readQuietEntry,
  readQuietFingerprint,
  safetyNetIdleUntil,
  storeQuietFingerprint,
  sweepScopeKey,
} from './verify-sweep-state.mjs'
import { postArgv } from './verify-post-files.mjs'
import { sweep } from './verify-route.mjs'
import { evaluateBossVerifyGate } from '../cron-gates/boss-verify.mjs'

const BOSS = '/fake/bin/boss'
const NODE = 'node'
const REPO = 'o/r'
const MIN = 60_000
const T0 = Date.parse('2026-11-02T00:00:00.000Z')
const sha = (n) => n.toString(16).padStart(40, '0')
const url = (n) => `https://github.com/${REPO}/pull/${n}`
const iso = (ms) => new Date(ms).toISOString()
const claimStatus = (token, at) => ({
  state: 'pending',
  description: `verifying… ${token}`,
  updatedAt: iso(at),
})
const BLOCKERS = [
  {
    id: 'conflict',
    kind: 'conflict',
    severity: null,
    file: null,
    line: null,
    summary: 'conflict',
    text: 'details',
  },
]
const JUDGED = {
  pass: { verdict: 'pass', reason: 'verified' },
  human: { verdict: 'human', reason: 'partial-build' },
  blocked: { verdict: 'blocked', reason: 'blockers', blockers: BLOCKERS },
}

/**
 * A fake GitHub holding each head's latest `boss/verify` status, behind the sweep's one `run` seam.
 * `candidates` classifies those statuses with verify-gate's own classifier and wake helper, and
 * `post` follows verify-gate's claim rules (a stale claim is taken over, a verdict over another
 * run's claim is abandoned as claim-lost through the gate's own `verdictClaimLost`, and a dry-run
 * takeover writes nothing), so a tick's writes land on the statuses the next tick
 * reads. `prs` are the open PRs; a PR whose URL the sweep excludes (or does not include) is
 * reported as a remote-session skip, as discovery reports it.
 */
function world(opts = {}) {
  const w = {
    clock: opts.clock ?? T0,
    statuses: { ...(opts.statuses ?? {}) },
    calls: [],
    spawns: [],
    sends: [],
    tokens: 0,
  }
  const prs = opts.prs ?? [1]
  const verdicts = opts.verdicts ?? {}
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  const fail = (error = 'exit-1') => ({ ok: false, error, exit: 1, output: '' })
  const rows = () =>
    prs.map((n) => ({
      pr: { number: n, url: url(n), headRefOid: sha(n), isDraft: false },
      status: w.statuses[n] ?? null,
    }))
  const setStatus = (pr, state, description) =>
    (w.statuses[pr] = { state, description, updatedAt: iso(w.clock) })

  const candidates = (rest) => {
    const flag = (name) => (rest.includes(`--${name}`) ? rest[rest.indexOf(`--${name}`) + 1] : null)
    const only = flag('pr-urls')?.split(',')
    const excluded = new Set(flag('exclude-pr-urls')?.split(',') ?? [])
    const included = (pr) => (!only || only.includes(pr.url)) && !excluded.has(pr.url)
    const all = rows()
    const inScope = all.filter((row) => included(row.pr))
    const wake = claimWake(all, w.clock, {
      inScope: included,
      remote: (pr) => excluded.has(pr.url),
    })
    const fingerprint = tickFingerprint({ rows: inScope, repo: REPO, now: w.clock })
    if (flag('quiet-fingerprint') === fingerprint)
      return ok({
        candidates: [],
        skipped: [],
        mergeRetries: [],
        unchanged: true,
        fingerprint,
        ...wake,
      })
    const out = []
    const skipped = [...(opts.discoverySkips ?? [])]
    for (const row of all) {
      const pr = row.pr.number
      if (!included(row.pr)) {
        skipped.push({
          ticket: null,
          pr,
          reason: 'remote-session',
          ...remoteClaimFields(row, w.clock, REPO),
        })
        continue
      }
      const head = classifyHeadStatus(row.status, w.clock)
      if (head.skip) skipped.push({ ticket: null, pr, reason: head.skip })
      else
        out.push({
          ticket: null,
          pr,
          url: url(pr),
          headSha: sha(pr),
          repo: REPO,
          claim: head.claim,
        })
    }
    return ok({
      candidates: out,
      skipped,
      mergeRetries: [],
      fingerprint,
      skippable: true,
      ...wake,
    })
  }

  const post = (rest) => {
    const flag = (name) => (rest.includes(`--${name}`) ? rest[rest.indexOf(`--${name}`) + 1] : '')
    const pr = Number(flag('pr'))
    const verdict = flag('verdict')
    const token = flag('token')
    const dry = rest.includes('--dry-run')
    const current = w.statuses[pr] ?? null
    if (verdict === 'claim') {
      const state = claimState(current, { now: w.clock })
      if (state.kind === 'foreign') return ok({ won: false, reason: 'claim-held' })
      if (rest.includes('--takeover') && state.kind !== 'stale')
        return ok({ won: false, reason: 'not-expired' })
      const next = `tok-${++w.tokens}`
      if (!dry) setStatus(pr, 'pending', `verifying… ${next}`)
      return ok({
        won: true,
        token: next,
        ...(state.kind === 'stale' ? { replaced: state.token } : {}),
      })
    }
    if (verdictClaimLost(current, token, w.clock, dry))
      return ok({ verdict, abandoned: 'claim-lost' })
    if (verdict === 'wait') {
      const kind = claimState(current, { token, now: w.clock }).kind
      if (kind === 'terminal') return ok({ verdict, posted: false, skipped: 'status-standing' })
      if (!dry) setStatus(pr, 'pending', `waiting: ${flag('reason')}`)
      return ok({ verdict, posted: true })
    }
    if (!dry) {
      if (verdict === 'pass') setStatus(pr, 'success', 'verified')
      if (verdict === 'human') setStatus(pr, 'pending', `needs human: ${flag('reason')}`)
      if (verdict === 'blocked') setStatus(pr, 'failure', 'blocked: conflict')
    }
    return ok({ verdict, posted: true })
  }

  w.run = (cmd, args) => {
    w.calls.push({ cmd, args: [...args] })
    if (cmd === NODE) {
      const [, verb, ...rest] = args
      if (verb === 'candidates') return candidates(rest)
      if (verb === 'judge') {
        const pr = Number(rest[rest.indexOf('--pr') + 1])
        const v = typeof verdicts[pr] === 'function' ? verdicts[pr](w) : verdicts[pr]
        return ok({ headSha: sha(pr), pr, ...(v ?? JUDGED.pass) })
      }
      if (verb === 'post') return post(rest)
      if (verb === 'observe-merge') return ok({ merged: false, state: 'OPEN' })
      return fail('unexpected-verb')
    }
    if (cmd === BOSS) {
      if (args[0] === 'callback' && args[1] === 'list') return ok([])
      if (args[0] === 'ls' && args.includes('--all-daemons'))
        return opts.fleetFails ? fail() : ok({ sessions: opts.fleet ?? [] })
      if (args[0] === 'ls') return ok({ sessions: opts.sessions ?? [] })
      if (args[0] === 'chats') return ok({ chats: [] })
      if (args[0] === 'chat' && args[1] === 'new')
        return ok({ chat: { agent_session_id: 'chat-new', session_id: args[2], title: 'verify' } })
      if (args[0] === 'chat' && args[1] === 'send') {
        // The session receives the message and writes nothing: the run that claimed is killed.
        w.sends.push(args[3])
        return ok({ delivery: { delivered: true, delivery_state: 'DELIVERY_STATE_SUBMITTED' } })
      }
      return fail('unexpected-boss')
    }
    return fail('unexpected-command')
  }
  w.deps = (extra = {}) => ({
    run: w.run,
    nodeBin: NODE,
    readProjectConfig: () => (opts.shared ? { daemons: [{}, {}] } : null),
    now: () => w.clock,
    readFile: () => `${JSON.stringify({ merged: true, mergeSha: sha(900) })}\n`,
    sleep: (ms) => {
      w.clock += ms
    },
    resolveBoss: () => ({ ok: true, path: BOSS, reason: '' }),
    spawn: (cmd, args) => {
      w.spawns.push({ cmd, args: [...args] })
      return { pid: 1, unref() {}, on() {} }
    },
    env: { PATH: '/usr/bin', LINEAR_API_KEY: 'k', REPO_DIR: '/repo' },
    cwd: '/repo',
    trackerName: 'linear',
    logDir: tempDir('verify-stale-claim-'),
    stateFile: null,
    ...extra,
  })
  // The gate argv after `node verify-gate.mjs`, in call order.
  w.verbs = () => w.calls.filter((c) => c.cmd === NODE).map((c) => c.args.slice(1))
  w.posts = () => w.verbs().filter((v) => v[0] === 'post')
  w.flag = (argv, name) => (argv.includes(`--${name}`) ? argv[argv.indexOf(`--${name}`) + 1] : null)
  w.merges = () => w.spawns.filter((s) => s.cmd === NODE && s.args[1] === 'merge')
  w.reset = () => {
    w.calls = []
    w.spawns = []
    w.sends = []
  }
  return w
}

const lines = (result) => result.records.map((r) => r.line)
const w2 = (argv) => argv[argv.indexOf('--pr') + 1] === '2'
const tokenOf = (status) => claimState(status, { now: 0 }).token

// ---------------------------------------------------------------------------
// Pure helpers.

test('earliestClaimExpiry is the first live foreign claim to expire, ignoring other statuses', () => {
  const row = (status) => ({ pr: { number: 1, url: url(1) }, status })
  const rows = [
    row(claimStatus('a', T0 - 10 * MIN)),
    row(claimStatus('b', T0 - 20 * MIN)),
    row(claimStatus('old', T0 - 40 * MIN)),
    row({ state: 'pending', description: 'waiting: ci-unsettled', updatedAt: iso(T0) }),
    row(null),
    { pr: { number: 9 }, gap: 'contexts', status: undefined },
  ]
  assert.equal(earliestClaimExpiry(rows, T0), T0 - 20 * MIN + CLAIM_STALE_AFTER_MS)
  assert.equal(
    earliestClaimExpiry(rows.slice(2), T0),
    null,
    'an expired claim has no future expiry',
  )
  assert.equal(
    earliestClaimExpiry(rows.slice(2), T0, REMOTE_ORPHAN_AFTER_MS),
    T0 - 40 * MIN + REMOTE_ORPHAN_AFTER_MS,
  )
  assert.equal(earliestClaimExpiry(undefined, T0), null)
})

test('claimWake keys in-scope expiry and remote orphan time on their own rows', () => {
  const rows = [
    { pr: { number: 1, url: url(1) }, status: claimStatus('a', T0 - 5 * MIN) },
    { pr: { number: 2, url: url(2) }, status: claimStatus('b', T0 - 40 * MIN) },
  ]
  const wake = claimWake(rows, T0, {
    inScope: (pr) => pr.number === 1,
    remote: (pr) => pr.number === 2,
  })
  assert.deepEqual(wake, {
    claimExpiresAt: T0 - 5 * MIN + CLAIM_STALE_AFTER_MS,
    remoteOrphanAt: T0 - 40 * MIN + REMOTE_ORPHAN_AFTER_MS,
  })
  assert.equal(wakeAtOf(wake), Math.min(wake.claimExpiresAt, wake.remoteOrphanAt))
  assert.equal(wakeAtOf({ claimExpiresAt: 5 }), 5)
  assert.deepEqual(claimWake([], T0, { inScope: () => true, remote: () => true }), {})
  assert.equal(wakeAtOf({}), null)
})

test('remoteClaimFields carries the claim and head only for another run’s claim', () => {
  const row = {
    pr: { number: 2, headRefOid: sha(2).toUpperCase() },
    status: claimStatus('x', T0 - 40 * MIN),
  }
  assert.deepEqual(remoteClaimFields(row, T0, REPO), {
    claim: 'stale',
    claimAt: iso(T0 - 40 * MIN),
    headSha: sha(2),
    repo: REPO,
  })
  assert.equal(
    remoteClaimFields({ ...row, status: claimStatus('x', T0) }, T0, REPO).claim,
    'foreign',
  )
  assert.deepEqual(remoteClaimFields({ ...row, status: null }, T0, REPO), {})
  assert.deepEqual(remoteClaimFields(undefined, T0, REPO), {})
})

test('postArgv passes a claim token on the verdict post', () => {
  const argv = postArgv({ pr: 3, head: sha(3), judged: JUDGED.pass, token: 'tok-9' })
  assert.equal(argv[argv.indexOf('--token') + 1], 'tok-9')
  assert.equal(postArgv({ pr: 3, head: sha(3), judged: JUDGED.pass }).includes('--token'), false)
})

test('the stored quiet entry keeps wakeAt, and a passed wake drops the fingerprint and the idle', (t) => {
  const file = join(tempDir('verify-sweep-state-', t), 'sweep-state.json')
  const wakeAt = T0 + 10 * MIN
  assert.equal(storeQuietFingerprint(file, 'k', 'fp', iso(T0), undefined, wakeAt), true)
  assert.equal(readQuietEntry(file, 'k').wakeAt, wakeAt)
  assert.equal(readQuietFingerprint(file, 'k'), 'fp')
  assert.equal(readQuietFingerprint(file, 'k', undefined, wakeAt - 1), 'fp')
  assert.equal(readQuietFingerprint(file, 'k', undefined, wakeAt), null)
  const entry = readQuietEntry(file, 'k')
  assert.equal(safetyNetIdleUntil(entry, 60 * MIN), wakeAt)
  assert.equal(safetyNetIdleUntil({ ...entry, wakeAt: 'soon' }, 60 * MIN), T0 + 60 * MIN)
  assert.equal(safetyNetIdleUntil({ at: iso(T0) }, 60 * MIN), null)
})

// ---------------------------------------------------------------------------
// The sweep.

test('a pass, human or blocked judgement on an expired claim takes it over and posts under the new token', () => {
  const finals = {
    pass: 'verified',
    human: 'needs human: partial-build',
    blocked: 'blocked: conflict',
  }
  for (const [name, judged] of Object.entries(JUDGED)) {
    const w = world({
      statuses: { 1: claimStatus('old', T0 - CLAIM_STALE_AFTER_MS - MIN) },
      verdicts: { 1: judged },
    })
    const result = sweep({}, w.deps())
    const posts = w.posts()
    assert.deepEqual(
      posts.map((p) => w.flag(p, 'verdict')),
      ['claim', judged.verdict],
      name,
    )
    const token = w.flag(posts[1], 'token')
    assert.ok(token && token !== 'old', `${name} posts under a new token`)
    assert.equal(token, tokenOf({ state: 'pending', description: `verifying… ${token}` }))
    assert.equal(w.statuses[1].description, finals[name], name)
    assert.ok(!lines(result).some((l) => l.includes('claim lost')), name)
    assert.equal(w.merges().length, name === 'pass' ? 1 : 0, name)
  }
})

test('a verdict on a head with no claim posts once, with no claim and no token', () => {
  const w = world({ verdicts: { 1: JUDGED.human } })
  sweep({}, w.deps())
  assert.deepEqual(
    w.posts().map((p) => [w.flag(p, 'verdict'), w.flag(p, 'token')]),
    [['human', null]],
  )
})

test('a takeover another run wins first leaves that live claim standing', () => {
  const w = world({ statuses: { 1: claimStatus('old', T0 - CLAIM_STALE_AFTER_MS - MIN) } })
  // Another sweep claims between discovery and this run's takeover.
  const run = w.run
  const racing = (cmd, args) => {
    if (cmd === NODE && args[1] === 'post' && args.includes('claim'))
      w.statuses[1] = claimStatus('theirs', w.clock)
    return run(cmd, args)
  }
  const result = sweep({}, w.deps({ run: racing }))
  assert.deepEqual(lines(result), ['#1 skipped: claim lost'])
  assert.equal(w.statuses[1].description, 'verifying… theirs')
  assert.equal(w.merges().length, 0)
})

test('a wait on an expired claim replaces it with waiting:, and a wait on no claim writes nothing', () => {
  const wait = { verdict: 'wait', reason: 'ci-unsettled' }
  const w = world({
    prs: [1, 2, 3],
    statuses: {
      1: claimStatus('old', T0 - CLAIM_STALE_AFTER_MS - MIN),
      3: { state: 'pending', description: 'waiting: merge-state-dirty', updatedAt: iso(T0 - MIN) },
    },
    verdicts: { 1: wait, 2: wait, 3: wait },
  })
  const result = sweep({}, w.deps())
  assert.equal(w.statuses[1].description, 'waiting: ci-unsettled')
  assert.equal(w.statuses[2], undefined, 'a head with no claim gets no write')
  assert.equal(w.statuses[3].description, 'waiting: merge-state-dirty')
  const posts = w.posts()
  assert.deepEqual(
    posts.map((p) => [w.flag(p, 'pr'), w.flag(p, 'verdict')]),
    [
      ['1', 'claim'],
      ['1', 'wait'],
    ],
  )
  assert.equal(w.flag(posts[1], 'reason'), 'ci-unsettled')
  assert.equal(
    w.flag(posts[1], 'token'),
    tokenOf({ state: 'pending', description: 'verifying… tok-1' }),
  )
  assert.deepEqual(lines(result), [
    '#1 skipped: wait ci-unsettled (replaced expired claim old)',
    '#2 skipped: wait ci-unsettled',
    '#3 skipped: wait ci-unsettled',
  ])
})

test('a dry-run sweep over an expired claim reports the takeover a real run would post and writes nothing', () => {
  const old = claimStatus('old', T0 - CLAIM_STALE_AFTER_MS - MIN)
  const wait = { verdict: 'wait', reason: 'ci-unsettled' }
  for (const [name, judged] of Object.entries({ wait, ...JUDGED })) {
    const w = world({ statuses: { 1: old }, verdicts: { 1: judged } })
    const result = sweep({ dryRun: true }, w.deps())
    assert.deepEqual(w.statuses[1], old, name)
    const posts = w.posts()
    assert.ok(posts.length > 0 && posts.every((p) => p.includes('--dry-run')), name)
    assert.deepEqual(
      posts.map((p) => w.flag(p, 'verdict')),
      ['claim', judged.verdict],
      name,
    )
    const line = lines(result).find((l) => l.startsWith('#1'))
    assert.ok(line && !/claim lost|post failed/.test(line), `${name}: ${line}`)
    if (name === 'wait')
      assert.equal(line, '#1 skipped: wait ci-unsettled (replaced expired claim old) (dry-run)')
  }

  const orphan = world({
    prs: [1, 2],
    shared: true,
    fleet: [{ id: 'remote', pr_url: url(2) }],
    statuses: { 2: claimStatus('owner', T0 - CLAIM_STALE_AFTER_MS - REMOTE_ORPHAN_AFTER_MS - MIN) },
  })
  const remote = lines(sweep({ dryRun: true }, orphan.deps())).find((l) => l.startsWith('#2'))
  assert.match(remote, new RegExp(`replaced claim owner with waiting: ${CLAIM_EXPIRED_REASON}`))
  assert.equal(orphan.statuses[2].description, 'verifying… owner')
})

test('post under --dry-run treats a token over an expired claim as held, and nothing else', async () => {
  const head = sha(7)
  const statusRead = (status) => (cmd, args) =>
    cmd === 'gh' && args.join(' ').includes(`/commits/${head}/statuses`)
      ? { ok: true, output: JSON.stringify([[{ context: 'boss/verify', ...status }]]) }
      : { ok: false, error: 'unexpected', output: '' }
  const at = (ms) => ({ state: 'pending', description: 'verifying… old', updated_at: iso(ms) })
  const waitPost = (status, extra) =>
    post(
      {
        pr: 7,
        head,
        verdict: 'wait',
        reason: 'ci-unsettled',
        token: 'tok-1',
        repo: REPO,
        ...extra,
      },
      { read: statusRead(status), now: () => T0 },
    )
  const expired = at(T0 - CLAIM_STALE_AFTER_MS - MIN)
  const dry = await waitPost(expired, { dryRun: true })
  assert.equal(dry.abandoned, undefined)
  assert.equal(dry.posted, true)
  assert.deepEqual(
    dry.writes.map((x) => x.description),
    ['waiting: ci-unsettled'],
  )
  assert.equal(
    (await waitPost(expired, {})).abandoned,
    'claim-lost',
    'a real run still needs its own claim',
  )
  const live = await waitPost(at(T0 - MIN), { dryRun: true })
  assert.equal(live.abandoned, 'claim-lost', 'another run’s live claim is lost under dry-run too')
  assert.equal((await waitPost(expired, { dryRun: true, token: '' })).abandoned, 'claim-lost')

  assert.equal(
    verdictClaimLost(claimStatus('old', T0 - CLAIM_STALE_AFTER_MS - MIN), 'tok', T0, false),
    true,
  )
  assert.equal(verdictClaimLost(claimStatus('tok', T0 - MIN), 'tok', T0, false), false)
  assert.equal(verdictClaimLost(null, undefined, T0, true), false)
})

test('an event sweep for a head with an expired claim re-claims it and settles after a pass', () => {
  const w = world({
    prs: [1, 2],
    statuses: { 2: claimStatus('old', T0 - CLAIM_STALE_AFTER_MS - MIN) },
  })
  const result = sweep({ eventPrUrl: url(2) }, w.deps())
  const posts = w.posts()
  assert.deepEqual(
    posts.map((p) => [w.flag(p, 'pr'), w.flag(p, 'verdict')]),
    [
      ['2', 'claim'],
      ['2', 'pass'],
    ],
  )
  assert.ok(!['', null, 'old'].includes(w.flag(posts[1], 'token')))
  assert.equal(w.statuses[2].description, 'verified')
  assert.equal(result.eventSettled, true)
  assert.deepEqual(
    w.merges().map((m) => m.args[m.args.indexOf('--pr') + 1]),
    ['2'],
  )
})

test('a run killed after claiming leaves no verifying… status standing past expiry', () => {
  for (const next of [
    { verdict: 'extensions-required', reason: 'extensions-installed' },
    { verdict: 'wait', reason: 'ci-unsettled' },
  ]) {
    let ticks = 0
    const w = world({
      sessions: [{ id: 's1', pr_url: url(1) }],
      verdicts: {
        1: () =>
          ticks++ === 0 ? { verdict: 'extensions-required', reason: 'extensions-installed' } : next,
      },
    })
    const first = sweep({}, w.deps())
    assert.match(lines(first)[0], /^#1 dispatched: /)
    assert.equal(w.sends.length, 1, 'the claim went to the session')
    const firstToken = tokenOf(w.statuses[1])
    assert.equal(w.statuses[1].description, `verifying… ${firstToken}`)

    // The session writes nothing; the clock passes the claim's expiry.
    w.clock += CLAIM_STALE_AFTER_MS + MIN
    w.reset()
    sweep({}, w.deps())
    assert.notEqual(
      w.statuses[1].description,
      `verifying… ${firstToken}`,
      `${next.verdict}: the first claim does not stand`,
    )
    if (next.verdict === 'wait') assert.equal(w.statuses[1].description, 'waiting: ci-unsettled')
    else assert.notEqual(tokenOf(w.statuses[1]), firstToken)
  }
})

test('a quiet entry wakes when a live claim it saw expires, inside the safety-net window', (t) => {
  const stateFile = join(tempDir('verify-stale-claim-state-', t), 'sweep-state.json')
  const claimedAt = T0 - 10 * MIN
  const expiresAt = claimedAt + CLAIM_STALE_AFTER_MS
  const w = world({ statuses: { 1: claimStatus('theirs', claimedAt) } })
  const options = { skipUnchanged: true, safetyNetMs: 60 * MIN }
  const discovered = () => w.verbs().some((v) => v[0] === 'candidates')

  const first = sweep(options, w.deps({ stateFile }))
  assert.equal(first.records.length, 0, 'a live claim is skipped as claimed')
  assert.equal(readQuietEntry(stateFile, sweepScopeKey())?.wakeAt, expiresAt)

  w.clock = expiresAt - MIN
  w.reset()
  assert.ok(sweep(options, w.deps({ stateFile })).idle, 'before expiry the tick is idle')
  assert.equal(discovered(), false)

  w.clock = expiresAt + MIN
  w.reset()
  const after = sweep(options, w.deps({ stateFile }))
  assert.ok(w.clock < T0 + 60 * MIN, 'still inside the safety-net window')
  assert.equal(after.idle, undefined)
  assert.equal(discovered(), true, 'after expiry the tick runs discovery')
  assert.equal(w.statuses[1].description, 'verified', 'and the expired claim is re-judged')
})

test('a remote-session head keeps an expired claim to its owner, then replaces it with waiting: claim-expired', () => {
  const fleet = [{ id: 'remote', pr_url: url(2) }]
  const run = (age, extra = {}) => {
    const w = world({
      prs: [1, 2],
      shared: true,
      fleet,
      statuses: { 2: claimStatus('owner', T0 - age) },
      ...extra,
    })
    return { w, result: sweep({}, w.deps()) }
  }

  const young = run(CLAIM_STALE_AFTER_MS + 15 * MIN)
  assert.ok(15 * MIN + CLAIM_STALE_AFTER_MS < REMOTE_ORPHAN_AFTER_MS)
  assert.ok(
    lines(young.result).includes(
      '#2 skipped: session on another machine (verify claim expired 15m ago)',
    ),
  )
  assert.deepEqual(
    young.w.posts().filter((p) => w2(p)),
    [],
  )
  assert.equal(young.w.statuses[2].description, 'verifying… owner')

  const orphaned = run(CLAIM_STALE_AFTER_MS + REMOTE_ORPHAN_AFTER_MS + MIN)
  assert.equal(orphaned.w.statuses[2].description, `waiting: ${CLAIM_EXPIRED_REASON}`)
  const posts = orphaned.w.posts().filter((p) => w2(p))
  assert.deepEqual(
    posts.map((p) => orphaned.w.flag(p, 'verdict')),
    ['claim', 'wait'],
  )
  assert.equal(orphaned.w.flag(posts[1], 'reason'), CLAIM_EXPIRED_REASON)
  assert.ok(orphaned.w.flag(posts[1], 'token'))
  const touched = (v) => v[0] !== 'candidates' && v.includes('2')
  assert.deepEqual(
    orphaned.w
      .verbs()
      .filter(touched)
      .map((v) => v[0]),
    ['post', 'post'],
    'no judge, dispatch or merge for a remote-owned PR',
  )
  assert.equal(orphaned.w.merges().length, 1, 'only the local PR 1 merges')
  assert.equal(orphaned.w.merges()[0].args.includes('2'), false)
  assert.equal(orphaned.w.sends.length, 0)

  // Ownership unread: the fleet read failed, or this machine acts on its own sessions only.
  const unread = world({
    prs: [1, 2],
    statuses: { 2: claimStatus('owner', T0 - REMOTE_ORPHAN_AFTER_MS - CLAIM_STALE_AFTER_MS) },
    sessions: [{ id: 's1', pr_url: url(1) }],
  })
  const local = sweep({ localSessionsOnly: true }, unread.deps())
  assert.match(
    lines(local).find((l) => l.startsWith('#2')),
    /verify claim expired/,
  )
  assert.deepEqual(
    unread.posts().filter((p) => w2(p)),
    [],
  )
  assert.equal(unread.statuses[2].description, 'verifying… owner')
})

test('under an unreadable fleet a remote-session skip writes nothing', () => {
  const w = world({
    prs: [2],
    shared: true,
    fleetFails: true,
    statuses: { 2: claimStatus('owner', T0 - REMOTE_ORPHAN_AFTER_MS - CLAIM_STALE_AFTER_MS) },
  })
  // Discovery still reports PR 2 as remote-owned (as a scan with excluded URLs would).
  const run = w.run
  const deps = w.deps({
    run: (cmd, args) =>
      cmd === NODE && args[1] === 'candidates'
        ? run(cmd, [...args, '--exclude-pr-urls', url(2)])
        : run(cmd, args),
  })
  const result = sweep({}, deps)
  assert.ok(result.fleetUnreadable)
  assert.deepEqual(w.posts(), [])
  assert.equal(w.statuses[2].description, 'verifying… owner')
})

test('the gate prints one discovery-skipped line before an unchanged summary', () => {
  const claimedAt = T0 - 5 * MIN
  const opts = {
    prs: [1, 2, 3],
    statuses: { 1: claimStatus('a', claimedAt), 2: claimStatus('b', T0 - 2 * MIN) },
    discoverySkips: [{ ticket: 'X-1', reason: 'draft', pr: 9 }],
  }
  const env = { PATH: '/usr/bin', LINEAR_API_KEY: 'k', REPO_DIR: '/repo' }
  const deps = (w) => ({ ...w.deps(), loadConfig: () => ({ adapters: { tracker: 'linear' } }) })
  const w = world(opts)
  const { lines: out } = evaluateBossVerifyGate({ argv: [], env, deps: deps(w) })
  const summary = out.at(-1)
  assert.equal(
    out.at(-2),
    `boss-verify gate: discovery skipped 3 (claimed 2 · first expires ${iso(claimedAt + CLAIM_STALE_AFTER_MS)}, draft 1)`,
  )
  assert.equal(out.filter((l) => l.includes('discovery skipped')).length, 1)
  // The same tick without the discovery skips summarises identically.
  const plain = world({ prs: [3] })
  const { lines: base } = evaluateBossVerifyGate({ argv: [], env, deps: deps(plain) })
  assert.equal(summary, base.at(-1))
  assert.equal(
    base.some((l) => l.includes('discovery skipped')),
    false,
  )
})

// ---------------------------------------------------------------------------
// The helpers against verify-gate's real `post`.

/**
 * A fake GitHub behind verify-gate's own `read` seam: one PR, open on `head`, whose latest
 * `boss/verify` status is `status`; a status POST replaces it, as GitHub's latest-status view does.
 */
function realGh(status) {
  const head = sha(7)
  const gh = { head, status, writes: 0, tokens: 0 }
  const view = { state: 'OPEN', number: 7, headRefOid: head }
  const read = (cmd, args) => {
    if (cmd !== 'gh') return { ok: false, error: 'unexpected', output: '' }
    if (args[0] === 'pr' && args[1] === 'view')
      return { ok: true, output: JSON.stringify(view), payload: view }
    if (args.includes('POST') && args.some((a) => a.endsWith(`/statuses/${head}`))) {
      const field = (k) => args.find((a) => a.startsWith(`${k}=`))?.slice(k.length + 1)
      gh.writes++
      gh.status = { state: field('state'), description: field('description'), updatedAt: iso(T0) }
      return { ok: true, output: '{}', payload: {} }
    }
    if (args.some((a) => a.includes(`/commits/${head}/statuses`))) {
      const s = gh.status && {
        context: 'boss/verify',
        ...gh.status,
        updated_at: gh.status.updatedAt,
      }
      return { ok: true, output: JSON.stringify([s ? [s] : []]) }
    }
    return { ok: false, error: 'unexpected', output: '' }
  }
  gh.deps = { read, now: () => T0, randomToken: () => `real-${++gh.tokens}` }
  return gh
}

/**
 * Runs a synchronous helper against the real, async `post`: each gate call the helper makes is
 * answered by `post` on that exact argv, replaying the answers already earned until the helper
 * makes no new call. Every `post` runs once, so the fake GitHub sees each write once.
 */
async function throughRealPost(gh, helper) {
  const answers = []
  for (;;) {
    let next = 0
    let asked = null
    const gate = (argv) => {
      if (next < answers.length) return answers[next++]
      asked ??= argv
      return { ok: false }
    }
    const out = helper(gate)
    if (!asked) return out
    answers.push({ ok: true, payload: await post(postOptionsFromArgv(asked.slice(1)), gh.deps) })
  }
}

test('a takeover never claims over a verdict another sweep posted after both saw the expired claim', async () => {
  const verified = { state: 'success', description: 'verified', updatedAt: iso(T0 - MIN) }
  // Both sweeps discovered `verifying… old` as expired; the first already posted its verdict.
  const target = { pr: 7, head: sha(7), repoArgs: ['--repo', REPO] }
  const gh = realGh(verified)
  const took = await throughRealPost(gh, (gate) =>
    claimForVerdict({ claim: 'stale' }, gate, target),
  )
  assert.equal(took, null, 'the second sweep counts the takeover as lost and posts no verdict')
  assert.equal(gh.writes, 0)
  assert.deepEqual(gh.status, verified)

  const orphan = realGh(verified)
  const s = {
    pr: 7,
    claim: 'stale',
    claimAt: iso(T0 - 3 * REMOTE_ORPHAN_AFTER_MS),
    headSha: sha(7),
    repo: REPO,
  }
  const o = await throughRealPost(orphan, (gate) =>
    remoteOrphan(s, { now: T0, gate, writable: true }),
  )
  assert.match(o.detail, /claim lost/)
  assert.equal(orphan.writes, 0, 'a non-owner never writes waiting: over the owner’s verdict')
  assert.deepEqual(orphan.status, verified)
})

test('a takeover over a claim that is still expired replaces it, through the real post', async () => {
  const expired = claimStatus('old', T0 - CLAIM_STALE_AFTER_MS - MIN)
  const target = { pr: 7, head: sha(7), repoArgs: ['--repo', REPO] }
  const gh = realGh(expired)
  const took = await throughRealPost(gh, (gate) =>
    claimForVerdict({ claim: 'stale' }, gate, target),
  )
  assert.deepEqual(took, { won: true, token: 'real-1', replaced: 'old' })
  assert.equal(gh.status.description, 'verifying… real-1')

  const orphan = realGh(expired)
  const s = {
    pr: 7,
    claim: 'stale',
    claimAt: iso(T0 - 3 * REMOTE_ORPHAN_AFTER_MS),
    headSha: sha(7),
    repo: REPO,
  }
  const o = await throughRealPost(orphan, (gate) =>
    remoteOrphan(s, { now: T0, gate, writable: true }),
  )
  assert.match(o.detail, /replaced claim old with waiting: claim-expired/)
  assert.equal(orphan.status.description, `waiting: ${CLAIM_EXPIRED_REASON}`)
})

test('orphan replacements count toward --batch, so a backlog drains a batch per tick', () => {
  const old = T0 - CLAIM_STALE_AFTER_MS - REMOTE_ORPHAN_AFTER_MS - MIN
  const w = world({
    prs: [1, 2, 3],
    shared: true,
    fleet: [
      { id: 'remote', pr_url: url(2) },
      { id: 'remote', pr_url: url(3) },
    ],
    statuses: { 2: claimStatus('two', old), 3: claimStatus('three', old) },
  })
  const result = sweep({ batch: 1 }, w.deps())
  const prOf = (p) => w.flag(p, 'pr')
  assert.deepEqual(
    w.posts().map((p) => [prOf(p), w.flag(p, 'verdict')]),
    [
      ['2', 'claim'],
      ['2', 'wait'],
    ],
  )
  assert.equal(w.statuses[2].description, `waiting: ${CLAIM_EXPIRED_REASON}`)
  assert.equal(w.statuses[3].description, 'verifying… three', 'the next tick takes the next one')
  assert.match(
    lines(result).find((l) => l.startsWith('#3')),
    /verify claim expired/,
  )
  assert.deepEqual(
    lines(result).filter((l) => l.startsWith('#1')),
    ['#1 deferred: budget'],
  )
})

test('an orphan the start floor held back never leaves the tick quiet', (t) => {
  const stateFile = join(tempDir('verify-stale-claim-held-', t), 'sweep-state.json')
  const w = world({
    prs: [2],
    shared: true,
    fleet: [{ id: 'remote', pr_url: url(2) }],
    statuses: { 2: claimStatus('owner', T0 - CLAIM_STALE_AFTER_MS - REMOTE_ORPHAN_AFTER_MS - MIN) },
  })
  const options = { skipUnchanged: true }
  sweep({ ...options, budgetMs: 0 }, w.deps({ stateFile }))
  assert.equal(w.statuses[2].description, 'verifying… owner', 'no time left: nothing written')
  assert.equal(readQuietFingerprint(stateFile, sweepScopeKey(), undefined, w.clock), null)

  w.reset()
  sweep(options, w.deps({ stateFile }))
  assert.equal(
    w.statuses[2].description,
    `waiting: ${CLAIM_EXPIRED_REASON}`,
    'the next tick replaces it',
  )
})
