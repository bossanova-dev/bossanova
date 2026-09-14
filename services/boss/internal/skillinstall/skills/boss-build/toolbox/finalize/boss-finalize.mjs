// skills-toolbox/finalize/boss-finalize.mjs
// boss-finalize reference implementation of the finalize adapter (see adapter.mjs).
// Captures the tag-injection -> draft->ready -> repair finalize policy the
// boss-build skill owns today, with ZERO behaviour change: injectPrTag delegates
// to the existing add-pr-numbers.sh helper (the dependency-free finalize helper the
// cron siblings shell), and the operation map + policy constants are the exact values
// the SKILL body's Steps 8-10 use. node builtins only (the cron worktree is
// dependency-free — mirrors the tracker reference).
//
// A BossFinalizeAdapter has:
//   - injectPrTag(prNumber, {baseBranch}) — the one executable capability (delegates
//     to add-pr-numbers.sh, exactly as the current Step 8/9 shell block does); and
//   - operationMap, a declarative description of the agent-driven finalize
//     capabilities (ready the PR, run repair) plus the tag-injection command — the
//     single source of truth the spine names instead of hard-wiring the commands.

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

// The dependency-free finalize helper (NOT a boss CLI) is installed under BOTH skill
// trees: services/boss/internal/skillinstall ships it to ~/.claude and the Codex mirror
// ships it to ~/.codex. boss-build can run from either host, so resolve to whichever
// tree actually contains the helper instead of hard-wiring the Claude path — a
// Codex-only install has no .claude tree, so shelling the Claude path would ENOENT
// before any commit tags are injected. Reachable in a cron worktree; the same helper the
// cron siblings use. TAG_INJECT_REL deliberately names the real installed namespace path:
// this executable resolver checks both candidate files before use, so it is exempt from
// the Markdown-only published-core namespace-literal gate.
const TAG_INJECT_REL = 'skills/bossanova/boss-finalize/add-pr-numbers.sh'
const TAG_INJECT_CANDIDATES = Object.freeze([
  path.join(os.homedir(), '.claude', TAG_INJECT_REL),
  path.join(os.homedir(), '.codex', TAG_INJECT_REL),
])

/**
 * Resolve the tag-injection helper to whichever skill tree ships it. Prefers the Claude
 * install (unchanged for the common case); falls back to the Codex install so a
 * Codex-only host still reaches the finalize path. Returns the Claude candidate when
 * neither exists so any ENOENT surfaces against the expected primary location.
 * @param {(p: string) => boolean} [exists]
 * @returns {string}
 */
export function resolveTagInject(exists = fs.existsSync) {
  return TAG_INJECT_CANDIDATES.find((p) => exists(p)) ?? TAG_INJECT_CANDIDATES[0]
}

const TAG_INJECT = resolveTagInject()

// Declarative map of agent-driven finalize capability -> the concrete command/skill
// the spine invokes for it. Frozen so a consumer can read but not mutate it.
export const finalizeOperationMap = Object.freeze({
  injectPrTag: Object.freeze({
    command: TAG_INJECT,
    arg: 'prNumber',
    env: 'BASE_BRANCH',
    summary: 'rebase since PR base + inject [#<PR>] into any commit missing it',
  }),
  readyPr: Object.freeze({
    command: 'gh pr ready',
    arg: 'prNumber',
    guard: 'isDraft==true',
    summary: 'flip a draft PR to ready-for-review once green',
  }),
  repair: Object.freeze({
    skill: 'boss-repair',
    cap: 5,
    summary: 'fix failing checks / rebase conflicts / review comments, capped',
  }),
})

// Bound on the captured child stderr carried into a thrown error. A helper that
// floods stderr must not turn a rejection message into megabytes; the LAST bytes are
// kept because the reason a helper exits non-zero is what it printed last.
const STDERR_TAIL_LIMIT = 8 * 1024

/**
 * The trailing `limit` bytes of `text`, marked with an ellipsis when truncated.
 * Measured in BYTES, not code units, so a multi-byte reason is bounded by what it
 * actually costs.
 * @param {string|undefined} text
 * @param {number} [limit]
 * @returns {string}
 */
export function stderrTail(text, limit = STDERR_TAIL_LIMIT) {
  const buf = Buffer.from(text ?? '', 'utf8')
  if (buf.length <= limit) return buf.toString('utf8')
  return '…' + buf.subarray(buf.length - limit).toString('utf8')
}

/**
 * The message a failed child produces. `Command failed: <argv>` is preserved as the
 * first line — it is what the previous runner threw and what callers recognise — and
 * the helper's own reason is appended below it. Without the second half a rejection
 * reached the caller as the wrapper's generic failure with the child's real reason
 * unrecoverable (execFileSync with `stdio: 'inherit'` sets `stderr` to null).
 * @param {string} cmd
 * @param {string[]} args
 * @param {string|undefined} stderrText
 * @returns {string}
 */
export function runnerFailureMessage(cmd, args, stderrText) {
  const base = `Command failed: ${[cmd, ...args].join(' ')}`
  const reason = stderrTail(stderrText).trim()
  return reason === '' ? base : `${base}\n${reason}`
}

/**
 * Build the default child runner. stdout stays inherited (live, unchanged); stderr is
 * piped so it can be CAPTURED for the thrown error, then written straight through to
 * this process's stderr so the caller still sees it. Capturing without forwarding
 * would trade one reporting defect for another — the helper's progress output would
 * vanish from the terminal.
 *
 * Ordering caveat: stdout is inherited and therefore interleaves in real time, while
 * the piped stderr is written through when the child exits. A helper that interleaves
 * both streams will have its stderr appear after its stdout rather than between lines.
 * That is the price of a synchronous capture, and it is worth paying: the previous
 * runner interleaved correctly and could not report WHY anything failed.
 *
 * @param {{spawnImpl?: typeof spawnSync, errWrite?: (s: string) => void}} [deps]
 * @returns {(cmd: string, args: string[], opts?: object) => any}
 */
export function createDefaultRun({
  spawnImpl = spawnSync,
  errWrite = (s) => process.stderr.write(s),
} = {}) {
  return (cmd, args, opts) => {
    // A caller-supplied stdio is deliberately dropped: the capture is the point of
    // this runner, and an inherited stderr cannot be read back.
    const { stdio: _ignoredStdio, ...rest } = opts ?? {}
    const result = spawnImpl(cmd, args, {
      ...rest,
      stdio: ['inherit', 'inherit', 'pipe'],
      encoding: 'utf8',
    })
    if (typeof result.stderr === 'string' && result.stderr !== '') errWrite(result.stderr)
    // A spawn failure (ENOENT on the helper) is not a child rejection; surface the
    // system error unchanged rather than dressing it as an exit status.
    if (result.error) throw result.error
    if (result.status !== 0) {
      const err = new Error(runnerFailureMessage(cmd, args, result.stderr))
      err.status = result.status
      err.signal = result.signal
      // The raw capture, for a caller that wants to classify rather than print.
      err.stderr = result.stderr
      throw err
    }
    return result.stdout
  }
}

const defaultRun = createDefaultRun()

/**
 * Build the boss-finalize adapter.
 * @param {{runImpl?: (cmd: string, args: string[], opts: object) => any}} [config]
 * @returns {import('./adapter.mjs').FinalizeAdapter}
 */
export function createBossFinalizeAdapter({ runImpl = defaultRun } = {}) {
  return {
    finalize: 'boss-finalize',
    operationMap: finalizeOperationMap,
    // The exact constants the SKILL body's Steps 8-10 use today: tag format, the
    // boss-repair cap (Step 8), and the settle-loop cap (Step 10).
    policy: Object.freeze({ tagFormat: '[#<PR>]', repairCap: 5, settleCap: 3 }),
    injectPrTag(prNumber, { baseBranch } = {}) {
      return runImpl(TAG_INJECT, [String(prNumber)], {
        env: { ...process.env, BASE_BRANCH: baseBranch },
      })
    },
  }
}
