#!/usr/bin/env node

// bs-repair-escalation.mjs — the escalation ladder for a residual that re-fires
// across repair rounds.
//
// A residual nobody acts on comes back byte-identically on the next round. The
// round re-reports it, consumes a repair attempt, and changes nothing. The
// decision this module owns is: given a residual identity and how many rounds
// have already reported it, does this round report it, mark it repeating,
// escalate to a different strategy, or hand it to a human?
//
// The decision lives here rather than in instruction prose because a ladder
// written as prose cannot be tested, and an untested ladder is re-decided from
// scratch by every reader. The skill body carries only the call and a branch
// table; it never re-derives a rung.
//
// IDENTITY. "The same thing again" is the `[file, line, title]` tuple the
// review-side oscillation guard already keys on, so review and repair share one
// notion of sameness instead of minting a second. Sharing the tuple is not enough
// on its own — the ENCODING has to match too, and the guard writes a line-less
// finding as the string 'null'. `residualIdentityKey` mirrors that rather than
// passing a raw null through, because a whole-file finding is the residual class
// most likely to re-fire and a byte-different key for it would make the shared
// notion of sameness false precisely where it matters.
//
// What the identity is NOT stable against: the line number. A round whose fix
// inserts lines above the residual re-derives a different key, so the count
// restarts at zero and the repeat reads as a first sighting. That is a real limit,
// not a bug this module can close from here — nothing in a tuple tells you it used
// to sit six lines lower. A caller that needs the count to survive its own edits
// reuses the recorded key string rather than re-deriving a tuple from the moved
// file; the skill body carries that instruction beside the call. That reuse only
// works if the key ROUND-TRIPS, so the accept side takes back the `'null'` the emit
// side writes — refusing it would break replay for the one class that needs it most.
//
// A malformed identity THROWS rather than defaulting, and the CLI refuses a count
// or an actionable token it cannot read rather than coercing one: a silent default
// would make every repeat look like a first sighting, which is precisely the
// failure this ladder exists to end.
//
// ACTION VOCABULARY. The terminal rung reports the shipped terminal token
// `blocked`, imported from the run-sentinel module rather than retyped, so a
// caller that classifies a repair outcome already understands it. The three
// lower rungs are non-terminal — the round keeps going — so they carry names of
// their own. That is an extension of the shipped vocabulary, never a parallel
// one: no rung invents a second spelling for an outcome the sentinel already
// names.
//
// Node built-ins only — unattended worktrees are dependency-free.

import { readFileSync } from 'node:fs'

import { REPAIR_RESULTS } from './bs-run-sentinel.mjs'
import { isMainModule } from './main-module.mjs'

/**
 * How many prior sightings of the same identity make a residual an escalation
 * rather than churn. Exported so it is tunable and testable without touching
 * prose. Two is the smallest value that leaves the "repeating but not yet
 * escalating" rung reachable: at one, a residual would jump from first sighting
 * straight to escalation and ordinary churn would escalate.
 */
export const ESCALATE_AFTER_OCCURRENCES = 2

/**
 * The ceiling. At or above this many prior sightings the residual goes to a human
 * regardless of what the caller claims about `actionable`.
 *
 * Without a ceiling the ladder is not a stopping rule. `actionable` is the sole
 * discriminator between "keep going" and "hand it over", it is supplied by the
 * same caller that benefits from answering `true`, and nothing here can check it —
 * so a residual could re-fire without limit and never reach the terminal rung the
 * ladder exists to reach. Four is chosen against the shipped 5-pass repair bound:
 * it leaves the escalate rung two sightings to work in and still fires inside a
 * single bounded run rather than only in a run nobody schedules.
 */
export const BLOCK_AFTER_OCCURRENCES = 4

if (!(BLOCK_AFTER_OCCURRENCES > ESCALATE_AFTER_OCCURRENCES)) {
  throw new Error(
    `bs-repair-escalation: the ceiling (${BLOCK_AFTER_OCCURRENCES}) must sit above the escalation threshold (${ESCALATE_AFTER_OCCURRENCES}), or the escalate rung is unreachable`,
  )
}

/** The identity tuple's arity. A tuple of any other length is malformed. */
export const RESIDUAL_IDENTITY_ARITY = 3

// The action the terminal rung reports. Derived from the shipped terminal
// vocabulary rather than retyped, and checked at module load: if that vocabulary
// ever stops carrying the token, this module fails to import instead of silently
// emitting an action no caller classifies.
const BLOCKED_ACTION = 'blocked'
if (!REPAIR_RESULTS.includes(BLOCKED_ACTION)) {
  throw new Error(
    `bs-repair-escalation: terminal action ${BLOCKED_ACTION} is not in the shipped terminal vocabulary (${REPAIR_RESULTS.join(', ')})`,
  )
}

// Rung 0 — first sighting. Nothing has re-fired; the residual is reported the
// way any residual is reported and the round proceeds.
export const RUNG_FIRST_SIGHTING = 0
export const ACTION_REPORT = 'report'
export const REASON_FIRST_SIGHTING = 'first-sighting: no prior round reported this identity'

// Rung 1 — repeating, below the escalation threshold. The residual is reported
// AND marked repeating, so the next round reads a repeat rather than re-deriving
// it. Marking is the whole point: an unmarked repeat is indistinguishable from a
// first sighting in the next round's report.
export const RUNG_REPEATING = 1
export const ACTION_REPORT_REPEATING = 'report-repeating'
export const REASON_REPEATING_BELOW_THRESHOLD =
  'repeating-below-threshold: seen before but under the escalation threshold'

// Rung 2 — at or above the threshold, and this pass can act. Re-reporting it
// unchanged would consume a repair attempt and change nothing, so the round
// changes strategy instead of repeating the one that already failed twice.
export const RUNG_ESCALATE = 2
export const ACTION_ESCALATE_STRATEGY = 'escalate-strategy'
export const REASON_REPEATING_ACTIONABLE =
  'repeating-actionable: at the escalation threshold with an action this pass can take'

// Rung 3 — at or above the threshold with nothing this pass can do. This is the
// terminal rung and the only one that reports a shipped terminal token.
export const RUNG_BLOCKED = 3
export const ACTION_BLOCKED = BLOCKED_ACTION
export const REASON_REPEATING_NOT_ACTIONABLE =
  'repeating-not-actionable: at the escalation threshold with no action this pass can take'
// The same terminal rung, reached the other way: the ceiling overrides `actionable`
// rather than trusting it. The reason is distinct because "you said you could act and
// you have now said so four times" is a different fact from "you said you could not".
export const REASON_CEILING_REACHED =
  'ceiling-reached: re-fired past the ceiling, so a changed strategy is no longer an answer'

/** Every action this module can return, in rung order. */
export const ESCALATION_ACTIONS = [
  ACTION_REPORT,
  ACTION_REPORT_REPEATING,
  ACTION_ESCALATE_STRATEGY,
  ACTION_BLOCKED,
]

/** Every reason this module can return, in rung order. */
export const ESCALATION_REASONS = [
  REASON_FIRST_SIGHTING,
  REASON_REPEATING_BELOW_THRESHOLD,
  REASON_REPEATING_ACTIONABLE,
  REASON_REPEATING_NOT_ACTIONABLE,
  REASON_CEILING_REACHED,
]

/**
 * The encoding the oscillation guard gives a line-less finding. `bs-review-caps.mjs`
 * builds its key as
 *
 *   Number.isInteger(finding.line) ? finding.line : finding.line === null ? 'null' : ''
 *
 * so a whole-file finding keys on the STRING `'null'`, not on JSON null. Passing the
 * raw null through here would produce `["a.go",null,"t"]` against the guard's
 * `["a.go","null","t"]` — byte-different keys for the same finding, in exactly the
 * class most likely to re-fire. Mirroring it is what makes "one notion of sameness"
 * a fact rather than a claim.
 */
export const IDENTITY_NO_LINE = 'null'

/**
 * Normalise a residual identity to the byte-stable key string the oscillation
 * guard uses: `JSON.stringify([file, line, title])`, with a line-less finding
 * carrying `IDENTITY_NO_LINE` exactly as the guard encodes it.
 *
 * Accepts either the 3-tuple `[file, line, title]` or the `{file, line, title}`
 * object a finding already carries. `line` is an integer, `null`, or the
 * `IDENTITY_NO_LINE` string this function itself emits for a line-less finding —
 * a recorded key parsed back with `JSON.parse` must be re-keyable, so the accept
 * side takes exactly what the emit side wrote. Anything else THROWS.
 *
 * `file` and `title` are trimmed before keying. The identity is hand-typed from a
 * prose template, so `"a.go"` and `"a.go "` name the same finding; keying them
 * apart would reset a repeat count on an invisible character.
 *
 * @param {[string, number|null|'null', string]|{file: string, line: number|null|'null', title: string}} identity
 * @returns {string}
 */
export function residualIdentityKey(identity) {
  let file
  let line
  let title
  if (Array.isArray(identity)) {
    if (identity.length !== RESIDUAL_IDENTITY_ARITY) {
      throw new TypeError(
        `residual identity must be a ${RESIDUAL_IDENTITY_ARITY}-tuple [file, line, title], got arity ${identity.length}`,
      )
    }
    ;[file, line, title] = identity
  } else if (identity !== null && typeof identity === 'object') {
    ;({ file, line, title } = identity)
  } else {
    throw new TypeError('residual identity must be a [file, line, title] tuple or an object')
  }

  if (typeof file !== 'string' || file.trim() === '') {
    throw new TypeError('residual identity is missing a non-empty file')
  }
  if (typeof title !== 'string' || title.trim() === '') {
    throw new TypeError('residual identity is missing a non-empty title')
  }
  // The emit and accept sides have to be symmetric, or a recorded key cannot be replayed. This
  // function WRITES a line-less finding as the string IDENTITY_NO_LINE, and the skill body tells a
  // later round to copy that recorded key verbatim rather than re-derive a tuple from a file whose
  // lines have moved. Refusing our own output here would make the replay path exit non-zero for
  // exactly the whole-file class this module's header calls the one most likely to re-fire.
  if (line === IDENTITY_NO_LINE) line = null
  if (line !== null && !Number.isInteger(line)) {
    throw new TypeError('residual identity line must be an integer or null')
  }
  return JSON.stringify([file.trim(), line === null ? IDENTITY_NO_LINE : line, title.trim()])
}

/**
 * The ladder. Returns `{identity, rung, action, reason}` for one residual.
 *
 * `priorOccurrences` is how many EARLIER rounds reported this identity, not
 * counting this one — zero on a first sighting. `actionable` is whether a
 * DIFFERENT strategy from the one already applied to this identity is available and
 * nameable — not merely that some action exists, which re-running the strategy that
 * already failed would also satisfy. That narrow reading is the definition; the skill
 * body states it in the same words beside the call, because this flag is the sole
 * rung-2-vs-rung-3 discriminator and a looser reading here would defer the terminal
 * rung the ceiling exists to bound. It is consulted only between the threshold and
 * the ceiling — below the threshold the round reports either way, and at or above
 * the ceiling the residual goes to a human whatever the caller claims.
 *
 * Fails closed on every input: a malformed identity or a non-integer,
 * negative `priorOccurrences` throws rather than resolving to rung 0.
 *
 * @param {{identity: unknown, priorOccurrences: number, actionable?: boolean}} input
 * @returns {{identity: string, rung: number, action: string, reason: string}}
 */
export function classifyResidual({ identity, priorOccurrences, actionable = false } = {}) {
  const key = residualIdentityKey(identity)
  if (!Number.isInteger(priorOccurrences) || priorOccurrences < 0) {
    throw new TypeError('priorOccurrences must be a non-negative integer')
  }

  if (priorOccurrences === 0) {
    return {
      identity: key,
      rung: RUNG_FIRST_SIGHTING,
      action: ACTION_REPORT,
      reason: REASON_FIRST_SIGHTING,
    }
  }
  if (priorOccurrences < ESCALATE_AFTER_OCCURRENCES) {
    return {
      identity: key,
      rung: RUNG_REPEATING,
      action: ACTION_REPORT_REPEATING,
      reason: REASON_REPEATING_BELOW_THRESHOLD,
    }
  }
  if (priorOccurrences >= BLOCK_AFTER_OCCURRENCES) {
    // The ceiling, checked BEFORE `actionable`. This is the one branch the caller
    // cannot talk its way past, and it is what makes the ladder terminate.
    return {
      identity: key,
      rung: RUNG_BLOCKED,
      action: ACTION_BLOCKED,
      reason: REASON_CEILING_REACHED,
    }
  }
  if (actionable === true) {
    return {
      identity: key,
      rung: RUNG_ESCALATE,
      action: ACTION_ESCALATE_STRATEGY,
      reason: REASON_REPEATING_ACTIONABLE,
    }
  }
  return {
    identity: key,
    rung: RUNG_BLOCKED,
    action: ACTION_BLOCKED,
    reason: REASON_REPEATING_NOT_ACTIONABLE,
  }
}

const USAGE = `usage: bs-repair-escalation.mjs classify --in <path> <priorOccurrences> [actionable]
       bs-repair-escalation.mjs classify <identityJson> <priorOccurrences> [actionable]
  --in <path>        a file holding the ["file", line, "title"] tuple as JSON. PREFER THIS:
                     a title is arbitrary English prose, and an apostrophe in it ends a
                     single-quoted shell argument early, so splicing one into a literal
                     produces a broken command whose only repair looks like rewording the
                     title — and a reworded title is a different identity, which resets
                     the very count this module exists to keep.
  identityJson       the same tuple inline, for a caller that can quote it safely
  priorOccurrences   how many earlier rounds reported this identity (0 on a first sighting)
  actionable         "true" or "false" (default false) — true ONLY when a DIFFERENT strategy from
                     the one already applied to this identity is available and you can name it.
                     Re-running the strategy that already failed is not a different strategy.
  prints {"identity","rung","action","reason"} on stdout`

/** Exactly the shape a decimal count arrives in. No sign, no padding, no radix prefix. */
const DECIMAL_COUNT = /^\d+$/

/**
 * CLI entry point. RETURNS an exit code and never calls process.exit, so a
 * caller can test it in-process: 0 on success, 2 on a usage or input error.
 * Nothing is written to stdout on the error path.
 */
export function runCli(argv = [], io = {}) {
  const stdout = io.stdout ?? ((text) => process.stdout.write(text))
  const stderr = io.stderr ?? ((text) => process.stderr.write(text))
  const readFile = io.readFile ?? ((path) => readFileSync(path, 'utf8'))

  const usesFile = argv[1] === '--in'
  const [command, , identityPath] = argv
  const rest = usesFile ? argv.slice(3) : argv.slice(2)
  const identityJson = usesFile ? undefined : argv[1]
  const [priorRaw, actionableRaw] = rest

  if (command !== 'classify') {
    stderr(
      `${command === undefined ? 'missing command' : `unknown command: ${command}`}\n${USAGE}\n`,
    )
    return 2
  }
  if (usesFile && typeof identityPath !== 'string') {
    stderr(`--in requires a path\n${USAGE}\n`)
    return 2
  }
  if (!usesFile && typeof identityJson !== 'string') {
    stderr(`classify requires <identityJson> <priorOccurrences>\n${USAGE}\n`)
    return 2
  }
  if (typeof priorRaw !== 'string') {
    stderr(`classify requires <identityJson> <priorOccurrences>\n${USAGE}\n`)
    return 2
  }

  // Validate the RAW strings before any coercion. `Number('')`, `Number('  ')` and `Number('+0')`
  // are all 0 — a non-negative integer classifyResidual happily accepts — so a coerce-first CLI
  // answers "first sighting" for the empty value an unset shell variable most easily produces,
  // which is exactly the silent default this ladder exists to end. In the other direction
  // `Number(' 2 ')`, `Number('0x2')` and `Number('1e2')` all parse, routing a padded, hex or
  // exponent token to a rung nobody typed. Both arms are refusals, not defaults.
  if (!DECIMAL_COUNT.test(priorRaw)) {
    stderr(
      `priorOccurrences must be a non-negative decimal integer, got ${JSON.stringify(priorRaw)}\n${USAGE}\n`,
    )
    return 2
  }
  // `actionableRaw === 'true'` alone reads every unrecognised token — '1', 'yes', 'TRUE' — as
  // false, which routes an actionable repeat to the human rung instead of refusing the typo.
  if (actionableRaw !== undefined && actionableRaw !== 'true' && actionableRaw !== 'false') {
    stderr(`actionable must be "true" or "false", got ${JSON.stringify(actionableRaw)}\n${USAGE}\n`)
    return 2
  }
  if (rest.length > 2) {
    stderr(`classify takes at most <priorOccurrences> and <actionable>\n${USAGE}\n`)
    return 2
  }

  let identityText = identityJson
  if (usesFile) {
    try {
      identityText = readFile(identityPath)
    } catch (err) {
      stderr(`cannot read identity file ${identityPath}: ${err.message}\n`)
      return 2
    }
  }

  let identity
  try {
    identity = JSON.parse(identityText)
  } catch (err) {
    stderr(`identity is not valid JSON: ${err.message}\n`)
    return 2
  }

  let verdict
  try {
    verdict = classifyResidual({
      identity,
      priorOccurrences: Number(priorRaw),
      actionable: actionableRaw === 'true',
    })
  } catch (err) {
    stderr(`${err.message}\n`)
    return 2
  }
  stdout(`${JSON.stringify(verdict)}\n`)
  return 0
}

if (isMainModule(import.meta.url)) {
  process.exitCode = runCli(process.argv.slice(2))
}
