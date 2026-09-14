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
//   node tracker/cli.mjs write-description --id <issueId> --body-file <path>
//     -> stdout: {"tool":"<adapter operationMap.writeDescription.tool>","args":{"id":<issueId>,
//        "description":<file contents>},"bytes":<size of the file ON DISK>,"outcome":"descriptor-emitted"}
//     The file-based description write: the caller composes and gates the description as a
//     file, and those same bytes reach the tracker without ever being retyped into a tool
//     argument. `bytes` is measured here with stat(2) rather than counted from the decoded
//     string, so a multi-byte body reports its true size; a caller must never report a size
//     it derived itself. `outcome` is the explicit success token the caller branches on, so
//     a write that changed nothing cannot read as success.
//     writeDescription is an OPTIONAL operation: an adapter that does not declare it exits 2
//     with a diagnostic naming the missing capability and NOTHING on stdout, so the caller
//     falls back to sending the description inline on its existing save.
//
// Verdict delegates to the resolved adapter's resolveClaim capability; the
// Linear reference impl computes first-writer-wins over the claim comments, optionally after
// liveness evidence forfeits claims whose owners are provably inactive.

import crypto from 'node:crypto'
import { readFileSync, statSync } from 'node:fs'
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

// The capability surface, printed by `--help` and appended to the unknown-capability
// rejection. It used to exist only as the dispatch chain below, so the accepted
// capabilities could be learned only by reading this file — and `--help` itself fell
// through to `unknown tracker capability: --help` with exit 2 and no list at all.
// tracker/cli.test.mjs derives the expected names from the dispatch chain's own
// `cmd === '...'` literals, so a capability cannot be added without appearing here.
export const TRACKER_USAGE = `usage: node tracker/cli.mjs <capability> [flags]

capabilities:
  claim-token
      Print a fresh tracker-agnostic run token (32-char hex) on stdout.

  claim-comment --token <token> [--session-id <id>]
      Print the tracker-specific claim comment body. Session id defaults to
      BOSS_SESSION_ID.

  claim-verdict --me <token> --comments <json-array> [--liveness <json>]
      Resolve first-writer-wins over the claim comments.
      exit 0 WON | exit 3 LOST | exit 4 NO_WINNER.

  states
      Print {"planned":...,"inProgress":...,"inReview":...} for the resolved adapter.
      OPTIONAL: an adapter without it exits 2 with a diagnostic and no stdout.

  update-comment --id <commentId> --body-file <path>
      Print the MCP tool descriptor for a single-comment progress update.

  write-description --id <issueId> --body-file <path>
      Print the MCP tool descriptor for a file-sourced description write.
      OPTIONAL: an adapter without it exits 2 and the caller sends inline instead.

  --help, -h, help
      Print this message and exit 0.
`

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
  // Resolved BEFORE the dispatch chain so `--help` cannot fall through to the
  // unknown-capability rejection it used to answer with.
  if (cmd === '--help' || cmd === '-h' || cmd === 'help') {
    write(TRACKER_USAGE)
    return 0
  }
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
    // Normalized HERE, at the flag, not left to surface through `resolveClaim`'s catch below. The
    // tracker's `list_comments` returns `{comments:[…]}`, and that envelope used to reach
    // `for (const c of comments)` and throw `comments is not iterable`, which this CLI reported as
    // `claim arbitration failed` — a diagnostic naming malformed EVIDENCE when the fault was an
    // argument shape. Both the bare array and the envelope are accepted; anything else is refused by
    // name against the flag that carried it.
    let commentList
    if (Array.isArray(parsedComments)) {
      commentList = parsedComments
    } else if (
      parsedComments !== null &&
      typeof parsedComments === 'object' &&
      Array.isArray(parsedComments.comments)
    ) {
      commentList = parsedComments.comments
    } else {
      // Names the KEYS an object carried, matching `normalizeClaimComments`'s descriptor in
      // `linear-claim.mjs` — the library this flag fronts. A bare `a object` discarded exactly the
      // detail that identifies the remaining likely misuse: `{nodes:[…]}`, the GraphQL spelling of
      // the same payload.
      errWrite(
        `claim-verdict: --comments (a JSON comment ARRAY, or the tracker's {"comments":[…]} envelope) — got ${
          parsedComments === null
            ? 'null'
            : typeof parsedComments === 'object'
              ? `an object with keys ${Object.keys(parsedComments).join(', ')}`
              : `a ${typeof parsedComments}`
        }\n`,
      )
      return 2
    }
    let livenessOptions = null
    if (liveness !== undefined) {
      livenessOptions = parseJsonFlag(liveness, 'claim-verdict: --liveness', errWrite)
      if (livenessOptions === undefined) return 2
    }
    const adapter = resolveAdapter({ env })
    let won
    try {
      won = adapter.resolveClaim(commentList, me, livenessOptions)
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
  if (cmd === 'write-description') {
    const { id, 'body-file': bodyFile } = parseFlags(rest)
    if (!id) {
      errWrite('write-description: --id <issueId> is required\n')
      return 2
    }
    if (!bodyFile) {
      errWrite('write-description: --body-file <path> is required\n')
      return 2
    }
    let body
    let bytes
    try {
      body = readFileSync(bodyFile, 'utf8')
      // Measured with stat(2), not Buffer.byteLength over the decoded string: the
      // contract is "the bytes on disk", and the two disagree the moment a body is
      // re-encoded or carries a lone surrogate. A caller that re-derived this number
      // itself would be reporting its own belief about a file it never opened.
      bytes = statSync(bodyFile).size
    } catch (err) {
      errWrite(`write-description: could not read --body-file ${bodyFile}: ${err.message}\n`)
      return 2
    }
    // The most destructive input this verb can receive. update-comment refuses a blank
    // body because it would erase a comment's marker anchor; a blank DESCRIPTION erases
    // the only surviving copy of the reporter's original notes, because the tracker
    // exposes no description history to recover them from. Fail closed.
    if (body.trim() === '') {
      errWrite(
        `write-description: --body-file ${bodyFile} is empty; refusing to blank the description\n`,
      )
      return 2
    }
    // The other way the bytes on disk can stop being the bytes that reach the tracker.
    // readFileSync(..., 'utf8') does NOT throw on malformed input — it substitutes U+FFFD
    // — so without this check `description` would carry the corruption while `bytes` still
    // attests to the intact on-disk size and `outcome` still reads `descriptor-emitted`.
    // The write replaces the whole description and the tracker keeps no history, so that
    // corruption is unrecoverable. Re-encoding a cleanly decoded body always reproduces
    // the file, so a mismatch means the decode was lossy (or the file changed under us
    // between read and stat). Fail closed either way.
    const decodedBytes = Buffer.byteLength(body, 'utf8')
    if (decodedBytes !== bytes) {
      errWrite(
        `write-description: --body-file ${bodyFile} is not valid UTF-8 (${bytes} bytes on disk, ` +
          `${decodedBytes} after decoding); refusing to write a corrupted description\n`,
      )
      return 2
    }
    const adapter = resolveAdapter({ env })
    const op = adapter.operationMap?.writeDescription
    if (!op) {
      errWrite(
        'write-description: resolved tracker adapter has no writeDescription operation; send the description inline instead\n',
      )
      return 2
    }
    if (typeof op.tool !== 'string' || op.tool.trim() === '') {
      errWrite(
        'write-description: resolved tracker adapter writeDescription operation has no tool\n',
      )
      return 2
    }
    write(
      JSON.stringify({
        tool: op.tool,
        args: { id, description: body },
        bytes,
        outcome: 'descriptor-emitted',
      }) + '\n',
    )
    return 0
  }
  errWrite(`unknown tracker capability: ${cmd ?? '(none)'}\n`)
  // The capability list goes out on the rejection too: a caller who guessed wrong
  // learns the real set here rather than having to read this file.
  errWrite(TRACKER_USAGE)
  return 2
}

import { isMainModule } from '../main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)
if (invokedDirectly) {
  process.exit(runCli(process.argv.slice(2)))
}
