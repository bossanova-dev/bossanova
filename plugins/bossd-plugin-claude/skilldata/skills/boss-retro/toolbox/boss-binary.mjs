// skills-toolbox/boss-binary.mjs
// Single source of truth for "is the `boss` CLI actually reachable from here?".
// The skills resolve the binary by hand in three different places (an explicit
// $BOSS_BIN override, `boss` on PATH, the repo build at ./bin/boss); this module
// is that resolution, once, as a pure function over an injected fs so a caller —
// or a test — never has to touch the machine's real PATH.
//
// Executability is decided by a STAT, never by a spawn: a probe that ran
// `boss --version` would hang every caller behind a slow or wedged binary, and
// the gate this backs (callbacksAvailable) runs on every CI wait.
// node builtins only (the cron worktree is dependency-free).

import fs from 'node:fs'
import path from 'node:path'

// The command name looked up on PATH and under <repo-root>/bin.
export const BOSS_BINARY_NAME = 'boss'

// A cheap first pass: no execute bit at all for anyone means no candidate, whatever
// the caller's identity. It is deliberately NOT the whole test — an execute bit for a
// class this process is not in still means EACCES at run time — so `rejectionFor`
// follows it with a real X_OK access check wherever the fs impl can answer one.
const EXEC_BITS = 0o111

/**
 * Why this path cannot serve as the `boss` binary, or null when it can.
 * @param {string} candidate absolute-or-relative path to test
 * @param {{statSync: (p: string) => any}} fsImpl
 * @returns {string|null}
 */
function rejectionFor(candidate, fsImpl) {
  let stat
  try {
    stat = fsImpl.statSync(candidate)
  } catch {
    // ENOENT, ENOTDIR, EACCES on a parent — all mean "not a usable candidate
    // here", which is the next candidate's turn, never an error of its own.
    return 'does not exist'
  }
  if (!stat || typeof stat.isFile !== 'function' || !stat.isFile()) {
    // A directory is 0o755 too, so the mode check alone would wave one through.
    return 'is not a regular file'
  }
  if ((stat.mode & EXEC_BITS) === 0) return 'is not executable'
  // The mode bits answer "may SOMEONE run this", not "may I". A file whose only
  // execute bit belongs to a class this process is not in — say mode 0o010, or an
  // owner-exec binary owned by another user — passes the test above and still gives
  // EACCES when it is actually run, which would arm a callback that cannot fire. So
  // ask the kernel the question we actually mean. Guarded on the method existing
  // because `deps.fs` is documented as needing only `statSync`: a double that never
  // promised `accessSync` keeps the mode-bit verdict rather than being failed for it.
  if (typeof fsImpl.accessSync === 'function') {
    try {
      fsImpl.accessSync(candidate, fs.constants.X_OK)
    } catch {
      return 'is not executable by this process'
    }
  }
  return null
}

// One phrasing for "nothing resolved", so both exits below read identically in a log.
const unusableReason = (notes) => `no usable ${BOSS_BINARY_NAME} executable: ${notes.join('; ')}`

/**
 * Nearest ancestor of `startDir` carrying a `.git` entry, else `startDir` itself.
 * `.git` is a directory in a plain checkout and a FILE in a linked worktree, so
 * the probe is a bare stat rather than an isDirectory check.
 * @param {string} startDir
 * @param {{statSync: (p: string) => any}} fsImpl
 */
function repoRootFrom(startDir, fsImpl) {
  const start = path.resolve(startDir)
  let dir = start
  for (;;) {
    try {
      fsImpl.statSync(path.join(dir, '.git'))
      return dir
    } catch {
      // not this level — keep walking up
    }
    const parent = path.dirname(dir)
    // path.dirname('/') === '/' and dirname('C:\\') === 'C:\\': that fixed point
    // is the only loop terminator, so a cwd outside any repo falls back cleanly.
    if (parent === dir) return start
    dir = parent
  }
}

/**
 * Resolve the `boss` CLI without running it.
 *
 * Order — the first candidate that is an existing, executable, regular file wins:
 *   1. `$BOSS_BIN` (an explicit operator override; blank is treated as unset)
 *   2. `boss` on `$PATH`
 *   3. `<repo root of deps.cwd>/bin/boss` (the repo build)
 *
 * A candidate that fails is simply the next candidate's turn — never a throw. When
 * every candidate fails, `reason` names each one that was tried and why it lost, so
 * a caller can log "polling, because BOSS_BIN=… does not exist" instead of degrading
 * silently.
 *
 * `path` is not decoration: a caller that goes on to RUN the CLI must invoke that
 * exact path rather than a bare `boss`, because two of the three arms ($BOSS_BIN and
 * the repo build) resolve to something a bare `boss` in a shell would not find at all —
 * or, for $BOSS_BIN, would find as a DIFFERENT binary, since the override deliberately
 * outranks PATH. Using the verdict while discarding the path reintroduces the failure
 * from the other side: a true gate followed by `command not found`.
 *
 * @param {Record<string,string|undefined>} [env]
 * @param {{fs?: object, cwd?: string}} [deps] injection seam; `deps.fs` needs only
 *   `statSync`, and `deps.cwd` anchors the ./bin/boss fallback. No spawn dependency
 *   is accepted or used — see the module header.
 * @returns {{path: string|null, ok: boolean, reason: string}}
 */
export function resolveBossBinary(env = process.env, deps = {}) {
  const fsImpl = deps.fs || fs
  const notes = []
  const found = (resolved) => ({ path: resolved, ok: true, reason: '' })

  // 1. $BOSS_BIN — an explicit override, so its failure is the most useful thing
  //    to report: a stale worktree leaves BOSS_BIN naming a path that is gone.
  const configured = String(env.BOSS_BIN ?? '').trim()
  if (configured) {
    const rejection = rejectionFor(configured, fsImpl)
    if (!rejection) return found(configured)
    notes.push(`BOSS_BIN=${configured} ${rejection}`)
  } else {
    notes.push('BOSS_BIN is unset')
  }

  // 2. `boss` on PATH. The managed cron environment is exactly the case that fails
  //    here: it exports BOSS_SESSION_ID but never puts the CLI on the cron PATH.
  const pathEntries = String(env.PATH ?? '')
    .split(path.delimiter)
    .filter(Boolean)
  for (const dir of pathEntries) {
    const candidate = path.join(dir, BOSS_BINARY_NAME)
    if (!rejectionFor(candidate, fsImpl)) return found(candidate)
  }
  notes.push(
    pathEntries.length === 0
      ? 'PATH is empty'
      : `no executable ${BOSS_BINARY_NAME} in ${pathEntries.length} PATH ` +
          `${pathEntries.length === 1 ? 'entry' : 'entries'}`,
  )

  // 3. The repo build. Relative to the repo the caller is in, not to the process's
  //    incidental cwd inside it. The cwd is read HERE and not at the top: `process.cwd()`
  //    throws ENOENT when the process's directory has been removed (a deleted worktree —
  //    adjacent to the stale-worktree case this resolver exists for), and this gate runs
  //    on every CI wait, where a throw is strictly worse than a `false`. Reading it
  //    eagerly would have let that throw escape even when $BOSS_BIN already won.
  let repoBuild
  try {
    const startDir = deps.cwd || process.cwd()
    repoBuild = path.join(repoRootFrom(startDir, fsImpl), 'bin', BOSS_BINARY_NAME)
  } catch (error) {
    notes.push(`the repo build could not be located (${error?.code || 'unresolvable cwd'})`)
    return { path: null, ok: false, reason: unusableReason(notes) }
  }
  const rejection = rejectionFor(repoBuild, fsImpl)
  if (!rejection) return found(repoBuild)
  notes.push(`${repoBuild} ${rejection}`)

  return { path: null, ok: false, reason: unusableReason(notes) }
}
