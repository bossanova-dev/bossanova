// skills-toolbox/session/boss.mjs
// Boss reference implementation of the session-runner-adapter interface.
// Preserves current boss-epic behaviour exactly: it reimplements nothing, it
// DOCUMENTS. Each capability names the boss MCP tool + the arg/response fields
// the boss-epic prose already uses, so the SKILL has one source of truth
// instead of hard-coding tool names inline. The agent still issues the MCP
// calls; this map just centralizes them (mirrors the tracker reference
// buildLinearOperationMap — see that module's configuration notes). node builtins only.
//
// Sub-skill dispatch resolves to the current Bossanova sub-skills. Note the
// reconciliation: older plans may name `/bs-implement`, but the rename
// (bs-* -> boss-*) is merged, so today's exact implement
// sub-skill is `/boss-build` (the boss-epic SKILL dispatches
// `/boss-build <ticket-id>`). Using `/bs-implement` would resolve to a
// non-existent skill and BREAK the zero-behaviour-change bar; `/boss-repair`
// was already renamed and is unchanged.

// capability -> { tool, args:[...], response:[...] } (boss MCP choreography) OR
// capability -> { subSkill, args:[...] } (per-ticket fan-out dispatch).
//
// Every tool-backed capability ALSO declares a second transport: `cli`, the
// equivalent `boss` CLI invocation, or an explicit `cli: null` plus a
// `cliReason` saying why the CLI cannot express it. Both are recorded because an
// absent key is ambiguous between "not yet mapped" and "cannot be mapped", and
// because the two transports do not share a wire shape — the CLI's `--json`
// envelopes NEST what the MCP tools return flat, and trim (or leave) enum names
// differently. `cli.response` therefore names the CLI's own paths, never a copy
// of the MCP `response`. A capability the CLI covers only partially keeps its
// `cli` transport and names the absent values in `cli.missingResponse`, so a
// caller cannot read `undefined` and route on it. Those partially-covered
// capabilities are surfaced at runtime by bossCliPartialCapabilities and
// reported in bossEpicTransportPreflight's `partial` key, so a CLI-transport run
// states the signals it is flying without rather than losing them silently.
//
// The point of the second transport is that a runtime without the boss MCP
// server is degraded, not dead — and that the CLI is the PREFERRED carrier, not
// a consolation: bossEpicTransportPreflight below picks the CLI whenever its
// command set is complete (including when MCP is also complete), falls back to
// MCP only when the CLI set is incomplete, and BLOCKS only when neither
// transport can carry the run.
export const bossSessionOperationMap = {
  createSession: {
    tool: 'create_session',
    // Headless fan-out fields (SKILL Phase 3a). `tmux_unattended:true` is the
    // durable, restart-surviving, auto-submitted pane; `model` is the Phase-0
    // MODEL. create_session flattens the created Session at the top level, so
    // the identifiers come back as `id` (the session id) + `agent_session_id`
    // (the chat id) directly — NOT `session_id`/`chat_id`.
    args: [
      'repo_id',
      'tmux_unattended',
      'model',
      'prompt',
      'title',
      'agent',
      'tracker_id',
      'tracker_source',
      'tracker_url',
    ],
    response: ['id', 'agent_session_id'],
    // `boss new --json` is the CLI equivalent, and the ONE capability whose two
    // transports disagree about the identifier names: the MCP tool flattens the
    // Session (`id` / `agent_session_id`), while the CLI nests a deliberately
    // narrow envelope under `session` and calls the chat id `chat_id`.
    cli: {
      cmd: 'boss',
      args: [
        'new',
        '--repo',
        '<repo_id>',
        '--prompt',
        '<prompt>',
        '--title',
        '<title>',
        '--agent',
        '<agent>',
        '--model',
        '<model>',
        '--tmux-unattended',
        '--tracker-id',
        '<tracker_id>',
        '--tracker-source',
        '<tracker_source>',
        '--tracker-url',
        '<tracker_url>',
        '--json',
      ],
      response: ['session.id', 'session.chat_id'],
    },
  },
  createPlanningChat: {
    tool: 'create_session',
    // Visible planning-only work (recon, plan review, an intentionally
    // human-visible `/boss-plan` chat) — NOT implementation fan-out. `quick_chat:
    // true` opens a no-worktree, no-branch, no-PR, no-finalize chat, so a planning
    // subtask can never be surfaced as a failed implementation session with
    // `pr_no_changes`. Deliberately omits `tmux_unattended`, `model`, `pr_number`,
    // and `branch_name` so it cannot collapse back into the PR-backed
    // `createSession` path. Unattended planning fan-out should stay inside the
    // driver as a subagent; use this only when a visible Boss chat is wanted.
    args: [
      'repo_id',
      'quick_chat',
      'prompt',
      'title',
      'agent',
      'tracker_id',
      'tracker_source',
      'tracker_url',
    ],
    response: ['id', 'agent_session_id'],
    // No CLI equivalent: `boss new` has no `--quick-chat` flag, so every CLI
    // session opens a worktree, a branch and a PR — precisely what quick_chat
    // exists to avoid. Substituting `boss new` here would surface planning work
    // as a failed implementation session with `pr_no_changes`, so CLI mode does
    // without visible planning chats and keeps planning fan-out in-driver.
    cli: null,
    cliReason: '`boss new` has no --quick-chat flag; a CLI chat would open a worktree/branch/PR',
  },
  getSession: {
    tool: 'get_session',
    args: ['id'],
    // state = IMPLEMENTING / GREEN_DRAFT / READY_FOR_REVIEW / BLOCKED / MERGED;
    // last_agent_activity_at drives hang detection; repair_active gates repair
    // dispatch. attention_status.reason carries AGENT_AUTH_FAILED (login-death
    // routing, SKILL Phase 3) — it NESTS under attention_status, not a top-level
    // attention_reason. pr_mergeable (optional bool; false = conflict-after-
    // green) + merge_block.gate route a "Passing but conflicting" green to a
    // repair round instead of a merge, so the map-driven runner does not treat it
    // as ordinary mergeable work (SKILL Phase 3; codex P2 on PR #1112).
    response: [
      'state',
      'last_agent_activity_at',
      'repair_active',
      'attention_status.reason',
      'pr_mergeable',
      'merge_block',
    ],
    // PARTIAL. `boss show --json` carries the lifecycle state (trimmed of its
    // SESSION_STATE_ prefix, same vocabulary as the MCP enum) and, beyond the
    // MCP response, `session.pr_number` / `session.pr_url` /
    // `session.display_status` / `session.last_check_state` / `session.last_repair`.
    // It carries NONE of get_session's routing signals, so those are named in
    // missingResponse rather than left to read `undefined`: a CLI-mode run must
    // detect hangs, auth death and conflict-after-green some other way (poll
    // `session checks` for CI, treat an unreadable signal as not-settled) instead
    // of silently routing on a missing field. A partial transport is still a
    // transport, so getSession stays OUT of the degraded set.
    cli: {
      cmd: 'boss',
      args: ['show', '<id>', '--json'],
      response: ['session.state'],
      missingResponse: [
        'last_agent_activity_at',
        'repair_active',
        'attention_status.reason',
        'pr_mergeable',
        'merge_block',
      ],
    },
  },
  listSessions: {
    tool: 'list_sessions',
    args: ['repo_id'],
    // list_sessions marshals the backend's `[]*Session` directly — the result
    // is the bare Session array, NOT an object with a `sessions` field. Match
    // each element to epic tickets by its `title` convention or `tracker_id`
    // (resume). Reading a top-level `sessions` key would be `undefined` and
    // miss already-running boss-epic sessions (codex P2 on PR #1112).
    response: [],
    // `boss ls --json` wraps the rows the MCP tool returns bare, so the response
    // path is the envelope key rather than an empty list.
    cli: {
      cmd: 'boss',
      args: ['ls', '--repo', '<repo_id>', '--json'],
      response: ['sessions'],
    },
  },
  listCheckSnapshots: {
    tool: 'list_check_snapshots',
    // ListCheckSnapshotsArgs keys the session on `session_id` (not `id`); the
    // value is the session `id` recorded from create_session above.
    args: ['session_id'],
    // ListCheckSnapshotsResponse carries a `snapshots` array; each snapshot's
    // `computed_status` is the DisplayStatus (Passing / Failing / Conflict /
    // Rejected / Pending). There is no top-level `DisplayStatus` field — read
    // the newest snapshot's `computed_status` (codex P2 on PR #1112).
    response: ['snapshots'],
    cli: {
      cmd: 'boss',
      args: ['session', 'checks', '<session_id>', '--limit', '<limit>', '--json'],
      response: ['snapshots'],
    },
  },
  mergeSession: {
    tool: 'merge_session',
    // confirm:true is MANDATORY — merge_session refuses without it. Serialized:
    // at most one in flight, only in nextToMerge order.
    args: ['id', 'confirm'],
    response: ['state'],
    // `-y` is mandatory, not decorative: the CLI prompts for confirmation
    // otherwise and an unattended driver would hang on a TTY read that never
    // arrives. Note the state vocabulary differs — `boss merge --json` emits the
    // FULL enum name (`SESSION_STATE_MERGED`) where the MCP tool and every other
    // `--json` surface trim the prefix — so a caller comparing against the MCP
    // spelling must not assume the two are interchangeable.
    cli: {
      cmd: 'boss',
      args: ['merge', '<id>', '-y', '--json'],
      response: ['session.state'],
    },
  },
  resolveContext: {
    tool: 'resolve_context',
    args: ['working_dir'],
    // ResolveContextResponse nests the resolved repo under `repo`; the id is
    // `repo.id`, NOT a top-level `repo_id`. Following the flat key leaves the
    // repo id empty even inside a registered checkout (codex P2 on PR #1112).
    response: ['repo.id'],
    // No CLI equivalent: nothing in the CLI maps an arbitrary directory to a
    // repo id the way resolve_context does. What the CLI *does* know is the repo
    // id of the session it is already running inside, which is the only lookup a
    // skill actually performs — hence cliFallback rather than a bare null.
    //
    // onEmpty is 'fail' deliberately. The tempting degradation is to guess the
    // repo from the directory name or from the first row of `boss ls`; both are
    // silently wrong in a multi-repo daemon and would aim session writes at
    // another repository. An empty BOSS_REPO_ID means "this is not a boss
    // session", which is a BLOCK, not a value to invent.
    cli: null,
    cliReason:
      'no CLI command maps a working directory to a repo id; use the cliFallback env lookup',
    cliFallback: {
      env: 'BOSS_REPO_ID',
      command: 'boss env --json',
      response: ['session.repo_id'],
      onEmpty: 'fail',
    },
  },
  listAgents: {
    tool: 'list_agents',
    args: [],
    // list_agents marshals the backend's `[]*AgentInfo` directly — the result
    // is the bare AgentInfo array, NOT an object with an `agents` field. The
    // chosen --agent runner must appear here by its `name` (Phase 0 preflight);
    // reading a top-level `agents` key would be `undefined` and wrongly report
    // the runner missing (codex P2 on PR #1112).
    response: [],
    cli: {
      cmd: 'boss',
      args: ['agents', '--json'],
      response: ['agents'],
    },
  },
  dispatchImplement: {
    // Reconciled from the plan's pre-rename `/bs-implement` — see file header.
    subSkill: '/boss-build',
    // dispatched as the BARE single-line prompt "/boss-build <ticket-id>".
    args: ['ticketId'],
  },
  dispatchRepair: {
    subSkill: '/boss-repair',
    // dispatched as "/boss-repair watch" in a fresh chat in the ticket's session.
    args: ['prNumber'],
  },
  // Documented boss MCP tools the choreography uses but that are not part of the
  // required capability set: record_chat + send_chat_message implement the
  // dispatchRepair fresh-chat mechanism (Phase 3c); get_chat_statuses is the
  // per-chat settled-chat signal that gates greens (Phase 3b/3c) — best-effort,
  // so an unreadable status during a restart window is treated as not-settled,
  // not merged; get_session_statuses is the session-aggregate display/diagnostic
  // signal only (never gate green/settled on it — SKILL Phase 3b).
  recordChat: {
    tool: 'record_chat',
    args: ['session_id', 'agent_session_id', 'agent_name', 'title'],
    response: [],
    // Direction reversed: record_chat REGISTERS an agent_session_id the caller
    // already holds, while `boss chat new` MINTS one and hands it back. A CLI
    // driver therefore cannot pre-generate the id and adopt it — it must take the
    // id the CLI returns and thread that through everything downstream. Hence a
    // response path here where the MCP transport needs none.
    cli: {
      cmd: 'boss',
      args: [
        'chat',
        'new',
        '<session_id>',
        '--title',
        '<title>',
        '--agent',
        '<agent_name>',
        '--json',
      ],
      response: ['chat.agent_session_id'],
    },
  },
  sendChatMessage: {
    tool: 'send_chat_message',
    args: ['agent_session_id', 'wake_if_asleep', 'submit', 'message'],
    response: [],
    // `boss chat send` has no --json, which costs nothing: the MCP tool's
    // response is empty too, so both transports carry the same (absent) payload
    // and the exit status is the whole result. The two flags are spelled out
    // rather than left to their defaults because a delivery that silently
    // prefilled a composer instead of submitting is the failure this capability
    // exists to avoid.
    cli: {
      cmd: 'boss',
      args: ['chat', 'send', '<session_id|chat_id>', '<message>', '--submit', '--wake-if-asleep'],
      response: [],
    },
  },
  getChatStatuses: {
    tool: 'get_chat_statuses',
    // get_chat_statuses keys on a single `session_id` (GetChatStatusesRequest);
    // the `statuses` array holds one ChatStatusEntry per chat (agent_session_id
    // + status). Phase 3b reads the entry whose agent_session_id == the ticket's
    // tracked chat to gate green-on-settled — this is the tool the run actually
    // invokes every poll, so it MUST be a mapped capability (and thus land in the
    // discovery-preflight set). Its earlier omission from the map was itself the
    // Late-discovery failure class for the merge gate.
    args: ['session_id'],
    response: ['statuses'],
    // Same values, different envelope key: the MCP tool calls the collection
    // `statuses`, the CLI calls it `chats`. Each row carries the per-chat
    // `status`, so the greens gate that keys off a recorded chat_id works
    // unchanged once the caller reads the right key.
    cli: {
      cmd: 'boss',
      args: ['chats', '<session_id>', '--json'],
      response: ['chats'],
    },
  },
  getSessionStatuses: {
    tool: 'get_session_statuses',
    // get_session_statuses takes a plural `session_ids` list (SessionIDsArgs),
    // not a single `id`. Session-aggregate, display/diagnostic only — NOT the
    // green/settled gate (that is get_chat_statuses above).
    args: ['session_ids'],
    response: ['statuses'],
    // No CLI equivalent: the session-level aggregate exists only as an MCP tool.
    // It is a convenience roll-up of what get_chat_statuses already reports, so
    // a CLI-mode driver loses a cheap summary, not a fact — it derives the same
    // answer by folding the per-chat statuses from `boss chats`. Recorded as a
    // degraded capability so the run says so out loud rather than appearing to
    // have consulted an aggregate it never read.
    cli: null,
    cliReason:
      'no CLI equivalent for the session-level aggregate; fold per-chat statuses from `boss chats` instead',
  },
}

/**
 * The distinct boss MCP tool names an epic run can invoke, derived from
 * bossSessionOperationMap (the single source of truth) instead of a hand-kept
 * duplicate list. This is boss-epic's deterministic *discovery-preflight* set
 *: a long unattended run must be able to SEE every one of these tools
 * before it schedules any session, so a tool that only surfaces after a targeted
 * search — the observed `list_check_snapshots` gap — fails the run early with a
 * concise diagnostic instead of stalling mid-run.
 *
 * Derived = every capability in the map that names a `.tool` (the sub-skill
 * dispatch capabilities carry `.subSkill`, not a tool, so they are excluded),
 * de-duplicated (create_session backs both createSession and createPlanningChat)
 * and sorted for a stable, diffable diagnostic. This deliberately covers MORE
 * than SESSION_RUNNER_CAPABILITIES: it also includes the repair-round tools
 * (record_chat / send_chat_message), the per-chat green/settled gate
 * (get_chat_statuses, invoked every poll in SKILL Phase 3b) and the
 * session-aggregate display signal (get_session_statuses), because an epic run
 * invokes them too — discovery must cover the WHOLE run, not just the
 * conformance-capability subset.
 * @returns {string[]} sorted, de-duplicated boss MCP tool names
 */
export function requiredBossToolsForEpic() {
  const tools = new Set()
  for (const op of Object.values(bossSessionOperationMap)) {
    if (typeof op.tool === 'string') tools.add(op.tool)
  }
  return [...tools].sort()
}

/**
 * Deterministic boss-epic tool-discovery preflight. Given the boss MCP
 * tool names the runtime actually exposes (`availableTools` — however the host
 * enumerates them), return which required tools are absent. The caller fails
 * fast on a non-empty `missing` and prints it so a human sees exactly which
 * tools to restore before the epic schedules anything. `ok` is true iff every
 * required tool is present; `missing` preserves requiredBossToolsForEpic()'s
 * sorted order.
 * @param {Iterable<string>} availableTools
 * @returns {{ ok: boolean, missing: string[] }}
 */
export function bossEpicToolPreflight(availableTools) {
  const { missingTools } = bossEpicTransportPreflight({ availableTools, availableCliCommands: [] })
  return { ok: missingTools.length === 0, missing: missingTools }
}

/**
 * The distinct `boss` CLI commands an epic run needs, derived from the same map
 * requiredBossToolsForEpic() walks. A "command" is the invocation's leading
 * words — `boss chat send`, not the whole argv — because that is the unit a
 * runtime can actually be probed for. Arguments are dropped at the first token
 * that starts with `<` (a placeholder) or `-` (a flag), which is what separates
 * `boss session checks` from its `<session_id> --limit <n> --json` tail.
 * @returns {string[]} sorted, de-duplicated `boss <subcommand>` strings
 */
export function requiredBossCliCommandsForEpic() {
  const commands = new Set()
  for (const op of Object.values(bossSessionOperationMap)) {
    if (typeof op.tool !== 'string' || !op.cli) continue
    const words = [op.cli.cmd]
    for (const arg of op.cli.args ?? []) {
      if (arg.startsWith('<') || arg.startsWith('-')) break
      words.push(arg)
    }
    commands.add(words.join(' '))
  }
  return [...commands].sort()
}

/**
 * The capabilities the CLI transport cannot carry at all — every op that
 * declares a `tool` but an explicit `cli: null`. A run on the CLI transport is
 * not broken, it is *degraded*, and this is the list it must say so about: a
 * caller that silently skipped these would look like it had consulted them.
 * Partially-covered capabilities (a real `cli` plus `missingResponse`) are NOT
 * here — they still have a working transport.
 * @returns {string[]} sorted capability names
 */
export function bossCliDegradedCapabilities() {
  return Object.entries(bossSessionOperationMap)
    .filter(([, op]) => typeof op.tool === 'string' && op.cli === null)
    .map(([name]) => name)
    .sort()
}

/**
 * The capabilities the CLI transport carries only PARTIALLY — every op with a
 * real `cli` that also declares a non-empty `cli.missingResponse`. These are
 * disjoint from bossCliDegradedCapabilities by construction: `degraded` means
 * "cannot do at all", this means "can do, but blind to these fields".
 *
 * It exists because reporting only `degraded` understated the cost of the CLI
 * transport. `getSession` is the live case: `boss show --json` works,
 * so the run reports ok, but it carries none of the routing signals the MCP
 * response does — hang detection, auth-death routing and conflict-after-green
 * all silently stop working. Once the boss MCP server stopped being wired by
 * default, that silence became EVERY run's, so a CLI-transport run has to name
 * the signals it is flying without.
 *
 * Derived from the map rather than hand-kept, so a capability that later gains
 * a `missingResponse` is reported without editing a second list. Note the gate
 * on the map's completeness does not *require* a `missingResponse` where one is
 * warranted, so a future partial transport could still be declared without one.
 * @returns {{ capability: string, missingResponse: string[] }[]} sorted by capability
 */
export function bossCliPartialCapabilities() {
  return Object.entries(bossSessionOperationMap)
    .filter(
      ([, op]) =>
        typeof op.tool === 'string' && op.cli && (op.cli.missingResponse?.length ?? 0) > 0,
    )
    .map(([capability, op]) => ({
      capability,
      missingResponse: [...op.cli.missingResponse],
    }))
    .sort((a, b) => a.capability.localeCompare(b.capability))
}

/**
 * Transport-aware preflight: the generalisation of bossEpicToolPreflight to a
 * runtime that may expose the boss MCP server, the boss CLI, or both.
 *
 * The CLI transport is PREFERRED, not a fallback: when both sets are complete
 * the resolver picks `cli`. That ordering is the whole point. A bossd spawn
 * wired the boss MCP server unconditionally until this preference existed, so
 * an MCP-preferring resolver would have selected `mcp` on every real run and
 * the CLI path would only ever have executed in foreign repos — and the
 * server's tool schemas are re-sent to the model every turn, so preferring the
 * CLI is what made it safe to stop wiring that server by default.
 * MCP is selected only when the CLI set is incomplete, and a complete MCP set
 * is then a PASS, not a consolation: a runtime missing one CLI command should
 * run on the richer transport rather than BLOCK. Only when neither is complete
 * is the run unable to
 * proceed, and then `missing` names everything absent from both so the operator
 * can see the full repair set in one line instead of discovering it twice.
 *
 * `missingTools` / `missingCliCommands` are always populated, even on a pass, so
 * a caller can report *why* it fell through to the CLI.
 *
 * `partial` is ADDITIVE and reported only on the CLI transport: the
 * capabilities that transport carries incompletely, with the response fields it
 * cannot supply. It is deliberately separate from `degraded` rather than folded
 * into it — `bossEpicToolPreflight`'s narrow `{ok, missing}` shape and the
 * pinned `degraded` contract both stay untouched — because a partial transport
 * is still a transport and reclassifying it as degraded would tell a run to
 * skip an operation it can perform.
 * @param {{ availableTools?: Iterable<string>, availableCliCommands?: Iterable<string> }} runtime
 * @returns {{ ok: boolean, transport: 'mcp'|'cli'|null, missing: string[], degraded: string[],
 *             partial: { capability: string, missingResponse: string[] }[],
 *             missingTools: string[], missingCliCommands: string[] }}
 */
export function bossEpicTransportPreflight({
  availableTools = [],
  availableCliCommands = [],
} = {}) {
  const tools = new Set(availableTools)
  const commands = new Set(availableCliCommands)
  const missingTools = requiredBossToolsForEpic().filter((tool) => !tools.has(tool))
  const missingCliCommands = requiredBossCliCommandsForEpic().filter((cmd) => !commands.has(cmd))

  // CLI first: a complete CLI set wins even when MCP is also complete.
  if (missingCliCommands.length === 0) {
    return {
      ok: true,
      transport: 'cli',
      missing: [],
      degraded: bossCliDegradedCapabilities(),
      partial: bossCliPartialCapabilities(),
      missingTools,
      missingCliCommands,
    }
  }
  if (missingTools.length === 0) {
    return {
      ok: true,
      transport: 'mcp',
      missing: [],
      degraded: [],
      // The MCP responses are complete, so there is nothing to warn about.
      partial: [],
      missingTools,
      missingCliCommands,
    }
  }
  return {
    ok: false,
    transport: null,
    missing: [...missingTools, ...missingCliCommands].sort(),
    degraded: [],
    // No usable transport, so nothing to be partial about.
    partial: [],
    missingTools,
    missingCliCommands,
  }
}

/**
 * Build the boss session-runner adapter: the declarative operation map plus the
 * resolved sub-skill commands the runner fans out per ticket.
 * @returns {import('./adapter.mjs').SessionRunnerAdapter}
 */
export function createBossSessionRunnerAdapter() {
  return {
    runner: 'boss',
    operationMap: bossSessionOperationMap,
    subSkills: {
      implement: bossSessionOperationMap.dispatchImplement.subSkill,
      repair: bossSessionOperationMap.dispatchRepair.subSkill,
    },
  }
}
