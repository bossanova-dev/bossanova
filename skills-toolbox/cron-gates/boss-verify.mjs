#!/usr/bin/env node
// Cron gate for boss-verify — the one gate that DISPATCHES instead of deciding.
//
// A cron fire always creates a new session, and verify work belongs in each PR's EXISTING session.
// So this gate does the work itself and exits non-zero, and cron spawns nothing:
//
//   1. `verify-route.mjs sweep`: discover the In Review PRs that need judging (verify-gate
//      `candidates`), judge each with the zero-token `judge`, then post/merge mechanically or route
//      `/boss-verify <pr> --claim <token>` into the PR's session (its `verify` chat, a new one, or a
//      `boss new --pr <n>` session for an orphan PR). Bounded: at most `--batch` PRs (default 3) and
//      a 45 s budget inside the scheduler's 60 s gate timeout; the rest are `deferred`.
//   2. Print one line per PR plus a summary line, so `gated` cron history stays readable.
//
// Exit codes:
//   1  nothing to do, or every candidate handled (merged / parked / dispatched / skipped / deferred)
//   1  fail closed: a tracker, config or `gh pr list` failure, a refused argument, any thrown error
//   1  --dry-run, always: a dry-run gate never fires a session
//   0  undispatchable: a candidate needed `boss` and it is unreachable from here. Cron then starts a
//      session running `/boss-verify`, whose session env carries BOSS_BIN/BOSS_SOCKET and routes the
//      same way. Never 126/127, which the scheduler reads as "command missing".
//
// The gate's environment (decided, no scheduler change): the scheduler hands a gate the daemon's own
// env plus REPO_DIR and LINEAR_API_KEY — no BOSS_BIN, no BOSS_SOCKET. Both service managers bake the
// shared service PATH (Homebrew, /usr/local/bin, ~/.local/bin) and BOSS_SETTINGS_PATH into that env,
// and `resolveBossBinary` adds $BOSS_BIN and <repo>/bin/boss on top, so `boss` resolves for every
// standard install and finds the daemon socket through the same settings file. A non-standard host
// (a dev daemon run from a checkout against another repo, say) is what the exit-0 fallback covers;
// its cost is one fallback session per tick while candidates exist, visible in cron history.
//
// Register on the cron job (gate cwd = repo root), prompt `/boss-verify`:
//   node "$BOSS_VERIFY_TOOLBOX/cron-gates/boss-verify.mjs" [--batch N] [--budget-ms MS] [selection flags]
// See boss-verify's references/cron-gate.md for the toolbox resolution.

import { isMainModule } from '../main-module.mjs'
import { parseSweepArgs, summaryLine, sweep } from '../verify-route.mjs'

const PREFIX = 'boss-verify gate'

/**
 * Run the sweep from an injected argv, env and deps, and map the outcome to an exit code.
 * @returns {{exitCode: 0|1, lines: string[]}}
 */
export function evaluateBossVerifyGate({ argv = [], env = process.env, deps = {} } = {}) {
  if (typeof env.LINEAR_API_KEY !== 'string' || env.LINEAR_API_KEY.trim() === '')
    return { exitCode: 1, lines: [`${PREFIX}: LINEAR_API_KEY is not set`] }
  let options
  try {
    options = parseSweepArgs(argv)
  } catch (error) {
    return { exitCode: 1, lines: [`${PREFIX}: ${error.message}`] }
  }
  let result
  try {
    result = sweep(options, { ...deps, env })
  } catch (error) {
    return { exitCode: 1, lines: [`${PREFIX}: ${error?.message ?? String(error)}`] }
  }
  const lines = result.records.map((r) => r.line)
  if (result.undispatchable)
    lines.push(
      `undispatchable: ${result.undispatchable}${result.dryRun ? '' : ' — exit 0 hands this run to a /boss-verify session'}`,
    )
  lines.push(`${summaryLine(PREFIX, result.summary)}${result.dryRun ? ' (dry-run)' : ''}`)
  return { exitCode: result.undispatchable && !result.dryRun ? 0 : 1, lines }
}

if (isMainModule(import.meta.url)) {
  let outcome
  try {
    outcome = evaluateBossVerifyGate({ argv: process.argv.slice(2) })
  } catch (error) {
    outcome = { exitCode: 1, lines: [`${PREFIX}: ${error?.message ?? String(error)}`] }
  }
  for (const line of outcome.lines) process.stdout.write(`${line}\n`)
  process.exitCode = outcome.exitCode
}
