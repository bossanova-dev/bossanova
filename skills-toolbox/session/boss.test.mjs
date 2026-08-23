import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  createBossSessionRunnerAdapter,
  bossSessionOperationMap,
  requiredBossToolsForEpic,
  requiredBossCliCommandsForEpic,
  bossCliDegradedCapabilities,
  bossCliPartialCapabilities,
  bossEpicTransportPreflight,
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

test('every tool-backed capability declares a cli transport or an explicit cli: null reason', () => {
  // The anti-drift gate. A capability that names an MCP `tool` but neither a
  // `cli` transport nor an explicit `cli: null` is ambiguous between "not yet
  // mapped" and "cannot be mapped" — and silently shrinks what the CLI
  // transport can do. Adding a capability without deciding is a test failure.
  for (const [name, op] of Object.entries(bossSessionOperationMap)) {
    if (typeof op.tool !== 'string') continue
    assert.ok('cli' in op, `${name} declares a tool but no cli decision`)
    if (op.cli === null) {
      assert.equal(typeof op.cliReason, 'string', `${name} is cli: null but states no cliReason`)
      assert.ok(op.cliReason.length > 0, `${name} has an empty cliReason`)
      continue
    }
    assert.equal(typeof op.cli.cmd, 'string', `${name} cli transport has no cmd`)
    assert.ok(Array.isArray(op.cli.args), `${name} cli transport has no args`)
    assert.ok(Array.isArray(op.cli.response), `${name} cli transport has no response paths`)
  }
})

test('requiredBossCliCommandsForEpic returns the distinct sorted CLI command set', () => {
  const commands = requiredBossCliCommandsForEpic()
  assert.deepEqual(commands, [...commands].sort())
  assert.equal(new Set(commands).size, commands.length)
  // Derived from the map, not hand-kept: every command is some capability's
  // cli.cmd + its leading subcommand words, and every cli transport is covered.
  const derived = new Set(
    Object.values(bossSessionOperationMap)
      .filter((op) => op.cli)
      .map((op) => {
        const words = []
        for (const arg of op.cli.args) {
          if (arg.startsWith('<') || arg.startsWith('-')) break
          words.push(arg)
        }
        return [op.cli.cmd, ...words].join(' ')
      }),
  )
  assert.deepEqual(new Set(commands), derived)
  // The commands the epic choreography cannot run without.
  for (const cmd of ['boss new', 'boss show', 'boss ls', 'boss merge', 'boss session checks']) {
    assert.ok(commands.includes(cmd), `missing required CLI command ${cmd}`)
  }
})

test('bossEpicTransportPreflight PREFERS the CLI when both transports are complete', () => {
  // The preference, pinned by name. A bossd spawn wired the boss MCP server
  // unconditionally until this preference existed, so an MCP-preferring resolver
  // would have picked `mcp` on every real run and the CLI path would only ever
  // have executed in foreign repos — which is exactly the outcome this
  // preference exists to prevent, and what made it safe to stop wiring that
  // server by default. Both sets complete MUST select `cli`.
  const result = bossEpicTransportPreflight({
    availableTools: requiredBossToolsForEpic(),
    availableCliCommands: requiredBossCliCommandsForEpic(),
  })
  assert.equal(result.ok, true)
  assert.equal(result.transport, 'cli')
  assert.deepEqual(result.missing, [])
  assert.deepEqual(result.missingTools, [])
  assert.deepEqual(result.missingCliCommands, [])
  // Preferring the CLI is not free: it costs the cli: null capabilities, and the
  // resolver must say so even on the happy path.
  assert.deepEqual(result.degraded, bossCliDegradedCapabilities())
})

test('bossEpicTransportPreflight selects MCP only when the CLI set is incomplete', () => {
  // The converse of the preference: MCP is reachable, but never by winning a tie.
  const result = bossEpicTransportPreflight({
    availableTools: requiredBossToolsForEpic(),
    availableCliCommands: requiredBossCliCommandsForEpic().slice(1),
  })
  assert.equal(result.ok, true)
  assert.equal(result.transport, 'mcp')
  assert.deepEqual(result.missing, [])
  assert.deepEqual(result.degraded, [])
  assert.deepEqual(result.missingCliCommands, [requiredBossCliCommandsForEpic()[0]])
})

test('bossEpicTransportPreflight falls through to the CLI transport with no MCP at all', () => {
  // The property this whole capability exists to create: a runtime with zero
  // boss MCP tools but a complete boss CLI is NOT blocked — it runs degraded.
  const result = bossEpicTransportPreflight({
    availableTools: [],
    availableCliCommands: requiredBossCliCommandsForEpic(),
  })
  assert.equal(result.ok, true)
  assert.equal(result.transport, 'cli')
  assert.deepEqual(result.missing, [])
  // `degraded` names EXACTLY the cli: null capabilities, sorted — so the run's
  // opening line can report what it cannot do rather than silently guessing.
  assert.deepEqual(result.degraded, bossCliDegradedCapabilities())
  assert.ok(result.degraded.includes('resolveContext'))
  assert.ok(result.degraded.includes('getSessionStatuses'))
  assert.deepEqual(result.degraded, [...result.degraded].sort())
})

test('bossEpicTransportPreflight passes via CLI when MCP is missing only merge_session', () => {
  // The literal BOS-816 failure: a runtime exposing every boss MCP tool except
  // merge_session used to hard-BLOCK an entire epic. With a complete CLI it now
  // runs on the CLI transport.
  const result = bossEpicTransportPreflight({
    availableTools: requiredBossToolsForEpic().filter((t) => t !== 'merge_session'),
    availableCliCommands: requiredBossCliCommandsForEpic(),
  })
  assert.equal(result.ok, true)
  assert.equal(result.transport, 'cli')
  assert.deepEqual(result.missing, [])
  assert.deepEqual(result.missingTools, ['merge_session'])
})

test('bossEpicTransportPreflight blocks only when neither transport is complete', () => {
  const result = bossEpicTransportPreflight({
    availableTools: requiredBossToolsForEpic().filter((t) => t !== 'merge_session'),
    availableCliCommands: requiredBossCliCommandsForEpic().filter((c) => c !== 'boss merge'),
  })
  assert.equal(result.ok, false)
  assert.equal(result.transport, null)
  assert.ok(result.missing.length > 0)
  assert.deepEqual(result.missing, [...result.missing].sort())
  assert.deepEqual(result.missingTools, ['merge_session'])
  assert.deepEqual(result.missingCliCommands, ['boss merge'])
  assert.deepEqual(result.degraded, [])
  // A wholly empty runtime reports both transports' full requirement lists.
  const empty = bossEpicTransportPreflight({ availableTools: [], availableCliCommands: [] })
  assert.equal(empty.ok, false)
  assert.deepEqual(empty.missingTools, requiredBossToolsForEpic())
  assert.deepEqual(empty.missingCliCommands, requiredBossCliCommandsForEpic())
})

test('bossEpicToolPreflight keeps its original shape for its original inputs', () => {
  // Back-compat guard: MCP-mode callers pass a bare iterable of tool names and
  // read exactly {ok, missing}. Generalising the preflight must not widen this.
  const pass = bossEpicToolPreflight(requiredBossToolsForEpic())
  assert.deepEqual(Object.keys(pass).sort(), ['missing', 'ok'])
  assert.equal(pass.ok, true)
  assert.deepEqual(pass.missing, [])

  const fail = bossEpicToolPreflight(
    requiredBossToolsForEpic().filter((t) => t !== 'merge_session'),
  )
  assert.deepEqual(Object.keys(fail).sort(), ['missing', 'ok'])
  assert.equal(fail.ok, false)
  assert.deepEqual(fail.missing, ['merge_session'])
})

test('resolveContext declares the loud BOSS_REPO_ID fallback, never a directory guess', () => {
  // A wrong repo id would schedule an entire epic against the wrong repository,
  // so the CLI-mode substitute must fail loudly on an empty value.
  const op = bossSessionOperationMap.resolveContext
  assert.equal(op.cli, null)
  assert.equal(typeof op.cliReason, 'string')
  assert.equal(op.cliFallback.env, 'BOSS_REPO_ID')
  assert.equal(op.cliFallback.command, 'boss env --json')
  assert.deepEqual(op.cliFallback.response, ['session.repo_id'])
  assert.equal(op.cliFallback.onEmpty, 'fail')
})

test('the cli transports name the real boss CLI envelope paths, not the MCP ones', () => {
  // The CLI nests what the MCP tools return flat, and trims (or does not trim)
  // enum names differently. Copying the MCP `response` paths across would read
  // `undefined` at runtime — the map documents both shapes deliberately.
  assert.deepEqual(bossSessionOperationMap.createSession.cli.response, [
    'session.id',
    'session.chat_id',
  ])
  assert.deepEqual(bossSessionOperationMap.mergeSession.cli.response, ['session.state'])
  // MCP list_sessions / list_agents are bare arrays; the CLI wraps both.
  assert.deepEqual(bossSessionOperationMap.listSessions.cli.response, [
    'sessions',
    'sessions[].last_agent_activity_at',
    'sessions[].tracker_id',
  ])
  assert.deepEqual(bossSessionOperationMap.listAgents.cli.response, ['agents'])
  // get_chat_statuses returns `statuses`; `boss chats --json` returns `chats`.
  assert.deepEqual(bossSessionOperationMap.getChatStatuses.cli.response, ['chats'])
  // send_chat_message has no --json surface at all, matching its empty response.
  assert.deepEqual(bossSessionOperationMap.sendChatMessage.cli.response, [])
})

test('partially-covered cli transports name what the CLI cannot supply', () => {
  // `boss show --json` carries state/last_agent_activity_at/pr_number/pr_url,
  // but not every getSession routing signal. A partial transport is still
  // usable, so it stays OUT of `degraded` (which means "cannot do at all") —
  // but it must say what is absent rather than let a caller read `undefined`
  // and route on it.
  const missing = bossSessionOperationMap.getSession.cli.missingResponse
  assert.ok(Array.isArray(missing))
  assert.deepEqual(bossSessionOperationMap.getSession.cli.response, [
    'session.state',
    'session.last_agent_activity_at',
  ])
  assert.equal(missing.includes('last_agent_activity_at'), false)
  for (const field of ['attention_status.reason', 'pr_mergeable', 'merge_block']) {
    assert.ok(missing.includes(field), `getSession cli must declare ${field} unavailable`)
  }
  assert.equal(bossCliDegradedCapabilities().includes('getSession'), false)
})

test('bossEpicTransportPreflight reports partially-covered capabilities on the CLI transport', () => {
  // BOS-827. `degraded` only ever named the cli: null capabilities, so a
  // capability whose CLI exists but is INCOMPLETE was reported as fine. Once the
  // boss MCP server stopped being wired by default, every run took the CLI
  // transport and reported ok:true while silently losing getSession's remaining
  // missing routing signals — auth-death routing and conflict-after-green. The
  // run must say so out loud instead.
  const result = bossEpicTransportPreflight({
    availableTools: [],
    availableCliCommands: requiredBossCliCommandsForEpic(),
  })
  assert.equal(result.ok, true)
  assert.equal(result.transport, 'cli')
  assert.deepEqual(
    result.partial.map((entry) => entry.capability),
    ['getSession'],
  )
  assert.deepEqual(result.partial[0].missingResponse, [
    'repair_active',
    'attention_status.reason',
    'pr_mergeable',
    'merge_block',
  ])
})

test('bossEpicTransportPreflight keeps `partial` and `degraded` disjoint and complete', () => {
  // The two lists answer different questions: `degraded` is "cannot do at all",
  // `partial` is "can do, but blind to these fields". Reclassifying getSession as
  // degraded would contradict the map's deliberate "a partial transport is still
  // a transport" stance and break BOS-825's shipped contract.
  const result = bossEpicTransportPreflight({
    availableTools: requiredBossToolsForEpic(),
    availableCliCommands: requiredBossCliCommandsForEpic(),
  })
  // Exactly the three cli: null capabilities, and getSession is not among them.
  assert.deepEqual(result.degraded, ['createPlanningChat', 'getSessionStatuses', 'resolveContext'])
  assert.equal(result.degraded.includes('getSession'), false)

  const partialNames = result.partial.map((entry) => entry.capability)
  for (const name of partialNames) {
    assert.equal(result.degraded.includes(name), false, `${name} must not be in both lists`)
  }
  assert.deepEqual(partialNames, [...partialNames].sort())
})

test('bossEpicTransportPreflight reports no partial capabilities on the MCP transport', () => {
  // The MCP responses are complete, so there is nothing to warn about — a
  // non-empty `partial` here would send an operator hunting a loss that is not
  // happening.
  const result = bossEpicTransportPreflight({
    availableTools: requiredBossToolsForEpic(),
    availableCliCommands: requiredBossCliCommandsForEpic().slice(1),
  })
  assert.equal(result.transport, 'mcp')
  assert.deepEqual(result.partial, [])
})

test('bossEpicTransportPreflight reports no partial capabilities when it BLOCKs', () => {
  // Neither transport is usable, so there is no transport to be partial about;
  // `missing` stays the sorted repair set, unchanged by BOS-827.
  const result = bossEpicTransportPreflight({
    availableTools: requiredBossToolsForEpic().filter((t) => t !== 'merge_session'),
    availableCliCommands: requiredBossCliCommandsForEpic().filter((c) => c !== 'boss merge'),
  })
  assert.equal(result.ok, false)
  assert.equal(result.transport, null)
  assert.deepEqual(result.partial, [])
  assert.deepEqual(result.degraded, [])
  assert.deepEqual(result.missing, [...result.missing].sort())
})

test('bossCliPartialCapabilities derives from the map, not a hand-kept list', () => {
  // Derived, so a capability that later gains a missingResponse is reported
  // automatically rather than needing this list edited in a second place.
  const derived = bossCliPartialCapabilities()
  assert.deepEqual(
    derived.map((entry) => entry.capability),
    ['getSession'],
  )
  assert.deepEqual(
    derived[0].missingResponse,
    bossSessionOperationMap.getSession.cli.missingResponse,
  )
  // A capability with a cli transport but no missingResponse is complete and
  // must NOT appear.
  assert.equal(
    derived.some((entry) => entry.capability === 'mergeSession'),
    false,
  )
})

test('bossEpicToolPreflight does not leak `partial` into its return', () => {
  // Back-compat: MCP-mode callers read exactly {ok, missing}. `partial` is
  // additive on the transport preflight only.
  for (const input of [requiredBossToolsForEpic(), []]) {
    const result = bossEpicToolPreflight(input)
    assert.deepEqual(Object.keys(result).sort(), ['missing', 'ok'])
  }
})
