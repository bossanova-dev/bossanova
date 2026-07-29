import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  createBossSessionRunnerAdapter,
  bossSessionOperationMap,
  requiredBossToolsForEpic,
  bossEpicToolPreflight,
} from './boss.mjs'
import { assertConforms, SESSION_RUNNER_CAPABILITIES } from './adapter.mjs'

test('names the exact boss MCP tools for each choreography capability', () => {
  assert.equal(bossSessionOperationMap.createSession.tool, 'create_session')
  assert.equal(bossSessionOperationMap.getSession.tool, 'get_session')
  assert.equal(bossSessionOperationMap.listSessions.tool, 'list_sessions')
  assert.equal(bossSessionOperationMap.listCheckSnapshots.tool, 'list_check_snapshots')
  assert.equal(bossSessionOperationMap.mergeSession.tool, 'merge_session')
  assert.equal(bossSessionOperationMap.resolveContext.tool, 'resolve_context')
  assert.equal(bossSessionOperationMap.listAgents.tool, 'list_agents')
})

test('mergeSession records the mandatory confirm arg', () => {
  assert.ok(bossSessionOperationMap.mergeSession.args.includes('confirm'))
})

test('createSession records the headless tmux fan-out args', () => {
  const args = bossSessionOperationMap.createSession.args
  // `tmux_unattended` is the field the SKILL actually passes for the durable,
  // restart-surviving, auto-submitted pane (BOS-179/BOS-208) — not `detach`.
  for (const a of ['tmux_unattended', 'model', 'prompt', 'title', 'tracker_id']) {
    assert.ok(args.includes(a), `createSession missing arg ${a}`)
  }
})

test('planning-only dispatch does not use PR-backed tmux sessions', () => {
  // BOS-322: planning-only epic work (recon, plan review, visible /boss-plan
  // chat) must route through a distinct create_session capability that never
  // opens a worktree/branch/PR/finalize path. `quick_chat: true` is the visible
  // no-PR chat; `tmux_unattended`/`pr_number`/`branch_name` are the PR-backed
  // implementation fields it MUST NOT carry, so planning fan-out can never
  // collapse back into the implementation `createSession` path.
  const planning = bossSessionOperationMap.createPlanningChat
  assert.ok(planning, 'missing createPlanningChat capability')
  assert.equal(planning.tool, 'create_session')
  assert.ok(planning.args.includes('quick_chat'), 'planning chat must be quick_chat-backed')
  assert.ok(planning.args.includes('prompt'), 'planning chat still needs a prompt')
  assert.ok(planning.args.includes('title'), 'planning chat still needs a visible title')
  assert.equal(planning.args.includes('tmux_unattended'), false)
  assert.equal(planning.args.includes('pr_number'), false)
  assert.equal(planning.args.includes('branch_name'), false)
  assert.deepEqual(planning.response, ['id', 'agent_session_id'])
})

test('the operation map names the real MCP arg/response fields', () => {
  // create_session flattens the Session, so the identifiers are `id` +
  // `agent_session_id` — NOT `session_id`/`chat_id`, which do not exist on the
  // response (codex P2 on PR #1112).
  assert.deepEqual(bossSessionOperationMap.createSession.response, ['id', 'agent_session_id'])
  // list_check_snapshots keys on `session_id` (ListCheckSnapshotsArgs); passing
  // `id` leaves it empty and the daemon rejects the call (codex P2).
  assert.ok(bossSessionOperationMap.listCheckSnapshots.args.includes('session_id'))
  assert.ok(!bossSessionOperationMap.listCheckSnapshots.args.includes('id'))
  // get_session_statuses takes a plural `session_ids` list (SessionIDsArgs).
  assert.ok(bossSessionOperationMap.getSessionStatuses.args.includes('session_ids'))
})

test('getSession documents the Phase-3 routing signals', () => {
  // The epic poller routes on more than `state`: attention_status.reason carries
  // AGENT_AUTH_FAILED (login-death), and pr_mergeable / merge_block detect a
  // conflict-after-green so a "Passing but conflicting" PR goes to repair rather
  // than being treated as ordinary mergeable work (codex P2 on PR #1112).
  const response = bossSessionOperationMap.getSession.response
  for (const field of ['attention_status.reason', 'pr_mergeable', 'merge_block']) {
    assert.ok(response.includes(field), `getSession missing routing signal ${field}`)
  }
  // attention_status.reason NESTS under attention_status — a flat
  // `attention_reason` key does not exist on the Session response.
  assert.ok(!response.includes('attention_reason'))
})

test('array-returning tools document a bare array, not a wrapper object', () => {
  // list_sessions and list_agents marshal the backend's `[]*Session` /
  // `[]*AgentInfo` directly — the result is a bare array with no `sessions` /
  // `agents` wrapper key. Documenting a top-level key would read `undefined`
  // (codex P2 on PR #1112). An empty response records "no wrapper object".
  assert.deepEqual(bossSessionOperationMap.listSessions.response, [])
  assert.deepEqual(bossSessionOperationMap.listAgents.response, [])
})

test('nested/collection response fields name the real JSON path', () => {
  // list_check_snapshots returns a `snapshots` array (each snapshot's
  // `computed_status` is the DisplayStatus) — there is no top-level
  // `DisplayStatus`. resolve_context nests the repo id at `repo.id`, not a flat
  // `repo_id`. Following the old flat keys reads `undefined` (codex P2).
  assert.deepEqual(bossSessionOperationMap.listCheckSnapshots.response, ['snapshots'])
  assert.ok(!bossSessionOperationMap.listCheckSnapshots.response.includes('DisplayStatus'))
  assert.deepEqual(bossSessionOperationMap.resolveContext.response, ['repo.id'])
  assert.ok(!bossSessionOperationMap.resolveContext.response.includes('repo_id'))
})

test('the repair-round + optional-signal boss MCP tools are documented', () => {
  // record_chat + send_chat_message are how dispatchRepair opens a fresh chat in
  // the ticket's own session; get_chat_statuses is the per-chat green/settled
  // gate the poll invokes (Phase 3b), and get_session_statuses is the
  // session-aggregate display-only signal. All documented in the map but not
  // part of the required conformance-capability set.
  assert.equal(bossSessionOperationMap.recordChat.tool, 'record_chat')
  assert.equal(bossSessionOperationMap.sendChatMessage.tool, 'send_chat_message')
  assert.equal(bossSessionOperationMap.getChatStatuses.tool, 'get_chat_statuses')
  assert.deepEqual(bossSessionOperationMap.getChatStatuses.args, ['session_id'])
  assert.equal(bossSessionOperationMap.getSessionStatuses.tool, 'get_session_statuses')
})

test('dispatch capabilities carry the sub-skill invocation shapes', () => {
  // Reconciled from the plan's pre-rename `/bs-implement`: the current
  // Bossanova implement sub-skill is `/boss-build` (BOS-194 rename merged).
  assert.equal(bossSessionOperationMap.dispatchImplement.subSkill, '/boss-build')
  assert.equal(bossSessionOperationMap.dispatchRepair.subSkill, '/boss-repair')
})

test('subSkills resolves to the Bossanova reference sub-skills', () => {
  const adapter = createBossSessionRunnerAdapter()
  assert.equal(adapter.subSkills.implement, '/boss-build')
  assert.equal(adapter.subSkills.repair, '/boss-repair')
})

test('every required capability is a key on the operation map', () => {
  for (const cap of SESSION_RUNNER_CAPABILITIES) {
    assert.ok(cap in bossSessionOperationMap, `operation map missing capability ${cap}`)
  }
})

test('the boss adapter conforms to the interface', () => {
  const adapter = createBossSessionRunnerAdapter()
  assert.equal(adapter.runner, 'boss')
  assert.doesNotThrow(() => assertConforms(adapter))
})

test('requiredBossToolsForEpic derives the epic discovery-preflight tool set from the map', () => {
  const tools = requiredBossToolsForEpic()
  // Every returned tool is some capability's `.tool` in the source-of-truth map
  // (derived, not duplicated), and every `.tool` in the map is covered — so the
  // preflight list can never silently drift from the choreography it gates.
  const mapTools = new Set(
    Object.values(bossSessionOperationMap)
      .map((op) => op.tool)
      .filter((t) => typeof t === 'string'),
  )
  assert.deepEqual(new Set(tools), mapTools)
  // Sorted + de-duplicated (create_session backs createSession + createPlanningChat).
  assert.deepEqual(tools, [...tools].sort())
  assert.equal(new Set(tools).size, tools.length)
})

test('the epic preflight tool set pins the tools whose late discovery stalls a run', () => {
  const required = requiredBossToolsForEpic()
  // list_check_snapshots was the tool that only surfaced after a targeted boss
  // search in a Wondercanvas run — it must always be in the preflight set.
  assert.ok(required.includes('list_check_snapshots'))
  // get_chat_statuses is the per-chat green/settled gate the poll invokes every
  // 2–5 min (SKILL Phase 3b); if the runtime cannot see it the run stalls at the
  // merge gate — the exact BOS-301 failure class — so it MUST be pinned too. (It
  // is distinct from the session-aggregate display-only get_session_statuses.)
  assert.ok(required.includes('get_chat_statuses'))
  assert.ok(required.includes('get_session_statuses'))
})

test('bossEpicToolPreflight passes when every required tool is present', () => {
  const { ok, missing } = bossEpicToolPreflight(requiredBossToolsForEpic())
  assert.equal(ok, true)
  assert.deepEqual(missing, [])
  // Extra unrelated tools do not affect the verdict.
  assert.equal(bossEpicToolPreflight([...requiredBossToolsForEpic(), 'some_other_tool']).ok, true)
})

test('bossEpicToolPreflight names the absent tools in a concise sorted list', () => {
  const full = requiredBossToolsForEpic()
  // Drop list_check_snapshots specifically — the historically-missed tool.
  const without = full.filter((t) => t !== 'list_check_snapshots')
  const one = bossEpicToolPreflight(without)
  assert.equal(one.ok, false)
  assert.deepEqual(one.missing, ['list_check_snapshots'])
  // Multiple missing tools come back sorted (stable diagnostic).
  const two = bossEpicToolPreflight(without.filter((t) => t !== 'merge_session'))
  assert.equal(two.ok, false)
  assert.deepEqual(two.missing, ['list_check_snapshots', 'merge_session'])
  // Empty runtime → every required tool reported missing.
  assert.deepEqual(bossEpicToolPreflight([]).missing, full)
})
