#!/usr/bin/env node

// bs-repair-derivations.mjs — the repair-round derivations whose answer has to reach the report.
//
// Four decisions share one shape: the round runs a derivation, and the answer either never reaches
// the Repair Summary or never gets taken at all. In every case the omission renders byte-identically
// to the benign outcome — a round that forgot to push reads exactly like a round with nothing to
// push, and a reply citing a follow-up record reads exactly like a reply whose record exists. A
// report that cannot tell ran-and-found-nothing from never-ran is not a report.
//
// The decisions live here rather than in instruction prose for the reason the escalation ladder in
// bs-repair-escalation.mjs already establishes: a derivation written as prose can be skipped
// silently and cannot be tested, and an untested derivation is re-decided from scratch by every
// reader. The skill body carries the call, the verdicts, and the action per verdict — never a
// restatement of the derivation.
//
// FAILING CLOSED, CONCRETELY. Every classifier here has a quiet arm and a loud arm, and the quiet
// arm is always the one that lets the round say nothing: `published` owes no push, `None` names no
// problem, a resolved `boss` binary needs no caveat, and an admitted reply needs no record. So
// unreadable input never takes the quiet arm. That is the whole discipline — not "throw on bad
// input", but "when in doubt, force the round to report".
//
// Node built-ins only — unattended worktrees are dependency-free.

import { readFileSync } from 'node:fs'

import { resolveBossBinary } from './boss-binary.mjs'
import { isMainModule } from './main-module.mjs'

// ---------------------------------------------------------------------------
// Push state — Phase 3's four-arm derivation, plus the withheld case the body
// already carves out, given a report destination.

/** Local and origin agree. The only state that owes nothing AND claims nothing. */
export const PUSH_STATE_PUBLISHED = 'published'
/** Origin is ahead: a peer pushed past this worktree. Do not push; re-derive the round. */
export const PUSH_STATE_REMOTE_AHEAD = 'remote-ahead'
/** This round holds commits origin does not. The one state that owes a push. */
export const PUSH_STATE_PUSH_OWED = 'push-owed'
/** Neither side descends from the other: a concurrent writer rewrote the branch. */
export const PUSH_STATE_DIVERGED = 'diverged'
/** Ahead of origin on purpose — the stale-SHA cancellation built a commit and did not send it. */
export const PUSH_STATE_WITHHELD = 'withheld'

/** Every push state this module can return. Frozen: the set is closed, not a starting point. */
export const PUSH_STATES = Object.freeze([
  PUSH_STATE_PUBLISHED,
  PUSH_STATE_REMOTE_AHEAD,
  PUSH_STATE_PUSH_OWED,
  PUSH_STATE_DIVERGED,
  PUSH_STATE_WITHHELD,
])

/**
 * The rendered Repair Summary value for each state. This is the field a round pastes verbatim, and
 * it exists so a round that landed no commits emits the SAME field shape as one that pushed —
 * which is the entire point: the two outcomes have to be distinguishable in the report.
 */
const PUSH_STATE_LINES = Object.freeze({
  [PUSH_STATE_PUBLISHED]: 'published — local and origin agree; no push owed',
  [PUSH_STATE_REMOTE_AHEAD]:
    'remote-ahead — origin is ahead of this worktree; do not push, re-derive the round from the new head',
  [PUSH_STATE_PUSH_OWED]: 'push-owed — this round holds commits origin does not; push them',
  [PUSH_STATE_DIVERGED]:
    'diverged — neither side descends from the other; do not push and do not force-push, report a residual and re-derive the round from the new head',
  [PUSH_STATE_WITHHELD]:
    'withheld — a commit was built and deliberately not pushed; report it, do not push it',
})

/** Why the state was reached. Distinct from the state: two inputs reach `diverged` differently. */
export const PUSH_REASON_SHAS_EQUAL = 'shas-equal: local and origin resolve to the same commit'
export const PUSH_REASON_LOCAL_BEHIND = 'local-behind: local is an ancestor of origin'
export const PUSH_REASON_LOCAL_AHEAD = 'local-ahead: origin is an ancestor of local'
export const PUSH_REASON_NO_COMMON_DESCENDANT =
  'no-common-descendant: neither side is an ancestor of the other'
export const PUSH_REASON_WITHHELD_DECLARED =
  'withheld-declared: the round declared this commit withheld by the stale-SHA cancellation'
// The fail-closed arm. `diverged` is the state because its ACTION — do not push, do not force-push,
// report a residual, re-derive — is exactly the correct action for a state nobody could read. The
// reason is separate from the state precisely so the report does not claim a concurrent writer that
// nobody observed.
export const PUSH_REASON_UNREADABLE =
  'unreadable-input: a SHA or ancestry answer did not resolve, so this round cannot claim it published anything'

/** Every reason this module can return. */
export const PUSH_REASONS = Object.freeze([
  PUSH_REASON_SHAS_EQUAL,
  PUSH_REASON_LOCAL_BEHIND,
  PUSH_REASON_LOCAL_AHEAD,
  PUSH_REASON_NO_COMMON_DESCENDANT,
  PUSH_REASON_WITHHELD_DECLARED,
  PUSH_REASON_UNREADABLE,
])

/** A SHA is readable when it is a non-empty string after trimming. Nothing else is accepted. */
function readableSha(value) {
  return typeof value === 'string' && value.trim() !== ''
}

/**
 * Classify what this round owes origin, and render the Repair Summary field for it.
 *
 * `localSha` and `remoteSha` are the two `git rev-parse` outputs, compared as STRINGS.
 * `remoteIsAncestorOfLocal` and `localIsAncestorOfRemote` are the two `git merge-base --is-ancestor`
 * answers as booleans — only `true` asserts the ancestry, because an unset shell variable arrives as
 * the empty string and coercing that to `false` would be indistinguishable from a real `false`.
 * `withheld` is the round's own declaration that it built a commit and deliberately did not send it.
 *
 * Fails closed: an absent, empty or non-string SHA, or a pair that asserts both ancestries at once,
 * returns `diverged` with the `unreadable-input` reason. It never returns `published`, because
 * `published` is the arm that lets a round that forgot to push say nothing at all.
 *
 * @param {{localSha?: unknown, remoteSha?: unknown, remoteIsAncestorOfLocal?: unknown,
 *   localIsAncestorOfRemote?: unknown, withheld?: unknown}} [input]
 * @returns {{state: string, owed: boolean, line: string, reason: string}}
 */
export function classifyPushState({
  localSha,
  remoteSha,
  remoteIsAncestorOfLocal,
  localIsAncestorOfRemote,
  withheld,
} = {}) {
  const verdict = (state, reason) => ({
    state,
    owed: state === PUSH_STATE_PUSH_OWED,
    line: PUSH_STATE_LINES[state],
    reason,
  })

  // Checked first, and without consulting the SHAs: `withheld` is the round declaring a deliberate,
  // reported outcome. It is loud by construction — the opposite of the silence this module exists to
  // end — so it is never overridden by an unreadable ancestry answer.
  if (withheld === true) return verdict(PUSH_STATE_WITHHELD, PUSH_REASON_WITHHELD_DECLARED)

  if (!readableSha(localSha) || !readableSha(remoteSha)) {
    return verdict(PUSH_STATE_DIVERGED, PUSH_REASON_UNREADABLE)
  }
  if (localSha.trim() === remoteSha.trim()) {
    return verdict(PUSH_STATE_PUBLISHED, PUSH_REASON_SHAS_EQUAL)
  }

  const behind = localIsAncestorOfRemote === true
  const ahead = remoteIsAncestorOfLocal === true
  // Both ancestries with unequal SHAs is arithmetically impossible in git, so a caller asserting it
  // has passed something it did not read. Refusing it here keeps a garbled pair out of `push-owed`,
  // where it would send a push nobody derived.
  if (behind && ahead) return verdict(PUSH_STATE_DIVERGED, PUSH_REASON_UNREADABLE)
  if (behind) return verdict(PUSH_STATE_REMOTE_AHEAD, PUSH_REASON_LOCAL_BEHIND)
  if (ahead) return verdict(PUSH_STATE_PUSH_OWED, PUSH_REASON_LOCAL_AHEAD)
  return verdict(PUSH_STATE_DIVERGED, PUSH_REASON_NO_COMMON_DESCENDANT)
}

// ---------------------------------------------------------------------------
// Decline reply — the ordering gate on a category (c) reply that cites a record.

/** The reply cites a record that exists. Admissible. */
export const DECLINE_REPLY_RECORD_CITED = 'record-cited'
/** The reply asserts a record nobody can point at. Inadmissible. */
export const DECLINE_REPLY_UNBACKED_CLAIM = 'unbacked-claim'

/** Every decline-reply verdict this module can return. */
export const DECLINE_REPLY_REASONS = Object.freeze([
  DECLINE_REPLY_RECORD_CITED,
  DECLINE_REPLY_UNBACKED_CLAIM,
])

/**
 * Decide whether a category (c) decline reply may be posted.
 *
 * The failure this closes is an ordering one, not a content one: the reply is drafted before the
 * residual is written, so it can promise a follow-up that never gets recorded — an unbacked claim on
 * a public PR that nothing downstream re-checks. The gate is therefore the record's IDENTIFIER, not
 * the round's intention to create one: an id exists only after the write succeeded.
 *
 * Fails closed on absent, empty and whitespace-only ids alike. Whitespace matters because the id
 * arrives from a command substitution, and a failed `boss notes add` or `gh pr comment` most easily
 * yields the empty string or a bare newline — exactly the values a truthiness check admits or a
 * trim-less check treats as present.
 *
 * @param {{residualRecordId?: unknown}} [input]
 * @returns {{admit: boolean, reason: string, recordId: string|null, detail: string}}
 */
export function classifyDeclineReply({ residualRecordId } = {}) {
  if (typeof residualRecordId !== 'string' || residualRecordId.trim() === '') {
    return {
      admit: false,
      reason: DECLINE_REPLY_UNBACKED_CLAIM,
      recordId: null,
      detail:
        'no residual record id: write the residual first, then cite the id the write returned. A reply that promises a record nobody can point at is an unbacked claim on a public PR.',
    }
  }
  return {
    admit: true,
    reason: DECLINE_REPLY_RECORD_CITED,
    recordId: residualRecordId.trim(),
    detail: 'the residual record exists and its id is available to cite in the reply',
  }
}

// ---------------------------------------------------------------------------
// Residual sink — the destination category (c) requires and never names.

/** The preferred sink: a durable, repo-scoped note the `boss` CLI writes. */
export const RESIDUAL_SINK_BOSS_NOTES = 'boss-notes'
/** The fallback: a PR comment. Weaker, but available in every repo this core installs into. */
export const RESIDUAL_SINK_PR_COMMENT = 'pr-comment'

/** Every sink this module can return. */
export const RESIDUAL_SINKS = Object.freeze([RESIDUAL_SINK_BOSS_NOTES, RESIDUAL_SINK_PR_COMMENT])

/** The tag the residual note carries, so a later sweep can find every residual this core recorded. */
export const RESIDUAL_NOTE_TAG = 'repair-residual'

/**
 * The placeholder the caller replaces with a path to the residual body it wrote.
 *
 * Both commands take the body from a FILE, never from a spliced literal. A residual body is
 * arbitrary English prose: an apostrophe in it ends a single-quoted shell argument early, and the
 * only repair that looks available is rewording the residual — which changes the thing being
 * recorded in order to make the command that records it parse.
 */
export const RESIDUAL_BODY_PLACEHOLDER = '<residual-body-path>'

/**
 * Resolve where a declined-remedy residual is written, and the exact command that writes it.
 *
 * `bossBinary` accepts what `resolveBossBinary` returns (`{path, ok}`), a bare path string, or
 * `null` for "none resolved". When the key is absent entirely the vendored resolver is consulted, so
 * the sink is detected rather than assumed. Anything that does not resolve to a usable path degrades.
 *
 * Fails closed toward the FALLBACK: this core installs into unrelated repositories, so assuming the
 * binary is present would fail silently in exactly the repos it ships to. A degraded sink is a
 * weaker record, but `degraded: true` makes it a reported fact rather than a silent substitution.
 *
 * The `boss notes add` shape puts `--` before the body deliberately: the body is positional, so a
 * residual whose text begins with `--` is otherwise eaten as a flag and the note is never written.
 *
 * @param {{bossBinary?: unknown}} [input]
 * @param {{env?: Record<string,string|undefined>, fs?: object, cwd?: string}} [deps]
 * @returns {{sink: string, command: string, degraded: boolean, binaryPath: string|null, reason: string}}
 */
export function resolveResidualSink({ bossBinary } = {}, deps = {}) {
  let resolved = bossBinary
  if (resolved === undefined) {
    resolved = resolveBossBinary(deps.env ?? process.env, deps)
  }

  let binaryPath = null
  if (typeof resolved === 'string') {
    if (resolved.trim() !== '') binaryPath = resolved.trim()
  } else if (resolved !== null && typeof resolved === 'object') {
    if (resolved.ok === true && typeof resolved.path === 'string' && resolved.path.trim() !== '') {
      binaryPath = resolved.path.trim()
    }
  }

  if (binaryPath === null) {
    return {
      sink: RESIDUAL_SINK_PR_COMMENT,
      command: `gh pr comment <pr-number> --body-file "${RESIDUAL_BODY_PLACEHOLDER}"`,
      degraded: true,
      binaryPath: null,
      reason:
        'no-boss-binary: no usable boss CLI resolved, so the residual is recorded as a PR comment. That is a weaker, less searchable record — report the degradation rather than presenting it as the normal sink.',
    }
  }

  return {
    sink: RESIDUAL_SINK_BOSS_NOTES,
    command: `"${binaryPath}" notes add --tag ${RESIDUAL_NOTE_TAG} --json -- "$(cat "${RESIDUAL_BODY_PLACEHOLDER}")"`,
    degraded: false,
    binaryPath,
    reason: 'boss-binary-resolved: the residual is recorded as a durable repo-scoped note',
  }
}

// ---------------------------------------------------------------------------
// Problem sources — what opens a repair cycle, widened so a finding is admitted
// on whether it is real rather than on where it happened to be written.

export const PROBLEM_SOURCE_MERGE_CONFLICT = 'merge-conflict'
export const PROBLEM_SOURCE_FAILING_CHECKS = 'failing-checks'
export const PROBLEM_SOURCE_REVIEW_FEEDBACK = 'review-feedback'
/** The source the closed three-value list could not express: a finding written only in a report. */
export const PROBLEM_SOURCE_REPORT_ONLY = 'report-only-finding'
/** The genuinely-nothing answer. Reachable only when every source above is absent. */
export const PROBLEM_SOURCE_NONE = 'none'

/** Every problem source this module can return, in report order. */
export const PROBLEM_SOURCES = Object.freeze([
  PROBLEM_SOURCE_MERGE_CONFLICT,
  PROBLEM_SOURCE_FAILING_CHECKS,
  PROBLEM_SOURCE_REVIEW_FEEDBACK,
  PROBLEM_SOURCE_REPORT_ONLY,
  PROBLEM_SOURCE_NONE,
])

/** The rendered `**Problem Identified**` label for each source. */
const PROBLEM_SOURCE_LABELS = Object.freeze({
  [PROBLEM_SOURCE_MERGE_CONFLICT]: 'Merge conflict',
  [PROBLEM_SOURCE_FAILING_CHECKS]: 'Failing tests',
  [PROBLEM_SOURCE_REVIEW_FEEDBACK]: 'Review feedback',
  [PROBLEM_SOURCE_REPORT_ONLY]: 'Report-only finding',
  [PROBLEM_SOURCE_NONE]: 'None',
})

/**
 * Is one probe's answer present?
 *
 * Absent is a closed list — `undefined`, `null`, `false`, `0`, `''`, whitespace and `[]` — and
 * EVERYTHING else is present, including a shape this function does not recognise. That asymmetry is
 * the fail-closed arm: `None` is the report's silence, so an unreadable probe must not reach it.
 */
function sourcePresent(value) {
  if (value === undefined || value === null || value === false) return false
  if (typeof value === 'number') return value !== 0
  if (typeof value === 'string') return value.trim() !== ''
  if (Array.isArray(value)) return value.length > 0
  return true
}

/**
 * Render the ordered `**Problem Identified**` value for this round.
 *
 * `reportOnlyFindings` is the fourth source, and the reason this function exists: a must-fix finding
 * recorded only in a build-review report or the PR body matches none of the other three, so a round
 * that has one reports `None`, commits nothing, and burns a full round. Whether a real finding opens
 * a repair cycle must not depend on where it was written.
 *
 * `None` is returned only when all four are absent — it is never one entry among several.
 *
 * @param {{conflict?: unknown, failingChecks?: unknown, reviewThreads?: unknown,
 *   reportOnlyFindings?: unknown}} [input]
 * @returns {{sources: string[], line: string, none: boolean}}
 */
export function classifyProblemSources({
  conflict,
  failingChecks,
  reviewThreads,
  reportOnlyFindings,
} = {}) {
  const sources = []
  if (sourcePresent(conflict)) sources.push(PROBLEM_SOURCE_MERGE_CONFLICT)
  if (sourcePresent(failingChecks)) sources.push(PROBLEM_SOURCE_FAILING_CHECKS)
  if (sourcePresent(reviewThreads)) sources.push(PROBLEM_SOURCE_REVIEW_FEEDBACK)
  if (sourcePresent(reportOnlyFindings)) sources.push(PROBLEM_SOURCE_REPORT_ONLY)

  if (sources.length === 0) {
    return {
      sources: [PROBLEM_SOURCE_NONE],
      line: PROBLEM_SOURCE_LABELS[PROBLEM_SOURCE_NONE],
      none: true,
    }
  }
  return {
    sources,
    line: sources.map((source) => PROBLEM_SOURCE_LABELS[source]).join(', '),
    none: false,
  }
}

// ---------------------------------------------------------------------------
// CLI

const USAGE = `usage: bs-repair-derivations.mjs <command> [--in <path>]
  push-state       --in <path>  {"localSha","remoteSha","remoteIsAncestorOfLocal",
                                 "localIsAncestorOfRemote","withheld"}
                   prints {"state","owed","line","reason"}; "line" is the Repair Summary
                   **Push state** value, emitted on every round including one that landed no commits
  decline-reply    --in <path>  {"residualRecordId"}
                   prints {"admit","reason","recordId","detail"}; admit is false until the
                   residual record exists and its id can be cited
  residual-sink    [--in <path>]  {"bossBinary"} — omit --in to detect the boss CLI
                   prints {"sink","command","degraded","binaryPath","reason"}
  problem-sources  --in <path>  {"conflict","failingChecks","reviewThreads","reportOnlyFindings"}
                   prints {"sources","line","none"}; "None" only when all four are absent

  --in <path>  a file holding the command's input as JSON. Input is taken by PATH, never spliced
               into a shell literal: these payloads carry SHAs, ids and finding text, and an
               apostrophe in arbitrary prose ends a single-quoted argument early — which produces a
               broken command whose only apparent repair is editing the very value being classified.`

const COMMANDS = Object.freeze(['push-state', 'decline-reply', 'residual-sink', 'problem-sources'])

/**
 * CLI entry point. RETURNS an exit code and never calls process.exit, so a caller can test it
 * in-process: 0 on success, 2 on a usage or input error. Nothing is written to stdout on the error
 * path — a caller reading a verdict off stdout must not receive a usage message instead.
 */
export function runCli(argv = [], io = {}) {
  const stdout = io.stdout ?? ((text) => process.stdout.write(text))
  const stderr = io.stderr ?? ((text) => process.stderr.write(text))
  const readFile = io.readFile ?? ((path) => readFileSync(path, 'utf8'))

  const [command, flag, inputPath] = argv

  if (!COMMANDS.includes(command)) {
    stderr(
      `${command === undefined ? 'missing command' : `unknown command: ${command}`}\n${USAGE}\n`,
    )
    return 2
  }
  if (flag !== undefined && flag !== '--in') {
    stderr(`unknown option: ${flag}\n${USAGE}\n`)
    return 2
  }
  if (flag === '--in' && typeof inputPath !== 'string') {
    stderr(`--in requires a path\n${USAGE}\n`)
    return 2
  }
  if (argv.length > (flag === '--in' ? 3 : 1)) {
    stderr(`${command} takes at most --in <path>\n${USAGE}\n`)
    return 2
  }
  // Only residual-sink has a meaningful no-input form: it detects the binary itself. The other
  // three classify an input, and running them with none would classify the empty object — which
  // is a real verdict for each, so it would SUCCEED and print a plausible answer nobody derived.
  if (flag === undefined && command !== 'residual-sink') {
    stderr(`${command} requires --in <path>\n${USAGE}\n`)
    return 2
  }

  let input = {}
  if (flag === '--in') {
    let text
    try {
      text = readFile(inputPath)
    } catch (err) {
      stderr(`cannot read input file ${inputPath}: ${err.message}\n`)
      return 2
    }
    try {
      input = JSON.parse(text)
    } catch (err) {
      stderr(`input is not valid JSON: ${err.message}\n`)
      return 2
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      stderr(
        `input must be a JSON object, got ${Array.isArray(input) ? 'an array' : typeof input}\n`,
      )
      return 2
    }
  }

  // Dispatched with explicit `command === '<verb>'` comparisons rather than a switch: the repo's
  // skill-symbol gate reads a helper's own dispatch to decide which verbs a skill body may cite, and
  // it recognises this form. A switch leaves the verbs invisible to it, so a body could cite one that
  // does not exist and no gate would say so.
  let verdict
  try {
    if (command === 'push-state') verdict = classifyPushState(input)
    else if (command === 'decline-reply') verdict = classifyDeclineReply(input)
    else if (command === 'residual-sink') verdict = resolveResidualSink(input)
    else if (command === 'problem-sources') verdict = classifyProblemSources(input)
  } catch (err) {
    stderr(`${err.message}\n`)
    return 2
  }
  // Unreachable while COMMANDS and the arms above agree; a refusal rather than a throw if they drift.
  if (verdict === undefined) {
    stderr(`${command} is listed as a command but has no handler\n${USAGE}\n`)
    return 2
  }
  stdout(`${JSON.stringify(verdict)}\n`)
  return 0
}

if (isMainModule(import.meta.url)) {
  process.exitCode = runCli(process.argv.slice(2))
}
