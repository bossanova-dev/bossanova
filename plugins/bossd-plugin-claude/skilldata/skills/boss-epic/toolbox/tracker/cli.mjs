// skills-toolbox/tracker/cli.mjs
// Thin shell entrypoint that routes the boss-build spine's executable claim
// helpers through the resolved tracker adapter, so the skill names a tracker-agnostic
// capability instead of a Linear-specific script. node builtins only (the cron
// worktree is dependency-free).
//
//   node tracker/cli.mjs claim-token
//     -> prints a fresh run token (stdout), tracker-agnostic 32-char hex
//   node tracker/cli.mjs claim-comment --token <token> [--session-id <id>]
//     -> prints the tracker-specific claim comment body; defaults session id to BOSS_SESSION_ID
//   node tracker/cli.mjs claim-verdict --me <token> --comments <json-array> [--liveness <json>]
//     -> exit 0 WON (my token is the first writer), exit 3 LOST, exit 4 NO_WINNER
//   node tracker/cli.mjs states
//     -> stdout: {"planned":"<name>","inProgress":"<name>|null","inReview":"<name>|null"}
//     The adapter is the PRIMARY authority for the tracker's workflow-state names, so a
//     repo wired through a vendored adapter never has to restate them in config. `states`
//     is an OPTIONAL capability: an adapter without it exits 2 with a diagnostic on stderr
//     and NOTHING on stdout, which is why callers invoke it as `... states 2>/dev/null ||
//     true` and fall back to their own `.boss-skills.json` read.
//   node tracker/cli.mjs update-comment --id <commentId> --body-file <path>
//     -> stdout: {"tool":"<adapter operationMap.updateComment.tool>","args":{"id":<commentId>,"body":<file contents>}}
//     The descriptor is emitted for the driver to execute through the tracker MCP —
//     drivers never issue raw GraphQL for the single-comment progress protocol.
//
// Verdict delegates to the resolved adapter's resolveClaim capability; the
// Linear reference impl computes first-writer-wins over the claim comments, optionally after
// liveness evidence forfeits claims whose owners are provably inactive.

import crypto from 'node:crypto'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { resolveTrackerAdapter } from './adapter.mjs'

// Tracker-agnostic run token: 16 random bytes as lowercase hex (32 chars) — the same
// shape the claim marker captures. Duplicated here rather than imported from the
// Linear claim helper so this dispatcher stays tracker-layer only.
export function generateClaimToken() {
  return crypto.randomBytes(16).toString('hex')
}

function parseFlags(rest) {
  const flags = {}
  for (let i = 0; i < rest.length; i += 2) flags[rest[i]?.replace(/^--/, '')] = rest[i + 1]
  return flags
}

function parseJsonFlag(value, label, errWrite) {
  try {
    return JSON.parse(value)
  } catch (err) {
    errWrite(`${label}: malformed JSON: ${err.message}\n`)
    return undefined
  }
}

/**
 * Dispatch one tracker capability. Returns the process exit code; never calls
 * process.exit directly so it is unit-testable.
 * @param {string[]} argv
 * @param {{write?: (s: string) => void, errWrite?: (s: string) => void, env?: object,
 *   resolveAdapter?: typeof resolveTrackerAdapter}} [io]
 *   `resolveAdapter` defaults to the real `resolveTrackerAdapter` import; tests inject a stub
 *   here to reach adapter shapes (e.g. one missing an operationMap entry) that the real,
 *   always-Linear-today registry can't produce.
 * @returns {number}
 */
export function runCli(
  argv,
  {
    write = (s) => process.stdout.write(s),
    errWrite = (s) => process.stderr.write(s),
    env = process.env,
    resolveAdapter = resolveTrackerAdapter,
  } = {},
) {
  const [cmd, ...rest] = argv
  if (cmd === 'claim-token') {
    write(generateClaimToken() + '\n')
    return 0
  }
  if (cmd === 'claim-comment') {
    const { token, 'session-id': sessionId } = parseFlags(rest)
    if (!token) {
      errWrite('claim-comment: --token <token> is required\n')
      return 2
    }
    const adapter = resolveAdapter({ env })
    try {
      write(adapter.formatClaimComment(token, sessionId ?? env.BOSS_SESSION_ID ?? null) + '\n')
    } catch (err) {
      errWrite(`claim-comment: could not format claim comment: ${err?.message ?? err}\n`)
      return 2
    }
    return 0
  }
  if (cmd === 'claim-verdict') {
    const { me, comments, liveness } = parseFlags(rest)
    if (!me) {
      errWrite('claim-verdict: --me <token> is required\n')
      return 2
    }
    // --comments is required (parity with the linear-claim.mjs verdict it replaces): an absent
    // list is a caller error, not a silent LOST. Only 0 (WON) / 3 (LOST) route the claim.
    if (comments === undefined) {
      errWrite('claim-verdict: --comments <json-array> is required\n')
      return 2
    }
    const parsedComments = parseJsonFlag(comments, 'claim-verdict: --comments', errWrite)
    if (parsedComments === undefined) return 2
    let livenessOptions = null
    if (liveness !== undefined) {
      livenessOptions = parseJsonFlag(liveness, 'claim-verdict: --liveness', errWrite)
      if (livenessOptions === undefined) return 2
    }
    const adapter = resolveAdapter({ env })
    let won
    try {
      won = adapter.resolveClaim(parsedComments, me, livenessOptions)
    } catch (err) {
      errWrite(`claim-verdict: claim arbitration failed: ${err?.message ?? err}\n`)
      return 2
    }
    if (won === null) {
      write('NO_WINNER\n')
      return 4
    }
    return won ? 0 : 3
  }
  if (cmd === 'states') {
    const adapter = resolveAdapter({ env })
    // Absent capability is a NORMAL outcome, not a crash: the caller's fallback path
    // is the whole reason `states` is optional. Exit 2 with a one-line diagnostic and
    // an empty stdout so `2>/dev/null || true` degrades to the config read cleanly —
    // never print a partial/empty map, which the caller would parse as an answer.
    if (typeof adapter?.states !== 'function') {
      errWrite('states: resolved tracker adapter has no states capability\n')
      return 2
    }
    // "Never throws" is the adapter contract, not something this CLI can assume: a
    // vendored adapter that violates it must still degrade to the caller's config
    // fallback rather than crash it. Same for a non-object return — JSON.stringify
    // would emit the literal `undefined`, which is neither valid JSON nor an empty
    // stdout, so a stricter caller than the SKILL's `try { JSON.parse } catch {}`
    // would mis-read it as an answer. Both collapse to the same exit-2 contract.
    let states
    try {
      states = adapter.states()
    } catch (err) {
      errWrite(`states: tracker adapter states capability threw: ${err?.message ?? err}\n`)
      return 2
    }
    if (!states || typeof states !== 'object') {
      errWrite('states: tracker adapter states capability returned a non-object\n')
      return 2
    }
    write(JSON.stringify(states) + '\n')
    return 0
  }
  if (cmd === 'update-comment') {
    const { id, 'body-file': bodyFile } = parseFlags(rest)
    if (!id) {
      errWrite('update-comment: --id <commentId> is required\n')
      return 2
    }
    if (!bodyFile) {
      errWrite('update-comment: --body-file <path> is required\n')
      return 2
    }
    let body
    try {
      body = readFileSync(bodyFile, 'utf8')
    } catch (err) {
      errWrite(`update-comment: could not read --body-file ${bodyFile}: ${err.message}\n`)
      return 2
    }
    // Same guard the progress-comment toolbox applies to its own upsert body: an
    // update carrying a blank body erases the target comment INCLUDING its marker
    // anchor line, so the next run matches nothing and posts a duplicate. Reject
    // it here too rather than emitting a descriptor that quietly does that.
    if (body.trim() === '') {
      errWrite(`update-comment: --body-file ${bodyFile} is empty; refusing to blank the comment\n`)
      return 2
    }
    const adapter = resolveAdapter({ env })
    const op = adapter.operationMap?.updateComment
    if (!op) {
      errWrite('update-comment: resolved tracker adapter has no updateComment operation\n')
      return 2
    }
    // An entry present but carrying no usable `tool` (`{}`, `{tool: ''}`) is as
    // unusable as an absent one — it would emit a descriptor naming no MCP tool
    // and exit 0, deferring the failure to whatever tried to execute it. Same
    // non-empty rule assertConforms applies to the operationMap.
    if (typeof op.tool !== 'string' || op.tool.trim() === '') {
      errWrite('update-comment: resolved tracker adapter updateComment operation has no tool\n')
      return 2
    }
    write(JSON.stringify({ tool: op.tool, args: { id, body } }) + '\n')
    return 0
  }
  errWrite(`unknown tracker capability: ${cmd ?? '(none)'}\n`)
  return 2
}

import { isMainModule } from '../main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)
if (invokedDirectly) {
  process.exit(runCli(process.argv.slice(2)))
}
