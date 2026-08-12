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

/**
 * @typedef {object} TrackerMcpPreflightResult
 * @property {boolean} ok               true only when the probe succeeded
 * @property {'ok'|'absent'|'unreachable'} status
 * @property {string} mcpServer         the server name the skill config named
 * @property {string} agent             the harness, or 'unknown harness'
 * @property {string[]} missing         expected tool names not found in this session
 * @property {string} message           empty when ok; otherwise the NO_CHANGE reason
 */

/**
 * @param {object} [input]
 * @param {Record<string, {tool?: string}>} [input.operationMap] the tracker adapter's operation map
 * @param {string} [input.mcpServer] the server name from the skill config's tracker block
 * @param {string} [input.agent] the harness running this session
 * @param {string[]} [input.availableTools] this session's own tool names
 * @param {boolean} [input.probeOk] did the cheap tracker read succeed?
 * @returns {TrackerMcpPreflightResult}
 */
export function trackerMcpPreflight({
  operationMap = {},
  mcpServer = '',
  agent = '',
  availableTools = [],
  probeOk = false,
} = {}) {
  const harness = String(agent).trim() === '' ? 'unknown harness' : String(agent)
  const expected = [
    ...new Set(
      Object.values(operationMap)
        .map((op) => op && op.tool)
        .filter((tool) => typeof tool === 'string' && tool !== ''),
    ),
  ].sort()

  // The probe wins. A harness that reaches the tracker some other way — one whose MCP tool
  // namespace is not `mcp__<server>__<op>`, or an adapter with a non-MCP carrier — passes on
  // probeOk alone, so tool-name matching can only ever EXPLAIN a failure, never cause one.
  if (probeOk) {
    return { ok: true, status: 'ok', mcpServer, agent: harness, missing: [], message: '' }
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
      `Expected tools: ${expected.join(', ') || '(none declared)'}.`,
  }
}
