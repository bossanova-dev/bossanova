#!/usr/bin/env node
// Cron gate for boss-verify — the one gate that DISPATCHES instead of deciding.
//
// A cron fire always creates a new session, and verify work belongs in each PR's EXISTING session.
// So this gate does the work itself and exits non-zero, and cron spawns nothing:
//
//   1. `verify-route.mjs sweep`: discover the In Review PRs that need judging (verify-gate
//      `candidates`), judge each with the zero-token `judge`, then post/merge mechanically or route
//      `/boss-verify <pr> --claim <token>` into the PR's session (its `verify` chat, a new one, or a
//      `boss new --pr <n>` session for an orphan PR). Bounded: at most `--batch` judged PRs (default 3),
//      plus a separate `--batch` cap for verified-head merge retries, and
//      a 45 s budget inside the scheduler's 60 s gate timeout; the rest are `deferred`. A merge is
//      handed to a detached `verify-gate merge` and settled inside that budget; one still running
//      at the deadline is `dispatched: merge in flight (log …)` and finishes on its own.
//   2. Print one line per PR plus a summary line, so `gated` cron history stays readable.
//   3. Skip unchanged ticks: the gate runs the sweep with `skipUnchanged`, so a tick whose open PRs,
//      in-review tickets and sessions match the last quiet tick ends after one batched GitHub read
//      and prints `unchanged` (state in `<git common dir>/boss-verify/sweep-state.json`).
//
// Shared repos read fleet ownership through boss ls --all-daemons: remote owners are skipped;
// an unreadable fleet prevents new sessions while local sessions and mechanical verdicts continue.
//
// bossd also invokes this gate for one event PR; that path logs the exit code and never spawns a session.
// Exit codes (cron unless specified):
//   1  event: the named head was settled (handled or deliberately skipped)
//  69  event: the named head still needs judging, including errors and undispatchable runs
//   1  nothing to do, or every candidate handled (merged / parked / dispatched / skipped / deferred)
//   1  nothing changed since the last quiet tick (`unchanged`)
//   1  fail closed: a missing tracker credential, tracker/config/open-PR read failure, refused argument or error
//   1  --dry-run, always: a dry-run gate never fires a session
//   1  --local-sessions-only with `boss` or its session list unreadable: that mode never hands off to
//      a session, whose sweep would route the other machine's PRs too
//   0  undispatchable: a candidate needed `boss` and it is unreachable from here. Cron then starts a
//      session running `/boss-verify`, whose session env carries BOSS_BIN/BOSS_SOCKET and routes the
//      same way. Never 126/127, which the scheduler reads as "command missing".
//
// The gate's environment (decided, no scheduler change): the scheduler hands a gate the daemon's own
// env plus REPO_DIR — no BOSS_BIN, no BOSS_SOCKET. The configured tracker needs its credential:
// LINEAR_API_KEY for Linear, the host's gh login for GitHub. Both service managers bake the
// shared service PATH (Homebrew, /usr/local/bin, ~/.local/bin) and BOSS_SETTINGS_PATH into that env,
// and `resolveBossBinary` adds $BOSS_BIN and <repo>/bin/boss on top, so `boss` resolves for every
// standard install and finds the daemon socket through the same settings file. A non-standard host
// (a dev daemon run from a checkout against another repo, say) is what the exit-0 fallback covers;
// its cost is one fallback session per tick while candidates exist, visible in cron history.
//
// Register on the cron job (gate cwd = repo root), prompt `/boss-verify`:
//   node "$BOSS_VERIFY_TOOLBOX/cron-gates/boss-verify.mjs" [--batch N] [--budget-ms MS]
//     [--local-sessions-only] [selection flags]
// `--local-sessions-only` acts only on PRs with a live session on this daemon (verify-route.mjs).
// See boss-verify's references/cron-gate.md for the toolbox resolution.

import { parseArgs } from 'node:util'

import { loadSkillConfig } from '../skill-config.mjs'
import { configuredTrackerName, trackerCredentialGap } from '../tracker/adapter.mjs'
import { isMainModule } from '../main-module.mjs'
import { SELECTION_FLAGS } from '../selection.mjs'
import { parseSweepArgs, summaryLine, sweep } from '../verify/verify-route.mjs'

const PREFIX = 'boss-verify gate'
// gatecmd classifies EX_UNAVAILABLE as could-not-evaluate, so bossd retries this head.
const EXIT_COULD_NOT_EVALUATE = 69

/**
 * Run the sweep from an injected argv, env and deps, and map the outcome to an exit code.
 * @param {{argv?: string[], env?: NodeJS.ProcessEnv, deps?: {loadConfig?: typeof loadSkillConfig, cwd?: string} & Record<string, unknown>}} [options]
 * @returns {{exitCode: 0|1|69, lines: string[]}}
 */
export function evaluateBossVerifyGate({ argv = [], env = process.env, deps = {} } = {}) {
  const eventPrUrl = String(env.BOSS_VERIFY_EVENT_PR_URL ?? '').trim()
  if (
    eventPrUrl &&
    !/^https:\/\/github\.com\/[^/\s?#]+\/[^/\s?#]+\/pull\/[1-9]\d*$/.test(eventPrUrl)
  )
    return { exitCode: 1, lines: [`${PREFIX}: BOSS_VERIFY_EVENT_PR_URL is not a pull request URL`] }
  const failureExit = eventPrUrl ? EXIT_COULD_NOT_EVALUATE : 1
  let trackerName
  try {
    const config = (deps.loadConfig ?? loadSkillConfig)({ cwd: deps.cwd ?? process.cwd() })
    trackerName = configuredTrackerName({ env, config })
    const gap = trackerCredentialGap({ tracker: trackerName, env })
    if (gap) return { exitCode: failureExit, lines: [`${PREFIX}: ${gap}`] }
  } catch (error) {
    return { exitCode: failureExit, lines: [`${PREFIX}: ${error?.message ?? String(error)}`] }
  }
  let options
  let safetyNetMinutes
  try {
    // Gate-only flag: preserve every other token for the sweep's existing argument parser.
    const parsed = parseArgs({
      args: argv,
      options: {
        'safety-net-minutes': { type: 'string', default: '60' },
        batch: { type: 'string' },
        'budget-ms': { type: 'string' },
        'dry-run': { type: 'boolean' },
        'local-sessions-only': { type: 'boolean' },
        ...Object.fromEntries(
          Object.keys(SELECTION_FLAGS).map((flag) => [
            flag.slice(2),
            { type: 'string', multiple: true },
          ]),
        ),
      },
      allowPositionals: true,
      tokens: true,
    })
    const raw = parsed.values['safety-net-minutes']
    safetyNetMinutes = Number(raw)
    if (
      !/^\d+$/.test(raw) ||
      !Number.isSafeInteger(safetyNetMinutes) ||
      !Number.isSafeInteger(safetyNetMinutes * 60_000)
    )
      throw new Error('--safety-net-minutes must be an integer >= 0')
    const stripped = new Set()
    for (const token of parsed.tokens)
      if (token.kind === 'option' && token.name === 'safety-net-minutes') {
        stripped.add(token.index)
        if (!token.inlineValue) stripped.add(token.index + 1)
      }
    options = parseSweepArgs(argv.filter((_, index) => !stripped.has(index)))
  } catch (error) {
    return { exitCode: failureExit, lines: [`${PREFIX}: ${error.message}`] }
  }
  let result
  try {
    result = sweep(
      {
        ...options,
        ...(eventPrUrl ? { eventPrUrl } : { skipUnchanged: true }),
        ...(env.BOSS_WEBHOOKS_HEALTHY === '1' ? { safetyNetMs: safetyNetMinutes * 60_000 } : {}),
      },
      { ...deps, env, trackerName },
    )
  } catch (error) {
    return { exitCode: failureExit, lines: [`${PREFIX}: ${error?.message ?? String(error)}`] }
  }
  const lines = result.records.map((r) => r.line)
  if (result.idle) lines.push(`${PREFIX}: idle: ${result.idle}`)
  if (result.unchanged) lines.push(`${PREFIX}: unchanged since the last quiet tick`)
  if (result.fleetUnreadable)
    lines.push(
      `${PREFIX}: cross-daemon sessions unreadable (${result.fleetUnreadable}) — no new sessions this tick`,
    )
  if (result.undispatchable)
    lines.push(
      `undispatchable: ${result.undispatchable}${eventPrUrl || result.dryRun || result.fleetUnreadable ? '' : ' — exit 0 hands this run to a /boss-verify session'}`,
    )
  const prefix = eventPrUrl ? `${PREFIX} (event #${eventPrUrl.split('/').pop()})` : PREFIX
  lines.push(`${summaryLine(prefix, result.summary)}${result.dryRun ? ' (dry-run)' : ''}`)
  return {
    exitCode: eventPrUrl
      ? result.eventSettled
        ? 1
        : EXIT_COULD_NOT_EVALUATE
      : result.undispatchable && !result.fleetUnreadable && !result.dryRun
        ? 0
        : 1,
    lines,
  }
}

/** The process entry point: evaluate `argv`, print the lines, set the exit code. */
export function runBossVerifyGate(argv) {
  let outcome
  try {
    outcome = evaluateBossVerifyGate({ argv })
  } catch (error) {
    outcome = { exitCode: 1, lines: [`${PREFIX}: ${error?.message ?? String(error)}`] }
  }
  for (const line of outcome.lines) process.stdout.write(`${line}\n`)
  process.exitCode = outcome.exitCode
}

if (isMainModule(import.meta.url)) runBossVerifyGate(process.argv.slice(2))
