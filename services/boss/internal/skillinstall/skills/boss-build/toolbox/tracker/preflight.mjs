// skills-toolbox/tracker/preflight.mjs — classify whether this session can reach its tracker's
// MCP server, and if not, say which of the two very different causes it is.
//
// The session runner does not configure MCP servers. Each agent harness discovers them its own
// native way and the repository is responsible for declaring them under the name its skill config
// names. The failure that arrangement introduces is a repo that has not configured the harness it
// is running, which without this helper surfaces mid-run as a baffling "tool not found". This turns
// it into a preflight stop that names the expected server and the harness, and separates:
//
//   - absent      — the repo never declared this server for this harness (or declared it without
//                   enabling it, where the harness has a separate approval step). Fix the REPO.
//   - unreachable — the declaration is there and the server did not answer. Fix CREDENTIALS or
//                   NETWORK.
//
// It is deliberately a pure function over facts the caller gathers. It never reads a config file
// and never knows any harness's config path — the caller passes the session's own tool list, which
// is the one harness-agnostic way to ask "what do I actually have?".
//
// The tool list alone has one blind spot, and it is exactly on the boundary the two causes divide:
// a server that fails at CONNECT publishes no tools, so its tool set is empty and indistinguishable
// from one that was never declared — sending the reader to fix a declaration that is already
// correct. So the caller may ALSO supply a declaration report: whatever its host can say about the
// servers this session resolved. That input is strictly optional and defaults to no signal, because
// every already-installed caller passes none and must keep today's behaviour to the byte.

/**
 * A per-server record from the caller's host. Field names mirror the host's own JSON so the caller
 * does no interpretation; both `toolCount` and `tool_count` spellings are accepted.
 * @typedef {object} DeclaredServerRecord
 * @property {string} name              the server name as the harness resolved it
 * @property {number} [toolCount]       how many tools it published (0 is the interesting case)
 * @property {string} [authStatus]      the harness's own auth verdict, when it has one
 */

/**
 * @typedef {object} TrackerMcpPreflightResult
 * @property {boolean} ok               true only when the probe succeeded
 * @property {'ok'|'absent'|'unreachable'} status
 * @property {string} mcpServer         the server name the skill config named
 * @property {string} agent             the harness, or 'unknown harness'
 * @property {string[]} missing         expected tool names not found in this session
 * @property {string} message           empty when ok; otherwise the NO_CHANGE reason
 * @property {boolean|null} declared    what the supplied declaration report said about this server:
 *                                      true it named it, false it did not, null none was supplied.
 *                                      It reports the EVIDENCE, not which branch decided the
 *                                      verdict — so a run log can say what the call actually knew.
 */

/**
 * @param {object} [input]
 * @param {Record<string, {tool?: string}>} [input.operationMap] the tracker adapter's operation map
 * @param {string} [input.mcpServer] the server name from the skill config's tracker block
 * @param {string} [input.agent] the harness running this session
 * @param {string[]} [input.availableTools] this session's own tool names
 * @param {boolean} [input.probeOk] did the cheap tracker read succeed?
 * @param {DeclaredServerRecord[]} [input.declaredServers] optional: the servers this session
 *   resolved, as the caller's host reports them. Anything that is not a non-empty array is treated
 *   as no signal, which is exactly today's behaviour.
 * @returns {TrackerMcpPreflightResult}
 */
export function trackerMcpPreflight({
  operationMap = {},
  mcpServer = '',
  agent = '',
  availableTools = [],
  probeOk = false,
  declaredServers = [],
} = {}) {
  const harness = String(agent).trim() === '' ? 'unknown harness' : String(agent)
  const expected = [
    ...new Set(
      Object.values(operationMap)
        .map((op) => op && op.tool)
        .filter((tool) => typeof tool === 'string' && tool !== ''),
    ),
  ].sort()

  // Read the optional declaration report before any verdict, so `declared` is auditable on EVERY
  // path including the passing one. Only a non-empty array is a signal: an omitted, null, empty or
  // non-array value leaves `declared` null and every branch below exactly as it was.
  const report = Array.isArray(declaredServers) ? declaredServers : []
  const hasReport = report.length > 0
  // Exact name matching, deliberately: a record differing by case or stray whitespace is how a
  // MIS-KEYED declaration presents, and calling that "declared" would hide the real bug. Malformed
  // records are skipped rather than thrown on — this is diagnostic code and must not add a failure
  // mode of its own to a session that is already failing.
  // An unconfigured `mcpServer` (the '' default) must never match, or a malformed record carrying
  // an empty name would report a server that has no name as "declared" and send the reader off to
  // check credentials for it. No configured name means there is nothing to look for.
  const record =
    hasReport && mcpServer !== ''
      ? report.find((entry) => entry && typeof entry.name === 'string' && entry.name === mcpServer)
      : undefined
  const declared = hasReport ? Boolean(record) : null

  // The probe wins. A harness that reaches the tracker some other way — one whose MCP tool
  // namespace is not `mcp__<server>__<op>`, or an adapter with a non-MCP carrier — passes on
  // probeOk alone, so tool-name matching can only ever EXPLAIN a failure, never cause one.
  if (probeOk) {
    return {
      ok: true,
      status: 'ok',
      mcpServer,
      agent: harness,
      missing: [],
      message: '',
      declared,
    }
  }

  const available = new Set(availableTools.map(String))
  // Match either namespacing convention: `mcp__<server>__<op>` (Claude Code) or `<server>__<op>`
  // (Codex). A harness whose tools are present under EITHER shape has been configured, so its
  // failure is a reachability problem and not a missing declaration.
  const missing = expected.filter(
    (tool) => !available.has(tool) && !available.has(tool.replace(/^mcp__/, '')),
  )
  // An empty operation map is not evidence of reachability: `missing.length < expected.length` is
  // false when both are 0, so a tracker adapter that declares no MCP tools at all falls to absent
  // rather than reporting a server it never looked for as present.
  const present = expected.length > 0 && missing.length < expected.length

  if (present) {
    return {
      ok: false,
      status: 'unreachable',
      mcpServer,
      agent: harness,
      missing,
      message:
        `tracker MCP server "${mcpServer}" is present in this session but unreachable ` +
        `(harness: ${harness}). The repo's declaration is fine — the server did not answer. ` +
        `Check the credential environment and network reachability for that server, not the ` +
        `declaration.`,
      declared,
    }
  }

  // Inserted AFTER the tool-visibility test on purpose: when tools ARE visible the branch above has
  // already decided, so this one only ever fires where the tool list is blind — the connect failure
  // that publishes nothing. The report is what turns that from a guess into a verdict.
  if (record) {
    const toolCount = firstNumber(record.toolCount, record.tool_count)
    const authStatus = firstString(record.authStatus, record.auth_status)
    const detail = [
      `harness: ${harness}`,
      toolCount === null ? '' : `${toolCount} tools`,
      authStatus === '' ? '' : `auth: ${authStatus}`,
    ]
      .filter(Boolean)
      .join(', ')
    return {
      ok: false,
      status: 'unreachable',
      mcpServer,
      agent: harness,
      missing,
      message:
        `tracker MCP server "${mcpServer}" is declared for this session but published no usable ` +
        `tools (${detail}). The repo's declaration is fine — the server did not answer. Check the ` +
        `credential environment and network reachability for that server, not the declaration.`,
      declared,
    }
  }

  return {
    ok: false,
    status: 'absent',
    mcpServer,
    agent: harness,
    missing,
    message:
      `tracker MCP server "${mcpServer}" is not present in this session (harness: ${harness}). ` +
      `MCP servers are not configured by the session runner — the repo must declare ` +
      `"${mcpServer}" through ${harness}'s own mechanism, and where that harness has a separate ` +
      `approval step it must be enabled there too. ` +
      `Expected tools: ${expected.join(', ') || '(none declared)'}.` +
      // Only ever appended when a report was supplied AND omitted the server, so a caller that
      // supplies nothing still reads the message above byte-for-byte.
      (declared === false ? ` The caller's declaration report lists no server by that name.` : ''),
    declared,
  }
}

/** First finite number among the candidates, or null when the caller supplied none. */
function firstNumber(...candidates) {
  for (const value of candidates)
    if (typeof value === 'number' && Number.isFinite(value)) return value
  return null
}

/** First non-empty string among the candidates, or '' when the caller supplied none. */
function firstString(...candidates) {
  for (const value of candidates) if (typeof value === 'string' && value !== '') return value
  return ''
}
