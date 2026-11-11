#!/usr/bin/env node
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  AUTOMATION_GRACE,
  applyGrace,
  classifySession,
  collect,
  draftHold,
  failingChecks,
  openResidual,
  remedy,
  residualSummary,
  residualsFor,
  sharedReds,
} from './orchestrate-snapshot.mjs'

const NOW = Date.parse('2026-11-07T09:00:00Z')
const session = (over = {}) => ({
  id: 's1',
  agent: 'codex',
  state: 'READY_FOR_REVIEW',
  last_agent_activity_at: '2026-11-07T08:55:00Z',
  ...over,
})
const idleChat = { status: 'IDLE', spinner_present: false, waiting_reason: '' }
const stoppedChat = { status: 'UNSPECIFIED', spinner_present: false, waiting_reason: '' }
const liveChat = { status: 'WORKING', spinner_present: true }
const readyWaiter = {
  status: 'WAITING',
  spinner_present: false,
  waiting_reason: 'awaiting checks_passed_ready on x/y#1',
}

// `boss pr state --json`, trimmed to the fields the classifier reads.
const prState = (over = {}) => ({
  pr_number: 12,
  state: 'open',
  is_draft: false,
  is_mergeable: true,
  merge_state_status: 'clean',
  has_build_receipt: true,
  check_verdict: { state: 'green', failed_count: 0, pending_count: 0 },
  verify_phase: { kind: 'none', reason: '', label: '' },
  checks: [{ name: 'boss/verify', conclusion: 'success' }],
  ...over,
})
const parked = (reason, label = '') => ({ verify_phase: { kind: 'needs_human', reason, label } })
const classify = (s, chats, pr) => classifySession({ session: session(s), chats, pr, now: NOW })

test('a ready PR whose head verify passed is done', () => {
  assert.equal(classify({}, [stoppedChat], prState()).action, 'done')
})

test('a policy park is verified as far as policy allows and waits on the operator', () => {
  for (const reason of [
    'always-human-path,human-review:ui',
    'human-review:infra',
    'human-review:database,human-review:security,human-review:asks',
  ]) {
    const pr = prState(parked(reason, 'moderate: Infrastructure'))
    assert.equal(classify({}, [stoppedChat], pr).action, 'awaiting-operator', reason)
  }
})

test('a deliberate partial build waits on the operator, since a fresh head would park again', () => {
  for (const reason of ['partial-build', 'partial-build,human-review:ui']) {
    const verdict = classify({}, [stoppedChat], prState(parked(reason, 'partial 1/8')))
    assert.equal(verdict.action, 'awaiting-operator', reason)
  }
  // Beside a mechanical code it still needs that code cleared first.
  const mixed = prState(parked('partial-build,repair-exhausted'))
  assert.equal(classify({}, [stoppedChat], mixed).action, 'refresh-park')
})

test('a repair-exhausted park whose checks now read green is stale and needs a fresh head', () => {
  const pr = prState(parked('repair-exhausted', 'moderate: API contract (proto)'))
  assert.equal(classify({}, [stoppedChat, stoppedChat], pr).action, 'refresh-park')
})

test('a mechanical park waits while its checks are still running', () => {
  const pr = prState({
    ...parked('repair-exhausted'),
    check_verdict: { state: 'pending', pending_count: 2 },
  })
  assert.equal(classify({}, [stoppedChat], pr).action, 'wait-ci')
})

test('a mechanical park with a live repair chat is left to that chat', () => {
  assert.equal(classify({}, [liveChat], prState(parked('repair-exhausted'))).action, 'working')
})

test('a blocked session whose PR was already handed off is judged by its PR', () => {
  const verdict = classify(
    {
      state: 'BLOCKED',
      blocked_reason: 'finalize failed (pr_failed): worktree has uncommitted tracked changes',
    },
    [stoppedChat],
    prState(parked('human-review:database', 'low: bossd database migration')),
  )
  assert.equal(verdict.action, 'awaiting-operator')
})

test('a blocked session reports its cap so the operator can waive it', () => {
  const verdict = classify(
    {
      state: 'BLOCKED',
      blocked_reason: 'fresh review required after boss-build two re-review cap.',
    },
    [idleChat],
    prState({ is_draft: true, has_build_receipt: false }),
  )
  assert.equal(verdict.action, 'waive-cap')
})

test('a blocked session whose chat is already working is left alone', () => {
  assert.equal(
    classify({ state: 'BLOCKED', blocked_reason: 're-review cap' }, [liveChat], null).action,
    'working',
  )
})

test('a conflicting PR with no live chat needs repair', () => {
  const pr = prState({ merge_state_status: 'dirty', is_mergeable: false })
  assert.equal(classify({}, [stoppedChat], pr).action, 'repair')
})

test('failing checks with no live chat need repair', () => {
  const pr = prState({ check_verdict: { state: 'failing', failed_count: 3 } })
  assert.equal(classify({}, [stoppedChat], pr).action, 'repair')
})

test('a ready, green PR with no verify status is routed to verify', () => {
  assert.equal(classify({}, [stoppedChat], prState({ checks: [] })).action, 'verify')
})

test('a verify claim that has gone quiet with no live chat is re-run', () => {
  const pr = prState({ checks: [], verify_phase: { kind: 'verifying', reason: '', label: '' } })
  assert.equal(
    classify({ last_agent_activity_at: '2026-11-07T07:00:00Z' }, [stoppedChat], pr).action,
    'verify',
  )
})

test('a session whose PR has merged is stale', () => {
  assert.equal(classify({}, [stoppedChat], prState({ state: 'merged' })).action, 'stale-session')
})

test('a session with no PR, no live chat and no activity for an hour is stalled', () => {
  const verdict = classify(
    { state: 'IMPLEMENTING_PLAN', last_agent_activity_at: '2026-11-07T07:00:00Z' },
    [stoppedChat],
    null,
  )
  assert.equal(verdict.action, 'stalled')
})

test('a draft that verify converted and repair then fixed is marked ready', () => {
  const hold = draftHold([
    {
      __typename: 'IssueComment',
      createdAt: '2026-11-06T09:25:43Z',
      body: '### Verification found 4 blocker(s) on `b13`',
    },
    { __typename: 'ConvertToDraftEvent', createdAt: '2026-11-06T09:25:45Z' },
    {
      __typename: 'IssueComment',
      createdAt: '2026-11-06T11:36:19Z',
      body: '### Repair blocker outcomes\n- fixed',
    },
  ])
  assert.deepEqual(hold, { holder: 'verify', repaired: true })
  const pr = prState({ is_draft: true, checks: [], draftHold: hold })
  assert.equal(classify({ state: 'GREEN_DRAFT' }, [idleChat, readyWaiter], pr).action, 'mark-ready')
})

test('a draft converted without a verify blockers comment is left for the operator', () => {
  const hold = draftHold([{ __typename: 'ConvertToDraftEvent', createdAt: '2026-11-06T09:25:45Z' }])
  assert.equal(hold.holder, 'operator')
  const pr = prState({ is_draft: true, checks: [], draftHold: hold })
  assert.equal(classify({ state: 'GREEN_DRAFT' }, [readyWaiter], pr).action, 'ask-operator')
})

test('the remedy opens a chat inside the existing session, never a new session', () => {
  for (const action of ['waive-cap', 'repair', 'refresh-park', 'verify']) {
    const next = remedy({ action, why: 'x' }, { host: 'build-host', session: session(), pr: 12 })
    assert.match(next, /^boss --host build-host chat new s1 --role (repair|verify) /, action)
    assert.doesNotMatch(next, /boss new\b/, action)
  }
})

test('the remedy invokes skills in the session agent’s syntax', () => {
  const codex = remedy(
    { action: 'verify', why: '' },
    { host: '', session: session({ agent: 'codex' }), pr: 1 },
  )
  const claude = remedy(
    { action: 'verify', why: '' },
    { host: '', session: session({ agent: 'claude' }), pr: 1 },
  )
  assert.match(codex, /\$boss-verify 1/)
  assert.match(claude, /\/boss-verify 1/)
})

test('a run blocked on a red inherited from main is routed to an upstream fix', () => {
  const verdict = classify(
    {
      state: 'BLOCKED',
      blocked_reason:
        'run blocked: T-7: inherited base-only-lint validation failure reproduced on base; PR 7 draft',
    },
    [idleChat],
    prState({ is_draft: true, has_build_receipt: false, checks: [] }),
  )
  assert.equal(verdict.action, 'upstream-red')
  assert.match(
    remedy(verdict, { host: 'build-host', session: session(), pr: 7 }),
    /chat new s1 --role repair/,
  )
})

test('a check failing on two PRs of one repo is one shared upstream red', () => {
  const failing = prState({
    checks: [
      { name: 'base-only-lint', conclusion: 'failure' },
      { name: 'boss/verify', conclusion: 'failure' },
    ],
  })
  assert.deepEqual(failingChecks(failing), ['base-only-lint'])
  const rows = [
    {
      repo: 'acme/app',
      pr: 1,
      session: 'a',
      host: 'build-host',
      action: 'repair',
      failing: ['base-only-lint'],
    },
    {
      repo: 'acme/app',
      pr: 2,
      session: 'b',
      host: 'build-host',
      action: 'repair',
      failing: ['base-only-lint', 'api'],
    },
    {
      repo: 'other/x',
      pr: 3,
      session: 'c',
      host: 'build-host',
      action: 'repair',
      failing: ['base-only-lint'],
    },
  ]
  const shared = sharedReds(rows)
  assert.equal(shared.length, 1)
  assert.equal(shared[0].check, 'base-only-lint')
  assert.deepEqual(
    shared[0].prs.map((p) => p.pr),
    [1, 2],
  )
})

// A repo root holding bs-orchestrate extensions. `scripts` maps a file name to its source.
function extensionRoot(t, extensions) {
  const root = tempDir('orchestrate-ext-', t)
  for (const { name, role, script, source } of extensions) {
    const dir = path.join(root, '.claude', 'skills', name)
    fs.mkdirSync(dir, { recursive: true })
    const marker = ['x-boss-extension:', '  extends: bs-orchestrate', `  role: ${role}`]
    if (script) marker.push(`  script: ${script}`)
    fs.writeFileSync(
      path.join(dir, 'SKILL.md'),
      ['---', ...marker, '---', '', `# ${name}`, ''].join('\n'),
    )
    if (script && source !== undefined) fs.writeFileSync(path.join(dir, script), source)
  }
  return root
}

// Reads the envelope from stdin and prints `respond(envelope)` as the result.
const scriptOf = (respond) =>
  `const envelope = JSON.parse(require('node:fs').readFileSync(0, 'utf8'))\n` +
  `process.stdout.write(JSON.stringify((${respond})(envelope)))\n`

// The failing-checks PR state the residual and repair tests share.
const failingPr = (over = {}) =>
  prState({
    check_verdict: { state: 'failing', failed_count: 1, pending_count: 0 },
    checks: [{ name: 'lint', conclusion: 'failure' }],
    ...over,
  })

// Two sessions on this machine: s1's PR is verified, s2's has failing checks and no live chat.
const fakeDump = () => ({
  sessions: [
    { session: session({ id: 's1' }), chats: [stoppedChat], pr: prState(), repo: 'acme/app' },
    {
      session: session({ id: 's2', agent: 'claude' }),
      chats: [stoppedChat],
      pr: failingPr({ pr_number: 13 }),
      repo: 'acme/app',
    },
  ],
  residuals: [],
})
const snapshot = (root, over = {}) =>
  collect({
    hosts: [],
    local: true,
    bossBin: 'boss',
    now: NOW,
    root,
    dump: fakeDump,
    ...over,
  })

test('with no extensions the snapshot is the built-in classification', (t) => {
  const result = snapshot(extensionRoot(t, []))
  assert.deepEqual(
    result.rows.map((row) => [row.session, row.action]),
    [
      ['s1', 'done'],
      ['s2', 'repair'],
    ],
  )
  assert.deepEqual(result.ledger, [])
  assert.deepEqual(result.operatorItems, [])
  assert.deepEqual(result.rules, [])
})

test('an orchestrate-classify extension overrides the rows it names', (t) => {
  const root = extensionRoot(t, [
    {
      name: 'bs-orchestrate-freeze',
      role: 'orchestrate-classify',
      script: 'classify.js',
      // Holds every PR that needs repair, and sends the verified one back to repair.
      source: scriptOf(`(env) => ({
        ok: true, extension: env.extension, role: env.role,
        overrides: env.rows.map((row) => row.action === 'repair'
          ? { host: row.host, session: row.session, action: 'hold', why: 'release freeze', next: '' }
          : { host: row.host, session: row.session, action: 'repair', why: 'recheck after freeze' }),
      })`),
    },
  ])
  const rows = Object.fromEntries(snapshot(root).rows.map((row) => [row.session, row]))
  assert.equal(rows.s2.action, 'hold')
  assert.equal(rows.s2.why, 'release freeze')
  assert.equal(rows.s2.next, '')
  assert.equal(rows.s2.classifiedBy, 'bs-orchestrate-freeze')
  // A new built-in action without `next` gets the built-in command for that action.
  assert.equal(rows.s1.action, 'repair')
  assert.match(rows.s1.next, /chat new s1 --role repair/)
  assert.match(rows.s1.next, /\$boss-repair 12 \(recheck after freeze\)/)
})

test('later classify extensions see earlier overrides; overrides for unknown rows are ledgered', (t) => {
  const root = extensionRoot(t, [
    {
      name: 'bs-orchestrate-a',
      role: 'orchestrate-classify',
      script: 'a.js',
      source: scriptOf(`(env) => ({ ok: true, extension: env.extension, role: env.role,
        overrides: [{ host: 'local', session: 's1', action: 'hold', why: 'first' },
                    { host: 'local', session: 'gone', action: 'hold', why: 'stale' }] })`),
    },
    {
      name: 'bs-orchestrate-b',
      role: 'orchestrate-classify',
      script: 'b.js',
      source: scriptOf(`(env) => ({ ok: true, extension: env.extension, role: env.role,
        overrides: env.rows.filter((row) => row.action === 'hold')
          .map((row) => ({ host: row.host, session: row.session, action: 'ask-operator', why: 'saw ' + row.why })) })`),
    },
  ])
  const result = snapshot(root)
  const s1 = result.rows.find((row) => row.session === 's1')
  assert.equal(s1.action, 'ask-operator')
  assert.equal(s1.why, 'saw first')
  assert.equal(s1.classifiedBy, 'bs-orchestrate-b')
  assert.deepEqual(result.ledger, [
    'extension bs-orchestrate-a: override for local gone matches no row',
  ])
})

test('a broken or misdeclared extension is skipped with a reason and changes nothing', (t) => {
  const reply = (body) => scriptOf(`(env) => (${body})`)
  const root = extensionRoot(t, [
    {
      name: 'bs-orchestrate-crash',
      role: 'orchestrate-classify',
      script: 'x.js',
      source: 'process.exit(3)\n',
    },
    {
      name: 'bs-orchestrate-prose',
      role: 'orchestrate-classify',
      script: 'x.js',
      source: 'console.log("hi")\n',
    },
    {
      name: 'bs-orchestrate-shape',
      role: 'orchestrate-classify',
      script: 'x.js',
      source: reply(
        `{ ok: true, extension: env.extension, role: env.role, overrides: [{ host: 'local', session: 's1', action: 'Hold!', why: 'x' }] }`,
      ),
    },
    {
      name: 'bs-orchestrate-hang',
      role: 'orchestrate-round',
      script: 'x.js',
      source: 'setInterval(() => {}, 1000)\n',
    },
    { name: 'bs-orchestrate-absent', role: 'orchestrate-round', script: 'nowhere.js' },
    { name: 'bs-orchestrate-escape', role: 'orchestrate-round', script: '../x.js' },
  ])
  const result = snapshot(root, { timeoutMs: 1000 })
  assert.deepEqual(
    result.rows.map((row) => [row.session, row.action, row.classifiedBy]),
    [
      ['s1', 'done', undefined],
      ['s2', 'repair', undefined],
    ],
  )
  assert.deepEqual(result.operatorItems, [])
  assert.deepEqual([...result.ledger].sort(), [
    'extension bs-orchestrate-absent: skipped (script-missing)',
    'extension bs-orchestrate-crash: skipped (exit:3)',
    'extension bs-orchestrate-escape: skipped (invalid "script" (expected a path inside the extension directory with no ".." segment))',
    'extension bs-orchestrate-hang: skipped (timeout)',
    'extension bs-orchestrate-prose: skipped (not-json)',
    'extension bs-orchestrate-shape: skipped (invalid: override 0 "action" must be a kebab-case name)',
  ])
})

test('orchestrate-round items reach the operator list; rules extensions are listed to read', (t) => {
  const root = extensionRoot(t, [
    { name: 'bs-orchestrate-house', role: 'orchestrate-rules' },
    {
      name: 'bs-orchestrate-audit',
      role: 'orchestrate-round',
      script: 'audit.js',
      source: scriptOf(`(env) => ({ ok: true, extension: env.extension, role: env.role,
        items: [{ title: env.rows.length + ' sessions', detail: env.sharedReds.length + ' shared reds' }] })`),
    },
  ])
  const result = snapshot(root)
  assert.deepEqual(result.operatorItems, [
    { extension: 'bs-orchestrate-audit', title: '2 sessions', detail: '0 shared reds' },
  ])
  assert.deepEqual(result.rules, [
    {
      name: 'bs-orchestrate-house',
      skillPath: path.join(root, '.claude', 'skills', 'bs-orchestrate-house', 'SKILL.md'),
    },
  ])
  assert.deepEqual(result.ledger, [])
})

test('a conflict that bossd is already repairing is left to that repair', () => {
  const pr = prState({ merge_state_status: 'dirty', is_mergeable: false })
  // Started moments ago: its repair chat has not appeared yet.
  const s = { repair_active: true, last_repair: { started_at: '2026-11-07T08:55:00Z' } }
  assert.equal(classify(s, [stoppedChat], pr).action, 'working')
  // Its repair chat is up and working.
  const early = { repair_active: true, last_repair: { started_at: '2026-11-07T08:20:00Z' } }
  const repairChat = { ...liveChat, role: 'repair', created_at: '2026-11-07T08:20:05Z' }
  assert.equal(classify(early, [repairChat], pr).action, 'working')
})

test('a finished bossd repair whose flag lags routes the failing check on', () => {
  const s = { repair_active: true, last_repair: { started_at: '2026-11-07T08:20:00Z' } }
  const finished = {
    ...idleChat,
    role: 'repair',
    title: 'repair: checks',
    created_at: '2026-11-07T08:20:05Z',
  }
  // The idle repair chat created after the start means that repair finished.
  assert.equal(classify(s, [finished], failingPr()).action, 'repair')
  // With no repair chat at all, the flag counts only for the startup window.
  assert.equal(classify(s, [stoppedChat], failingPr()).action, 'repair')
})

// A repair-residual note as `boss notes ls --tag repair-residual --json` lists it.
const residualNote = (over = {}) => ({
  id: 'n1',
  repo_id: 'r1',
  session_id: 's2',
  created_at: '2026-11-07T08:40:00Z',
  body: 'PR #13: lint fails on main too; inherited from the base, not this branch.\nDetails...',
  ...over,
})

test('an open inherited-red residual turns a failing PR into an upstream fix', (t) => {
  const dump = () => ({
    ...fakeDump(),
    residuals: [
      residualNote(),
      // A residual on another session that cites no PR of this one is not its.
      residualNote({ id: 'n2', session_id: 'zz', body: 'PR #99: fails on main' }),
    ],
  })
  const result = snapshot(extensionRoot(t, []), { dump })
  const s2 = result.rows.find((row) => row.session === 's2')
  assert.equal(s2.action, 'upstream-red')
  assert.match(s2.why, /^repair residual n1: PR #13: lint fails on main too/)
  assert.match(s2.next, /hold.*Otherwise fix the cause on the base yourself/)
  assert.deepEqual(s2.residual, { id: 'n1', summary: residualSummary(residualNote()) })
  assert.equal(result.rows.find((row) => row.session === 's1').action, 'done')
})

test('a residual names a session by its PR in the same repo, never another repo', () => {
  const own = session({ id: 's2', repo_id: 'r1' })
  const cited = residualNote({ session_id: 'other' })
  assert.deepEqual(residualsFor(own, 13, [cited]), [cited])
  assert.deepEqual(residualsFor(own, 13, [{ ...cited, repo_id: 'r2' }]), [])
  assert.deepEqual(residualsFor(own, 14, [cited]), [])
})

test('a residual stops counting once superseded or declined, and yields to a live chat', () => {
  const note = residualNote()
  // A later bossd repair that recorded no newer residual supersedes it.
  const later = { last_repair: { started_at: '2026-11-07T08:45:00Z' } }
  assert.equal(openResidual(session(later), [note], ['lint']), null)
  assert.equal(openResidual(session(), [note], ['lint']), note)
  // It must name a check failing now: a defect found on the base, or another red, is not this one.
  assert.equal(openResidual(session(), [note], ['api-tests']), null)
  assert.equal(openResidual(session(), [note], []), null)
  // A residual that records something other than a red on the base is not an upstream red.
  const declined = residualNote({ body: 'PR #13: lint finding 2 declined: out of scope' })
  assert.equal(openResidual(session(), [declined], ['lint']), null)
  const residual = openResidual(session(), [note], ['lint'])
  assert.ok(residual)
  const verdict = classifySession({
    session: session(),
    chats: [liveChat],
    pr: failingPr(),
    residual,
    now: NOW,
  })
  assert.equal(verdict.action, 'working')
})

test('residuals mark a shared red as inherited from the base', () => {
  const row = (pr, sessionId, over = {}) => ({
    repo: 'acme/app',
    pr,
    session: sessionId,
    host: 'local',
    action: 'repair',
    failing: ['lint'],
    ...over,
  })
  const [plain] = sharedReds([row(1, 'a'), row(2, 'b')])
  assert.equal(plain.inherited, false)
  const [group] = sharedReds([
    row(1, 'a', { action: 'upstream-red', residual: { id: 'n1', summary: 'x' } }),
    row(2, 'b'),
  ])
  assert.equal(group.inherited, true)
  assert.deepEqual(
    group.prs.map((p) => [p.pr, p.residual ?? '']),
    [
      [1, 'n1'],
      [2, ''],
    ],
  )
})

test('a repair flag that never cleared does not hide a stalled conflict', () => {
  const pr = prState({ merge_state_status: 'dirty', is_mergeable: false })
  const s = { repair_active: true, last_repair: { started_at: '2026-11-05T22:19:20Z' } }
  assert.equal(classify(s, [stoppedChat], pr).action, 'repair')
})

test('a fresh conflict waits for the factory before the orchestrator repairs it', () => {
  const seen = {}
  const row = () => ({
    host: 'h',
    session: 's1',
    action: 'repair',
    why: 'merge conflict',
    next: 'x',
    autoRepair: true,
  })
  const first = applyGrace([row()], seen, NOW)
  assert.equal(first[0].action, 'grace')
  assert.equal(first[0].next, '')
  const later = applyGrace([row()], seen, NOW + AUTOMATION_GRACE)
  assert.equal(later[0].action, 'repair')
})

test('a repo with auto-repair off gets no repair grace', () => {
  const rows = applyGrace(
    [{ host: 'h', session: 's1', action: 'repair', why: '', autoRepair: false }],
    {},
    NOW,
  )
  assert.equal(rows[0].action, 'repair')
})

test('a missing verify also waits for the verify cron', () => {
  const rows = applyGrace([{ host: 'h', session: 's1', action: 'verify', why: '' }], {}, NOW)
  assert.equal(rows[0].action, 'grace')
})

test('a row that cleared restarts its grace if it comes back', () => {
  const seen = {}
  const row = () => ({ host: 'h', session: 's1', action: 'repair', why: '', autoRepair: true })
  applyGrace([row()], seen, NOW)
  applyGrace([], seen, NOW + 5 * 60 * 1000)
  assert.equal(applyGrace([row()], seen, NOW + AUTOMATION_GRACE)[0].action, 'grace')
})

test('an unreachable host is reported and keeps its grace clocks across rounds', (t) => {
  const down = (host) => {
    if (host) throw new Error('ssh: connect to host build-host: Connection refused')
    return fakeDump()
  }
  const seen = { 'build-host/s9/repair': NOW - 10 * 60 * 1000 }
  const result = snapshot(extensionRoot(t, []), { hosts: ['build-host'], dump: down, seen })
  assert.deepEqual(result.unread, ['build-host'])
  assert.match(result.errors.join('\n'), /build-host: ssh: connect to host build-host/)
  assert.ok('build-host/s9/repair' in result.seen)
  // Once the host reads again and the row is gone, its clock goes too.
  const back = snapshot(extensionRoot(t, []), {
    hosts: ['build-host'],
    dump: () => ({ sessions: [], residuals: [] }),
    seen,
  })
  assert.deepEqual(back.unread, [])
  assert.equal('build-host/s9/repair' in back.seen, false)
})

// A chat as `boss chats --json` lists it: idle, last output at 08:22.
const chatRow = (over = {}) => ({
  agent_session_id: 'c-new',
  title: 'build',
  role: 'build',
  created_at: '2026-11-07T08:00:00Z',
  status: 'IDLE',
  spinner_present: false,
  waiting_reason: '',
  last_output_at: '2026-11-07T08:22:00Z',
  last_substantive_output_at: '2026-11-07T08:22:00Z',
  ...over,
})
const olderChat = chatRow({
  agent_session_id: 'c-old',
  created_at: '2026-11-07T06:00:00Z',
  last_output_at: '2026-11-07T06:30:00Z',
  last_substantive_output_at: '2026-11-07T06:30:00Z',
})
const building = { state: 'IMPLEMENTING_PLAN', last_agent_activity_at: '2026-11-07T08:50:00Z' }
const unreceipted = prState({
  has_build_receipt: false,
  checks: [],
  check_verdict: { state: 'pending', pending_count: 2 },
})

test('an in-progress session whose newest chat sat idle with nothing armed gets a nudge', () => {
  for (const pr of [null, unreceipted]) {
    const verdict = classify(building, [olderChat, chatRow()], pr)
    assert.equal(verdict.action, 'stalled-chat', pr ? 'unreceipted PR' : 'no PR')
    assert.equal(verdict.chat, 'c-new')
    const next = remedy(verdict, { host: 'build-host', session: session(), pr: pr?.pr_number })
    assert.match(next, /^boss --host build-host chat send --json c-new '/)
    assert.match(next, /continue toward the hand-off/)
    assert.match(next, /Never wait on a monitor/)
  }
})

test('a working chat, an armed callback or a recent output is not a stuck chat', () => {
  const working = chatRow({ status: 'WORKING', spinner_present: true })
  assert.equal(classify(building, [olderChat, working], null).action, 'working')
  // A live older chat counts too, even when the newest is idle.
  const liveOld = { ...olderChat, status: 'WORKING' }
  assert.equal(classify(building, [liveOld, chatRow()], null).action, 'working')
  for (const armed of [
    chatRow({ status: 'WAITING', waiting_reason: 'awaiting checks_passed on x/y#1' }),
    chatRow({ waiting_reason: 'awaiting merged on x/y#1' }),
  ])
    assert.notEqual(classify(building, [olderChat, armed], null).action, 'stalled-chat')
  // The operator's own conversation, such as an attended plan chat, waits on them.
  for (const role of ['operator', 'coordinator'])
    assert.notEqual(classify(building, [chatRow({ role })], null).action, 'stalled-chat', role)
  // A chat held by a usage limit is not idle: a nudge cannot move it.
  const limited = chatRow({ status: 'LIMITED' })
  assert.notEqual(classify(building, [olderChat, limited], null).action, 'stalled-chat')
  const recent = chatRow({
    last_output_at: '2026-11-07T08:55:00Z',
    last_substantive_output_at: '2026-11-07T08:55:00Z',
  })
  assert.equal(classify(building, [olderChat, recent], null).action, 'ok')
})

test('idle chats on a finished or operator-parked PR are not stuck', () => {
  const quiet = [olderChat, chatRow()]
  assert.equal(classify({}, quiet, prState()).action, 'done')
  assert.equal(classify({}, quiet, prState(parked('human-review:ui'))).action, 'awaiting-operator')
  // Handed off with checks still running: verify takes it from here.
  const handedOff = prState({ checks: [], check_verdict: { state: 'pending', pending_count: 1 } })
  assert.equal(classify({}, quiet, handedOff).action, 'wait-ci')
  assert.equal(classify({ state: 'MERGED' }, quiet, null).action, 'skip')
})

test('a chat showing a dialog is read, then approved or nudged', () => {
  const asking = chatRow({ agent_session_id: 'c-ask', status: 'QUESTION' })
  // Even beside a working chat: the dialog holds its own chat until someone answers it.
  const verdict = classify(building, [liveChat, asking], prState())
  assert.equal(verdict.action, 'question-chat')
  assert.equal(verdict.chat, 'c-ask')
  const next = remedy(verdict, { host: 'build-host', session: session(), pr: 12 })
  assert.match(next, /orchestrate-snapshot\.mjs pane --chat c-ask --host build-host /)
  assert.match(next, /pane --chat c-ask --host build-host --approve/)
  assert.match(next, /boss --host build-host chat send --json c-ask '/)
  // The orchestrator's own question chats wait on the operator on purpose.
  const own = chatRow({ status: 'QUESTION', title: 'question: q1 · release' })
  assert.notEqual(classify(building, [liveChat, own], null).action, 'question-chat')
})

test('a run blocked on an infrastructure red has its cap waived and finishes the hand-off', () => {
  for (const reason of [
    'scripts CI exhausted five attempts downloading golangci-lint with HTTP 504',
    'go-lint: curl: (22) The requested URL returned error: 502; curl exit code 22 after 3 retries',
    'build-receipt: GitHub API returned 503 Service Unavailable',
    'e2e job failed: The self-hosted runner lost communication with the server.',
  ]) {
    const verdict = classify({ state: 'BLOCKED', blocked_reason: reason }, [idleChat], unreceipted)
    assert.equal(verdict.action, 'waive-cap', reason)
    const next = remedy(verdict, { host: '', session: session({ agent: 'claude' }), pr: 12 })
    assert.match(next, /infrastructure/, reason)
    assert.match(next, /gh run rerun <run-id> --failed/, reason)
    assert.match(next, /finish the boss-build hand-off/, reason)
  }
  // A cap on review rounds is still a plain cap, and a red from the base is still upstream.
  const blocked = (reason) =>
    classify({ state: 'BLOCKED', blocked_reason: reason }, [idleChat], unreceipted)
  const cap = blocked('re-review cap')
  assert.equal(cap.action, 'waive-cap')
  assert.doesNotMatch(remedy(cap, { host: '', session: session(), pr: 12 }), /infrastructure/)
  assert.equal(blocked('lint fails on main too').action, 'upstream-red')
})

test('a message quoting an apostrophe stays one shell argument', () => {
  const next = remedy(
    { action: 'waive-cap', why: "the runner's disk filled", cause: 'infra' },
    { host: '', session: session(), pr: 1 },
  )
  assert.match(next, /runner'\\''s disk/)
})

test('an outage record holds its sessions and marks their shared red as infrastructure', (t) => {
  const stateDir = tempDir('orchestrate-state-', t)
  const infraPr = (pr_number) =>
    failingPr({
      pr_number,
      checks: [{ name: 'lint', conclusion: 'failure', description: 'curl exit code 22: HTTP 504' }],
    })
  const dump = () => ({
    sessions: [
      { session: session({ id: 's1' }), chats: [stoppedChat], pr: infraPr(12), repo: 'acme/app' },
      { session: session({ id: 's2' }), chats: [stoppedChat], pr: infraPr(13), repo: 'acme/app' },
    ],
    residuals: [],
  })
  const plain = snapshot(extensionRoot(t, []), { dump })
  assert.equal(plain.sharedReds[0].infra, true)
  assert.equal(plain.outage, null)
  const outage = { reason: 'CI outage', since: 'x', crons: [], runs: [], probes: [] }
  fs.writeFileSync(
    path.join(stateDir, 'outage.json'),
    JSON.stringify({ ...outage, sessions: [{ host: '', session: 's1' }] }),
  )
  const held = snapshot(extensionRoot(t, []), { dump, stateDir })
  assert.deepEqual(
    held.rows.map((row) => [row.session, row.action]),
    [
      ['s1', 'held'],
      ['s2', 'repair'],
    ],
  )
  assert.equal(held.outage.reason, 'CI outage')
})

// A cron fire's session before finalize: no PR, on its cron-<job>-<unix> branch.
const cronRun = { ...building, branch: 'cron-bossanova-auto-build-1762500000' }
const quietSince = (at) => chatRow({ last_output_at: at, last_substantive_output_at: at })

test('a cron run that ended its turn with no PR is left for bossd to finalize, not nudged', () => {
  // Linked by its cron job's last run, or by its branch alone.
  for (const [s, cronJob] of [
    [building, { id: 'cron-1', name: 'auto build' }],
    [cronRun, null],
  ]) {
    const verdict = classifySession({
      session: session(s),
      chats: [olderChat, chatRow()],
      pr: null,
      cronJob,
      now: NOW,
    })
    assert.equal(verdict.action, 'finalizing', cronJob ? 'cron link' : 'cron branch')
    assert.equal(remedy(verdict, { host: '', session: session(s) }), '')
  }
  // Quiet past the session-level stall too: still bossd's within the finalize wait.
  const quietHour = { ...cronRun, last_agent_activity_at: '2026-11-07T08:05:00Z' }
  assert.equal(classify(quietHour, [quietSince('2026-11-07T08:05:00Z')], null).action, 'finalizing')
})

test('a cron run is still flagged when it is working, armed, or its finalize never came', () => {
  assert.equal(classify(cronRun, [chatRow({ status: 'WORKING' })], null).action, 'working')
  const armed = chatRow({ waiting_reason: 'awaiting merged on x/y#1' })
  assert.notEqual(classify(cronRun, [armed], null).action, 'finalizing')
  // An hour after its turn ended the run is still listed: bossd's finalize did not take it.
  const stuck = classify(cronRun, [olderChat, quietSince('2026-11-07T07:50:00Z')], null)
  assert.equal(stuck.action, 'stalled-chat')
  // A build that is not a cron run and went idle mid-work is still nudged.
  const named = { ...building, branch: 'bos-123-add-thing' }
  assert.equal(classify(named, [olderChat, chatRow()], null).action, 'stalled-chat')
})

test('a dependabot PR is never routed to verify; any other ready PR without verify is', () => {
  const unverified = { checks: [], has_build_receipt: false }
  const bump = prState({ ...unverified, head_branch: 'dependabot/npm_and_yarn/vite-7.1.2' })
  const verdict = classify({}, [stoppedChat], bump)
  assert.equal(verdict.action, 'dependabot')
  assert.equal(remedy(verdict, { host: '', session: session(), pr: 12 }), '')
  // No grace clock either: nothing is waiting on the factory's verify.
  const rows = [{ host: 'local', session: 's1', ...verdict }]
  applyGrace(rows, {}, NOW)
  assert.equal(rows[0].action, 'dependabot')
  // The session's branch decides when the PR state carries no head branch.
  const sessionBranch = { branch: 'dependabot/go_modules/x-1.2' }
  assert.equal(classify(sessionBranch, [stoppedChat], prState(unverified)).action, 'dependabot')
  const feature = prState({ ...unverified, head_branch: 'bos-123-add-thing' })
  assert.equal(classify({}, [stoppedChat], feature).action, 'verify')
})

// A fake `boss` for the real daemon dump: its cron list names s1 as a job's latest run.
const FAKE_BOSS = `#!/usr/bin/env node
const [cmd, , id] = process.argv.slice(2)
const chat = { agent_session_id: 'c-' + id, role: 'build', created_at: '2026-11-07T08:00:00Z',
  status: 'IDLE', spinner_present: false, waiting_reason: '',
  last_output_at: '2026-11-07T08:30:00Z', last_substantive_output_at: '2026-11-07T08:30:00Z' }
const out = {
  cron: [{ id: 'job-1', name: 'auto build', last_run_session_id: 's1' }],
  ls: { sessions: [{ id: 's1', state: 'IMPLEMENTING_PLAN' }, { id: 's2', state: 'IMPLEMENTING_PLAN' }] },
  chats: { chats: [chat] },
  show: { session: { id, state: 'IMPLEMENTING_PLAN', branch: 'bos-1-' + id,
    last_agent_activity_at: '2026-11-07T08:30:00Z' } },
  notes: [],
}[cmd]
process.stdout.write(JSON.stringify(out))
`

test('the daemon dump links a cron job’s latest run, so only the other idle build is nudged', (t) => {
  const dir = tempDir('orchestrate-boss-', t)
  const boss = path.join(dir, 'boss')
  fs.writeFileSync(boss, FAKE_BOSS, { mode: 0o755 })
  const result = collect({ hosts: [], local: true, bossBin: boss, now: NOW, root: dir })
  assert.deepEqual(result.errors, [])
  assert.deepEqual(result.rows.map((row) => [row.session, row.action]).sort(), [
    ['s1', 'finalizing'],
    ['s2', 'stalled-chat'],
  ])
})

// A plan cron run as `boss ls`/`boss show` report it: titled with its job's name, no branch, no PR.
// A later fire of the job (allowed to overlap) took the job's last-run pointer.
const FAKE_PLAN_BOSS = `#!/usr/bin/env node
const [cmd, , id] = process.argv.slice(2)
const chat = { agent_session_id: 'c-' + id, title: 'build', role: 'build',
  created_at: '2026-11-07T08:00:00Z', status: 'IDLE', spinner_present: false, waiting_reason: '',
  last_output_at: '2026-11-07T08:48:00Z', last_substantive_output_at: '2026-11-07T08:48:00Z' }
const sessions = {
  p1: { id: 'p1', title: 'fc-plan', repo_id: 'r-fc', state: 'IMPLEMENTING_PLAN', branch: '' },
  o1: { id: 'o1', title: 'fc-plan', repo_id: 'r-other', state: 'IMPLEMENTING_PLAN', branch: '' },
}
const out = {
  cron: [{ id: 'job-fc', repo_id: 'r-fc', name: 'fc-plan', last_run_session_id: 'p-newer' }],
  ls: { sessions: Object.values(sessions) },
  chats: { chats: [chat] },
  show: { session: { ...sessions[id], last_agent_activity_at: '2026-11-07T08:48:00Z' } },
  notes: [],
}[cmd]
process.stdout.write(JSON.stringify(out))
`

test('a finished plan cron run that lost its job’s last-run pointer is still bossd’s to finalize', (t) => {
  const dir = tempDir('orchestrate-boss-', t)
  const boss = path.join(dir, 'boss')
  fs.writeFileSync(boss, FAKE_PLAN_BOSS, { mode: 0o755 })
  const result = collect({ hosts: [], local: true, bossBin: boss, now: NOW, root: dir })
  assert.deepEqual(result.errors, [])
  // The same title in a repo with no such job is no cron run, so its idle chat is nudged.
  assert.deepEqual(result.rows.map((row) => [row.session, row.action]).sort(), [
    ['o1', 'stalled-chat'],
    ['p1', 'finalizing'],
  ])
})
