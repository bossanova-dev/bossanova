// citation-coordinate.mjs — the single citation-resolution mechanic in this tree:
// does a repo-relative `file`, optionally carrying a `line`, name something readable
// inside the working-tree root?
//
// A second, subtly different resolver is the defect this module exists to prevent.
// `plan-contract-guard.mjs` gates a plan body's citations through it and
// `bs-dispatch-claims.mjs` adjudicates a dispatch report's cited coordinates through the
// same function, so a coordinate a plan citation would reject can never be accepted from
// a subagent's claim, or the reverse.
//
// It sits in its OWN module rather than inside the plan guard so a caller needing only
// this mechanic imports it alone, instead of dragging the guard's whole import closure
// (plan-deps-lib.mjs, gate-outcome.mjs, skill-config.mjs) into three published skill
// payloads. Reuse is the requirement; paying a hundred kilobytes per core for twenty
// lines is not the only way to meet it.
//
// Node built-ins only — cron worktrees are dependency-free.

import { accessSync, constants, readFileSync, realpathSync, statSync } from 'node:fs'
import { relative, resolve } from 'node:path'

function containedIn(root, path) {
  const rel = relative(root, path)
  return rel !== '' && !rel.startsWith('..') && !rel.split(/[\\/]/).includes('..')
}

/**
 * Is `root` a directory this process can actually resolve against?
 *
 * A root that is merely PRESENT is not a root: `resolve()` builds a path beneath a
 * directory that does not exist just as happily as beneath one that does, and every read
 * below it then fails ENOENT. A caller reading that as "the coordinate is a fabrication"
 * turns ONE failed read of the root into a refutation of every coordinate under it.
 *
 * It lives here, beside the confinement rule it precedes, so the tree keeps one module
 * owning what a repo root is. It is deliberately NOT a new `resolveCitationCoordinate`
 * result code: the two callers that map those codes already fail CLOSED on an unreadable
 * root — `plan-contract-guard.mjs` raises `unresolvable-citation`, `bs-review-triage.mjs`
 * rejects the finding — so threading a new code through their pinned contract strings
 * would change no outcome.
 *
 * Not a security boundary and not a TOCTOU guarantee: the root may vanish a microsecond
 * later, and the read below reports that as unreadable. This is only the cheap distinction
 * between "there is nothing to resolve against" and "this coordinate is false".
 *
 * @returns {boolean}
 */
export function repoRootIsReadable(root) {
  if (typeof root !== 'string' || root.trim() === '') return false
  const resolved = resolve(root)
  try {
    if (!statSync(resolved).isDirectory()) return false
    accessSync(resolved, constants.R_OK | constants.X_OK)
    return true
  } catch {
    return false
  }
}

/**
 * Resolve `file` against `root`, confining it to that root BOTH lexically and physically.
 *
 * The lexical pass alone is not confinement: `readFileSync` follows symlinks, so a
 * repo-local link pointing outside the tree resolves, reads, and is otherwise reported as
 * an in-repo coordinate. The physical pass re-checks the realpath of BOTH sides — both,
 * because a root reached through a symlinked prefix (macOS `/var` -> `/private/var`, and
 * every `mkdtemp` root beneath it) would otherwise make every citation look like an escape.
 *
 * A path that does not exist cannot be realpath'd. That is not an escape: the lexical
 * answer stands and the read below reports it as unreadable.
 *
 * @returns {string|null} the resolved absolute path, or null when it leaves the root.
 */
export function resolveCitationPath(root, file) {
  const resolvedRoot = resolve(root)
  const resolvedFile = resolve(resolvedRoot, file)
  if (!containedIn(resolvedRoot, resolvedFile)) return null
  let physicalRoot
  let physicalFile
  try {
    physicalRoot = realpathSync(resolvedRoot)
    physicalFile = realpathSync(resolvedFile)
  } catch {
    return resolvedFile
  }
  return containedIn(physicalRoot, physicalFile) ? resolvedFile : null
}

/**
 * `readBody` is injectable so a caller resolving many coordinates against the same files
 * (as `checkPlanCitations` does) reads each one once; the default reads from disk.
 *
 * An `ok` result carries the `body` it read. A caller that needs the bytes (to count
 * occurrences in them, say) must not reach them by letting an injected `readBody` assign an
 * outer variable: nothing in this contract promises `readBody` runs, runs once, or runs
 * before the result — the `bad-line` return fires before any read at all — so an
 * out-parameter is temporal coupling that happens to hold. This function has already read
 * the body and already derives `lineCount` from it, so it hands it back.
 *
 * @returns {{ok: true, path: string, lineCount: number, body: string}
 *          |{ok: false, code: 'escapes-root', path: null}
 *          |{ok: false, code: 'bad-line', path: string, line: unknown}
 *          |{ok: false, code: 'unreadable', path: string, error: Error}
 *          |{ok: false, code: 'short-file', path: string, lineCount: number}}
 */
export function resolveCitationCoordinate(root, file, line = null, { readBody = null } = {}) {
  const resolved = resolveCitationPath(root, file)
  if (!resolved) return { ok: false, code: 'escapes-root', path: null }
  // Line 0 — or a negative, or a fraction — is a coordinate no file can have. It is
  // decided BEFORE the read so that no file length can make it true: the only other test
  // here asks whether the line is PAST the end of the file, which every non-empty file
  // answers "no" for line 0.
  if (line !== null && (!Number.isInteger(line) || line < 1)) {
    return { ok: false, code: 'bad-line', path: resolved, line }
  }
  let read
  if (readBody) {
    read = readBody(resolved)
  } else {
    try {
      read = { body: readFileSync(resolved, 'utf8'), error: null }
    } catch (error) {
      read = { body: null, error }
    }
  }
  if (read.error) return { ok: false, code: 'unreadable', path: resolved, error: read.error }
  const lineCount = read.body.split('\n').length
  if (line !== null && line > lineCount) {
    return { ok: false, code: 'short-file', path: resolved, lineCount }
  }
  return { ok: true, path: resolved, lineCount, body: read.body }
}
