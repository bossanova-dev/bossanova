// bs-dispatch-claims.mjs — adjudicate the checkable claims in a dispatch report.
//
// A dispatched subagent's report is validated for SHAPE and never for CONTENT:
// `skill-extensions.mjs` `validateResult` enforces that a role's envelope carries
// the declared keys, and nothing then asks whether the values in those keys name
// anything real. A confident falsehood therefore survives the run and reaches a
// public artefact — a PR reply, a review report, a plan body, a downstream brief.
//
// A claim is in scope here IFF one filesystem or git read settles it with no
// judgement. Three kinds qualify:
//
//   path        a repo-relative path, optionally `:<line>` — resolvable, readable,
//               long enough
//   git-object  a short or full object name — `git rev-parse` either knows it or
//               does not
//   tree        a tree hash offered as "the tree the gates ran on" — equal to
//               `HEAD^{tree}` or stale
//
// Everything else the incident record describes — whether a mutant really went
// red, whether a residual risk is noise, whether two dispatches contradict each
// other, whether a count is right — needs judgement or a second run and is
// deliberately out of scope. This module is the mechanical floor, not a referee.
//
// EXTRACTION STAYS WITH THE CALLER. The caller knows which field of a report
// holds a citation; this module adjudicates the list it is handed. A caller that
// extracts nothing gets an empty result, which is a caller bug this module cannot
// see.
//
// Node built-ins only — cron worktrees are dependency-free.

import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { isMainModule } from './main-module.mjs'
import { repoRootIsReadable, resolveCitationCoordinate } from './citation-coordinate.mjs'

/** The closed set of claim kinds this module adjudicates. */
export const CLAIM_KINDS = Object.freeze(['path', 'git-object', 'tree'])

/**
 * The closed verdict vocabulary, and the action each one obliges at every call site:
 *
 *   verified      the path/object/tree resolved — the claim may be cited and published
 *   refuted       it resolved to nothing, or the tree is not the tip — strike the claim,
 *                 never publish or forward it, and treat the conclusion resting on it as
 *                 unproven
 *   unverifiable  no repo root, no git, or the path escapes the root — record it, do not
 *                 cite it; an unverifiable claim is never promoted to verified
 */
export const CLAIM_VERDICTS = Object.freeze(['verified', 'refuted', 'unverifiable'])

// An object name is hex and between an abbreviation and a full SHA-1. Shape is
// checked BEFORE git is consulted so an arbitrary claim string is never handed to
// the process runner as a revision argument.
const OBJECT_NAME = /^[0-9a-f]{4,40}$/i

// A FULL object name. A tree claim is an identity statement, so only this shape can
// carry it (see `verifyTree`).
const FULL_OBJECT_NAME = /^[0-9a-f]{40}$/i

// `<path>:<line>` — the trailing group is the line, and only a trailing group is.
const PATH_WITH_LINE = /^(.*?):(\d+)$/

/** The default git runner: a real `git` invocation in `repoRoot`. */
export function spawnGit(repoRoot) {
  return (args) => {
    const res = spawnSync('git', args, { cwd: repoRoot ?? undefined, encoding: 'utf8' })
    if (res.error) throw res.error
    return { ok: res.status === 0, stdout: res.stdout ?? '', stderr: res.stderr ?? '' }
  }
}

function record(kind, claim, verdict, reason) {
  return { kind, claim, verdict, reason }
}

/**
 * Run one git query. A runner that is absent, throws (no binary, no repository),
 * or answers non-zero degrades to `null` — the caller then reports
 * `unverifiable`. It must never degrade to a verdict.
 */
function ask(git, args) {
  if (typeof git !== 'function') return null
  let res
  try {
    res = git(args)
  } catch {
    return null
  }
  if (!res || res.ok !== true) {
    return { ok: false, stderr: String(res?.stderr ?? '').trim() }
  }
  return { ok: true, value: String(res.stdout ?? '').trim(), stderr: '' }
}

/**
 * The coordinate a path claim names. STRUCTURED input wins: a caller that already holds
 * a file and a line must not format them into a string this module then re-splits. The
 * round trip is lossy for a file whose own name ends in `:<digits>`, and re-deriving
 * what the caller already knew is, in miniature, the second resolver R1 forbids.
 */
function pathTarget(entry, claim) {
  if (typeof entry?.file === 'string' && entry.file.trim() !== '') {
    return { file: entry.file, line: entry.line ?? null }
  }
  const located = PATH_WITH_LINE.exec(claim)
  return {
    file: located ? located[1] : claim,
    line: located ? Number(located[2]) : null,
  }
}

/**
 * Not every failed read is a false claim.
 *
 *   ENOENT/ENOTDIR  nothing is there — the fabricated-path shape this module exists to
 *                   catch, and the one unreadable case that refutes
 *   EISDIR          the path IS there. A bare claim naming a directory inside the root
 *                   is TRUE; branding it refuted is the mirror image of verifying line 0.
 *                   A directory has no lines, so a LINE on one is still false.
 *   anything else   a permission wall, a symlink loop, an I/O fault: a failure of this
 *                   CHECK, not of the claim. R2 makes that `unverifiable`.
 */
function unreadablePath(claim, file, line, error) {
  if (error?.code === 'EISDIR') {
    return line === null
      ? record(
          'path',
          claim,
          'verified',
          `${file} resolves inside the repository root as a directory`,
        )
      : record('path', claim, 'refuted', `${file} is a directory, so line ${line} does not exist`)
  }
  if (error?.code === 'ENOENT' || error?.code === 'ENOTDIR') {
    return record('path', claim, 'refuted', `${file} does not exist in the repository`)
  }
  // `error.message` is not guaranteed: an unexpected resolver result, or a thrown
  // non-Error, reaches here carrying no message — and a TypeError raised while EXPLAINING
  // a failed check is the loudest possible way to not fail closed.
  const detail = error?.message ?? `no diagnostic was reported (${String(error)})`
  return record('path', claim, 'unverifiable', `${file} could not be read: ${detail}`)
}

/**
 * The canonical rendering of a structured coordinate: the bare `file` when no line is
 * carried, `file:line` otherwise. It is the ONE rendering in this module — `claimText`
 * composes a record's claim string with it and `structuredDisagreement` tests a
 * caller-supplied claim string against it — so what a record PUBLISHES and what must agree
 * with the coordinate actually ADJUDICATED cannot drift apart.
 */
function renderCoordinate(file, line) {
  return line === null || line === undefined ? file : `${file}:${line}`
}

/**
 * Two contradictory readings of one claim.
 *
 * `pathTarget` prefers an entry's STRUCTURED `file`/`line`, while `claimText` publishes the
 * caller's `claim` string verbatim. An entry supplying both can therefore adjudicate one
 * coordinate and publish another: `{claim: 'src/missing.js:1', file: 'src/real.js', line: 1}`
 * reads `src/real.js` and stamps `verified` on a record whose claim names `src/missing.js:1`.
 * That is the fabricated coordinate this module exists to catch, emitted by this module, and
 * R2 makes it fail CLOSED.
 *
 * The test is exact equality against the canonical rendering and nothing looser — no path
 * normalisation, no case folding, no trimming. Anything cleverer would be a second
 * path-resolution rule, which R1 forbids, and two readings that differ in text do differ.
 *
 * Neither `verified` nor `refuted` is available here: the claim may well be true. A module
 * holding two contradictory readings of WHAT WAS CLAIMED must decline to decide rather than
 * pick one of them and publish the other.
 *
 * @returns {string|null} the reason to decline, or null when the two readings agree (or when
 *   the entry carries only one of them, which is the ordinary shape).
 */
function structuredDisagreement(entry, claim) {
  if (typeof entry?.claim !== 'string') return null
  if (typeof entry?.file !== 'string' || entry.file.trim() === '') return null
  const canonical = renderCoordinate(entry.file, entry.line ?? null)
  if (claim === canonical) return null
  return (
    `claim ${claim} disagrees with the structured coordinate ${canonical}, ` +
    'so which coordinate was claimed cannot be settled'
  )
}

function verifyPath(claim, repoRoot, entry) {
  // Decided BEFORE the root and before any read: a claim this module cannot even READ
  // unambiguously is not made adjudicable by the environment being healthy.
  const disagreement = structuredDisagreement(entry, claim)
  if (disagreement !== null) {
    return record('path', claim, 'unverifiable', disagreement)
  }
  if (typeof repoRoot !== 'string' || repoRoot.trim() === '') {
    return record('path', claim, 'unverifiable', 'no readable repository root to resolve against')
  }
  // A root that is PRESENT but names nothing readable is that same failure wearing a
  // string. Every path beneath it reads ENOENT, so without this the module strikes EVERY
  // claim in the list `refuted` — the fail-FALSE direction of the rule it exists to
  // enforce. Decided BEFORE any per-file resolution, so no coordinate is ever adjudicated
  // against a root that was never there.
  if (!repoRootIsReadable(repoRoot)) {
    return record(
      'path',
      claim,
      'unverifiable',
      `repository root ${repoRoot} could not be read, so no path resolves against it`,
    )
  }
  const { file, line } = pathTarget(entry, claim)
  if (typeof file !== 'string' || file.trim() === '') {
    return record('path', claim, 'unverifiable', 'claim carries no path to resolve')
  }
  // A `line` that is not a NUMBER is a shape this module cannot read, not a false
  // coordinate. The CLI hands `verifyDispatchClaims` whatever a claims file holds, so
  // `{file: 'src/real.js', line: '3'}` — a line that file genuinely has, spelled as a
  // string — reached the resolver, failed `Number.isInteger`, and was published as
  // `refuted` with "line 3 is not a line any file has". That is a false statement about a
  // true claim, and it is terminal under R3.
  //
  // An impossible NUMBER still refutes below: 0, a negative and a fraction are coordinates
  // no file can have, which is a fact about the claim rather than about its spelling.
  if (line !== null && typeof line !== 'number') {
    return record(
      'path',
      claim,
      'unverifiable',
      `line is given as a ${typeof line}, not a number, so the coordinate cannot be resolved`,
    )
  }

  let resolved
  try {
    resolved = resolveCitationCoordinate(repoRoot, file, line)
  } catch (error) {
    return record('path', claim, 'unverifiable', `path could not be resolved: ${error.message}`)
  }
  if (resolved.ok) {
    return record(
      'path',
      claim,
      'verified',
      line === null
        ? `${file} resolves inside the repository root`
        : `${file} resolves and has at least ${line} line(s)`,
    )
  }
  // An escape from the root is NOT a refutation: the claim may name a real file
  // this check simply has no authority over. Fail closed instead of guessing.
  if (resolved.code === 'escapes-root') {
    return record('path', claim, 'unverifiable', `${file} escapes the repository root`)
  }
  if (resolved.code === 'bad-line') {
    return record(
      'path',
      claim,
      'refuted',
      `line ${String(resolved.line)} is not a line any file has`,
    )
  }
  if (resolved.code === 'short-file') {
    return record(
      'path',
      claim,
      'refuted',
      `${file} has only ${resolved.lineCount} line(s), so line ${line} does not exist`,
    )
  }
  if (resolved.code === 'unreadable') {
    return unreadablePath(claim, file, line, resolved.error)
  }
  // A result code this mapping does not know. Today the resolver returns exactly the four
  // failure codes handled above, so nothing reaches here — but this was an UNCONDITIONAL
  // fall-through into `unreadablePath`, which dereferences an `error` a new code need not
  // carry. That throws a TypeError straight out of `verifyDispatchClaims`: the try/catch
  // above wraps only the resolver CALL, and `bs-review-triage.mjs` has no catch of its own,
  // so one new resolver code would crash a whole triage run. An unhandled code must degrade
  // to a verdict — and under R2 the only verdict available for a check that did not run is
  // `unverifiable`.
  return record(
    'path',
    claim,
    'unverifiable',
    `path resolution returned an unexpected result code: ${String(resolved.code)}`,
  )
}

// Git's own word for the condition, taken from the diagnostic it writes rather than from a
// paraphrase: `error: short object ID <prefix> is ambiguous`. Matched on that one word and
// not on the surrounding prose, so a reworded hint or a translated `fatal:` tail does not
// silently turn an ambiguity back into a refutation.
const AMBIGUOUS_OBJECT = /\bambiguous\b/i

/**
 * Did the failed `--quiet` ask above fail because the abbreviation is AMBIGUOUS?
 *
 * Re-asks without `--quiet`, which is the only way to see the distinction: `--quiet`
 * suppresses the ambiguity diagnostic exactly as it suppresses the not-found one. Costs one
 * extra git call on the failure path only, and never on the path that answered.
 *
 * A runner that cannot answer the re-ask returns false, which leaves the caller's existing
 * `refuted` — the pre-existing behaviour for a claim git says it does not have.
 */
function isAmbiguousObject(git, claim) {
  const retry = ask(git, ['rev-parse', '--verify', `${claim}^{object}`])
  return retry !== null && retry.ok !== true && AMBIGUOUS_OBJECT.test(retry.stderr)
}

function verifyGitObject(claim, git) {
  if (!OBJECT_NAME.test(claim)) {
    return record('git-object', claim, 'refuted', `${claim} is not an object name`)
  }
  const answer = ask(git, ['rev-parse', '--verify', '--quiet', `${claim}^{object}`])
  if (answer === null) {
    return record('git-object', claim, 'unverifiable', 'no git runner available to resolve objects')
  }
  // MEASURED against the real binary: with `--verify --quiet`, git suppresses its own
  // diagnostic for a revision it does not have — exit 1, EMPTY stderr. Any stderr here
  // therefore means git could not ANSWER (no repository, a broken object store, refused
  // ownership), and R2 makes that `unverifiable`: absent git never decides a claim false.
  if (!answer.ok && answer.stderr !== '') {
    return record(
      'git-object',
      claim,
      'unverifiable',
      `git could not resolve objects here: ${answer.stderr.split('\n')[0]}`,
    )
  }
  if (!answer.ok || answer.value === '') {
    // `--quiet` suppresses the AMBIGUITY diagnostic too, so the answer above cannot tell
    // "no such object" from "several objects share this prefix". MEASURED against the real
    // binary in this repository, the two are byte-identical there: `0000^{object}` with
    // three candidates and a nonexistent full SHA BOTH give exit 1, empty stdout, empty
    // stderr. Reading that as `refuted` publishes "names no object in this repository"
    // about a prefix that names three — a false statement, terminal under R3, in the module
    // whose purpose is to stop false claims.
    //
    // So re-ask WITHOUT `--quiet`, on the failure path only, and let git's own word decide.
    // Same measurement: the ambiguous form adds `error: short object ID 0000 is ambiguous`,
    // the nonexistent one says only `fatal: Needed a single revision`.
    if (isAmbiguousObject(git, claim)) {
      return record(
        'git-object',
        claim,
        'unverifiable',
        `${claim} is an ambiguous abbreviation: several objects share this prefix`,
      )
    }
    return record('git-object', claim, 'refuted', `${claim} names no object in this repository`)
  }
  return record('git-object', claim, 'verified', `${claim} resolves to ${answer.value}`)
}

function verifyTree(claim, git) {
  // Shape FIRST, matching the `git-object` sibling: an arbitrary claim string is never
  // handed to the process runner as a revision argument.
  if (!OBJECT_NAME.test(claim)) {
    return record('tree', claim, 'refuted', `${claim} is not an object name`)
  }
  // An ABBREVIATION cannot carry this claim. "the tree the gates ran on" is an identity
  // statement, and a prefix is satisfied by every object that shares it — prefix matching
  // verified a four-hex stub as the tip, which is exactly the evidence a short name must
  // not buy. Fail closed: the claim may be true, and this module declines to decide it
  // from a prefix rather than guessing.
  if (!FULL_OBJECT_NAME.test(claim)) {
    return record(
      'tree',
      claim,
      'unverifiable',
      `${claim} is abbreviated: a tree claim must give the full 40-character hash`,
    )
  }
  const answer = ask(git, ['rev-parse', 'HEAD^{tree}'])
  if (answer === null) {
    return record('tree', claim, 'unverifiable', 'no git runner available to read HEAD^{tree}')
  }
  if (!answer.ok || answer.value === '') {
    return record('tree', claim, 'unverifiable', 'HEAD^{tree} could not be read in this repository')
  }
  const tip = answer.value
  if (tip === claim.toLowerCase()) {
    return record('tree', claim, 'verified', `${claim} is HEAD^{tree} (${tip})`)
  }
  return record('tree', claim, 'refuted', `stale tree: HEAD^{tree} is ${tip}, not ${claim}`)
}

/**
 * Adjudicate a list of extracted dispatch claims.
 *
 * @param {{kind: string, claim: string}[]} claims
 * @param {{repoRoot?: string|null, git?: ((args: string[]) => {ok: boolean, stdout: string})|null}} [opts]
 *   `git` is injected so callers can supply a runner (and tests never shell out);
 *   pass `null` for a repository with no git, which degrades every git-backed
 *   claim to `unverifiable`. Omit it entirely for the real `git` binary.
 * @returns {{kind: string|null, claim: string|null, verdict: string, reason: string}[]}
 *   exactly one record per input claim, in input order.
 */
export function verifyDispatchClaims(claims, opts = {}) {
  if (!Array.isArray(claims)) return []
  const repoRoot = Object.hasOwn(opts, 'repoRoot') ? opts.repoRoot : safeCwd()
  const git = Object.hasOwn(opts, 'git') ? opts.git : spawnGit(repoRoot)

  return claims.map((entry) => {
    const kind = typeof entry?.kind === 'string' ? entry.kind : null
    const claim = claimText(entry)
    if (claim === null || claim.trim() === '') {
      return record(kind, claim, 'unverifiable', 'claim is missing or blank')
    }
    if (kind === 'path') return verifyPath(claim, repoRoot, entry)
    if (kind === 'git-object') return verifyGitObject(claim, git)
    if (kind === 'tree') return verifyTree(claim, git)
    return record(kind, claim, 'unverifiable', `unknown claim kind: ${String(kind)}`)
  })
}

/**
 * What the record CALLS this claim. A structured entry carries no string of its own, so
 * one is composed for the record; adjudication still reads the structured values, never
 * this text.
 */
function claimText(entry) {
  if (typeof entry?.claim === 'string') return entry.claim
  if (typeof entry?.file === 'string' && entry.file.trim() !== '') {
    return renderCoordinate(entry.file, entry.line ?? null)
  }
  return null
}

function safeCwd() {
  try {
    return process.cwd()
  } catch {
    return null
  }
}

// Thin CLI (the surface the skill prose invokes):
//   node bs-dispatch-claims.mjs verify --file <json> [--repo-root <dir>]
//
// Exit 0 when no claim is refuted, 1 when at least one is, 2 on an operator
// error (bad usage, unreadable input). An operator error is never a verdict.
if (isMainModule(import.meta.url)) {
  const [cmd, ...args] = process.argv.slice(2)
  const fail = (message) => {
    process.stderr.write(`bs-dispatch-claims.mjs: ${message}\n`)
    process.exit(2)
  }
  if (cmd !== 'verify') {
    fail('usage: bs-dispatch-claims.mjs verify --file <json> [--repo-root <dir>]')
  }
  let file = null
  let repoRoot = undefined
  while (args.length > 0) {
    const option = args.shift()
    const value = args.shift()
    if (typeof value !== 'string') fail(`missing value after ${option}`)
    if (option === '--file') file = value
    else if (option === '--repo-root') repoRoot = value
    else fail(`unknown option: ${option}`)
  }
  if (!file) fail('verify requires --file <json>')
  let claims
  try {
    claims = JSON.parse(readFileSync(file, 'utf8'))
  } catch (err) {
    fail(`failed to read ${file}: ${err.message}`)
  }
  if (!Array.isArray(claims)) fail(`${file} must hold a JSON array of {kind, claim} objects`)
  const records = verifyDispatchClaims(
    claims,
    repoRoot === undefined ? {} : { repoRoot, git: spawnGit(repoRoot) },
  )
  process.stdout.write(`${JSON.stringify(records)}\n`)
  process.exit(records.some((entry) => entry.verdict === 'refuted') ? 1 : 0)
}
