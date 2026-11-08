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
  remedy,
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

// Two sessions on this machine: s1's PR is verified, s2's has failing checks and no live chat.
const fakeDump = () => [
  { session: session({ id: 's1' }), chats: [stoppedChat], pr: prState(), repo: 'acme/app' },
  {
    session: session({ id: 's2', agent: 'claude' }),
    chats: [stoppedChat],
    pr: prState({
      pr_number: 13,
      check_verdict: { state: 'failing', failed_count: 1, pending_count: 0 },
      checks: [{ name: 'lint', conclusion: 'failure' }],
    }),
    repo: 'acme/app',
  },
]
const snapshot = (root, over = {}) =>
  collect({
    hosts: [],
    local: true,
    bossBin: 'boss',
    now: NOW,
    root,
    dump: fakeDump,
    crons: () => [],
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
  const s = { repair_active: true, last_repair: { started_at: '2026-11-07T08:50:00Z' } }
  assert.equal(classify(s, [stoppedChat], pr).action, 'working')
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
