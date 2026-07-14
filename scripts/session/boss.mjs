// scripts/session/boss.mjs
// Boss reference implementation of the session-runner-adapter interface.
// Preserves current boss-epic behaviour exactly: it reimplements nothing, it
// DOCUMENTS. Each capability names the boss MCP tool + the arg/response fields
// the boss-epic prose already uses, so the SKILL has one source of truth
// instead of hard-coding tool names inline. The agent still issues the MCP
// calls; this map just centralizes them (mirrors scripts/tracker/linear.mjs'
// linearOperationMap — see that ticket's Open Questions). node builtins only.
//
// Sub-skill dispatch resolves to the current Bossanova sub-skills. Note the
// reconciliation: the BOS-198 plan/acceptance-criteria wrote `/bs-implement`,
// but the BOS-194 rename (bs-* -> boss-*) is merged, so today's exact implement
// sub-skill is `/boss-build` (the boss-epic SKILL dispatches
// `/boss-build BOS-NN`). Using `/bs-implement` would resolve to a
// non-existent skill and BREAK the zero-behaviour-change bar; `/boss-repair`
// was already renamed and is unchanged.

// capability -> { tool, args:[...], response:[...] } (boss MCP choreography) OR
// capability -> { subSkill, args:[...] } (per-ticket fan-out dispatch).
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
  },
  createPlanningChat: {
    tool: 'create_session',
    // BOS-322: visible planning-only work (recon, plan review, an intentionally
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
  },
  mergeSession: {
    tool: 'merge_session',
    // confirm:true is MANDATORY — merge_session refuses without it. Serialized:
    // at most one in flight, only in nextToMerge order.
    args: ['id', 'confirm'],
    response: ['state'],
  },
  resolveContext: {
    tool: 'resolve_context',
    args: ['working_dir'],
    // ResolveContextResponse nests the resolved repo under `repo`; the id is
    // `repo.id`, NOT a top-level `repo_id`. Following the flat key leaves the
    // repo id empty even inside a registered checkout (codex P2 on PR #1112).
    response: ['repo.id'],
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
  },
  dispatchImplement: {
    // Reconciled from the plan's pre-rename `/bs-implement` — see file header.
    subSkill: '/boss-build',
    // dispatched as the BARE single-line prompt "/boss-build BOS-NN".
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
  // so an unreadable status (BOS-244 restart window) is treated as not-settled,
  // not merged; get_session_statuses is the session-aggregate display/diagnostic
  // signal only (never gate green/settled on it — SKILL Phase 3b).
  recordChat: {
    tool: 'record_chat',
    args: ['session_id', 'agent_session_id', 'agent_name', 'title'],
    response: [],
  },
  sendChatMessage: {
    tool: 'send_chat_message',
    args: ['agent_session_id', 'wake_if_asleep', 'submit', 'message'],
    response: [],
  },
  getChatStatuses: {
    tool: 'get_chat_statuses',
    // get_chat_statuses keys on a single `session_id` (GetChatStatusesRequest);
    // the `statuses` array holds one ChatStatusEntry per chat (agent_session_id
    // + status). Phase 3b reads the entry whose agent_session_id == the ticket's
    // tracked chat to gate green-on-settled — this is the tool the run actually
    // invokes every poll, so it MUST be a mapped capability (and thus land in the
    // discovery-preflight set). Its earlier omission from the map was itself the
    // BOS-301 late-discovery failure class for the merge gate.
    args: ['session_id'],
    response: ['statuses'],
  },
  getSessionStatuses: {
    tool: 'get_session_statuses',
    // get_session_statuses takes a plural `session_ids` list (SessionIDsArgs),
    // not a single `id`. Session-aggregate, display/diagnostic only — NOT the
    // green/settled gate (that is get_chat_statuses above).
    args: ['session_ids'],
    response: ['statuses'],
  },
}

/**
 * The distinct boss MCP tool names an epic run can invoke, derived from
 * bossSessionOperationMap (the single source of truth) instead of a hand-kept
 * duplicate list. This is boss-epic's deterministic *discovery-preflight* set
 * (BOS-301): a long unattended run must be able to SEE every one of these tools
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
 * Deterministic boss-epic tool-discovery preflight (BOS-301). Given the boss MCP
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
  const available = new Set(availableTools)
  const missing = requiredBossToolsForEpic().filter((tool) => !available.has(tool))
  return { ok: missing.length === 0, missing }
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
