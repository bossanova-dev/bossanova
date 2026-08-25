// Single source of truth for the review-round cap and the byte-stable
// `bs-review clean:` / `bs-review capped:` sentinels shared by the `bs-review`
// skill and `bs-implement`'s Step 6 review loop.
//
// The round cap is env-configurable but **lower-only**: `BS_REVIEW_MAX_ROUNDS`
// may only *reduce* the cap below the hard default, never raise it — a
// pathological session must never be able to grant itself more review rounds.
// Any invalid / absent / too-high value falls back to the default. This mirrors
// wondercanvas' `WC_AUTO_REVIEW_MAX_ROUNDS` contract.
//
// The sentinel prefixes are the contract downstream callers route on
// (`bs-implement` Step 6c). They are pinned byte-for-byte by
// `bs-review-caps.test.mjs`; keep them byte-identical.
//
// Node built-ins only — cron worktrees are dependency-free.

// The hard default (and ceiling) for the review-round cap. The env may lower
// this but never raise it.
export const DEFAULT_REVIEW_MAX_ROUNDS = 3

/**
 * Resolve an effective round cap from a raw env value, clamped **lower-only**
 * against `defaultCap`: a strict positive integer in `[1, defaultCap]` is
 * honored; anything else — non-integer, `< 1`, `> defaultCap`, absent, empty —
 * falls back to `defaultCap`. The env can therefore only *lower* the cap.
 * @param {string|number|null|undefined} raw
 * @param {number} [defaultCap]
 * @returns {number}
 */
export function resolveMaxRounds(raw, defaultCap = DEFAULT_REVIEW_MAX_ROUNDS) {
  if (raw === null || raw === undefined) return defaultCap
  const trimmed = String(raw).trim()
  // Strict base-10 integer only: rejects '', signs, decimals, hex, exponents.
  if (!/^\d+$/.test(trimmed)) return defaultCap
  const n = Number.parseInt(trimmed, 10)
  // Lower-only clamp: never below 1, never above the default ceiling.
  if (n < 1 || n > defaultCap) return defaultCap
  return n
}

/**
 * The effective review-round cap, reading `BS_REVIEW_MAX_ROUNDS` from `env`.
 * @param {Record<string, string|undefined>} [env]
 * @returns {number}
 */
export function reviewMaxRounds(env = process.env) {
  return resolveMaxRounds(env.BS_REVIEW_MAX_ROUNDS, DEFAULT_REVIEW_MAX_ROUNDS)
}

// Sentinel prefixes downstream callers route on — byte-identical contract.
export const CLEAN_PREFIX = 'bs-review clean:'
export const CAPPED_PREFIX = 'bs-review capped:'

/** The clean sentinel (no open must-fix findings). Byte-identical. */
export function cleanSentinel() {
  return `${CLEAN_PREFIX} no open must-fix findings.`
}

/**
 * The capped sentinel — a fixed prefix with only the round-count tail dynamic.
 *
 * The wording names BOTH things that cap a run. A run can cap with zero open
 * must-fix findings when unrepaired `invalid` entries are the only blocker, and
 * the old "open must-fix findings remain" text asserted the wrong reason there.
 * Routing is unaffected: callers match on `CAPPED_PREFIX` and `matchSentinel`
 * parses only the `after N rounds.` tail.
 *
 * @param {number} rounds
 * @returns {string}
 */
export function cappedSentinel(rounds) {
  return `${CAPPED_PREFIX} unresolved must-fix findings or invalid evidence remain after ${rounds} rounds.`
}

/**
 * Classify a printed sentinel line. Routing matches on the fixed prefix; for a
 * capped line the trailing round count is extracted when present.
 * @param {string} line
 * @returns {{status:'clean'}|{status:'capped',rounds:number|null}|null}
 */
export function matchSentinel(line) {
  if (typeof line !== 'string') return null
  const s = line.trim()
  if (s.startsWith(CLEAN_PREFIX)) return { status: 'clean' }
  if (s.startsWith(CAPPED_PREFIX)) {
    const m = s.match(/after\s+([1-9]\d*)\s+rounds?\./)
    return m ? { status: 'capped', rounds: Number.parseInt(m[1], 10) } : null
  }
  return null
}

// ---------------------------------------------------------------------------
// Fix-round admission — the deadline gate plus the must-fix override.
// ---------------------------------------------------------------------------
//
// A caller's wall-clock deadline is priced per leg: a fix round is admitted only
// when its WHOLE allowance still remains. That is the right overrun policy and
// the wrong value policy — it refuses a round that would have closed a located,
// never-attempted must-fix, and the run's only honest terminal state then names
// the clock rather than the finding.
//
// `admitFixRound` is the decision table for that admission. It is **pure**: it
// reads no clock and no env, so the caller passes the remainder it re-derived
// from `date +%s` at the boundary. The override is bounded twice over — by
// `MUSTFIX_OVERRUN_ROUNDS` (one extra round, never more) and by the round cap,
// which is evaluated FIRST and is never overridden. Overriding the cap would
// break `resolveMaxRounds`' lower-only contract above: a pathological session
// must never be able to grant itself more review rounds.

/** The price of one fix→confirm round in seconds — `FIX_ROUND_MINUTES * 60` in the skill prose. */
export const DEFAULT_FIX_ROUND_SECONDS = 1200

/**
 * How many rounds the must-fix override may admit past the deadline, in total.
 * One: the whole allowance then sits inside the caller's post-review reserve,
 * so an override round is self-sufficient and needs no second absolute bound
 * threaded across the skill boundary.
 */
export const MUSTFIX_OVERRUN_ROUNDS = 1

/**
 * The overrun allowance expressed in seconds — the REPORTED total for the run's
 * overrun ledger field, not a gate input. The gate compares round counts.
 */
export const MUSTFIX_OVERRUN_SECONDS = MUSTFIX_OVERRUN_ROUNDS * DEFAULT_FIX_ROUND_SECONDS

/** The closed reason set `admitFixRound` returns over. */
export const ADMIT_FIX_ROUND_REASONS = Object.freeze([
  'within-budget',
  'mustfix-override',
  'no-open-mustfix',
  'all-attempted',
  'overrun-exhausted',
  'round-cap',
])

/**
 * A non-negative integer count, or `fallback`. Strings are rejected outright
 * rather than coerced: every caller of this helper falls back in the REFUSING
 * direction, so a malformed count can never widen the allowance.
 * @param {unknown} raw
 * @param {number} fallback
 * @returns {number}
 */
function safeCount(raw, fallback) {
  return typeof raw === 'number' && Number.isInteger(raw) && raw >= 0 ? raw : fallback
}

/**
 * Decide whether to admit one more fix→confirm round. Pure — no clock, no env.
 *
 * Order is the contract, not an implementation detail:
 *   1. round cap  — evaluated first, never overridden (lower-only contract)
 *   2. no open must-fix — the override buys nothing, so today's refusal stands
 *   3. whole allowance remains (or no deadline at all) — ordinary admission
 *   4. below the allowance — only an UNATTEMPTED must-fix overrides, and only
 *      while overrun allowance remains
 *
 * `remainingSeconds: null` (or absent) means **no deadline was supplied**, never
 * a deadline of `0` — the distinction the skill's gate turns on. An unreadable
 * remainder falls back to `0`, which routes through the bounded override rather
 * than granting free admission.
 *
 * @param {object} input
 * @param {number|null} [input.remainingSeconds] seconds left against the caller's deadline; `null` = no deadline
 * @param {number} [input.fixRoundSeconds] the price of one round
 * @param {boolean} [input.openMustFix] any must-fix finding still open
 * @param {boolean} [input.unattemptedMustFix] at least one open must-fix no fix round has dispatched against
 * @param {number} [input.roundsUsed] fix rounds already run
 * @param {number} [input.maxRounds] the effective round cap (clamped lower-only)
 * @param {number} [input.overrunRoundsUsed] override rounds already spent
 * @returns {{admit: boolean, reason: string}}
 */
export function admitFixRound({
  remainingSeconds = null,
  fixRoundSeconds = DEFAULT_FIX_ROUND_SECONDS,
  openMustFix = false,
  unattemptedMustFix = false,
  roundsUsed = 0,
  maxRounds = DEFAULT_REVIEW_MAX_ROUNDS,
  overrunRoundsUsed = 0,
} = {}) {
  // 1. The round cap bounds ATTEMPT COUNT and is never overridden. A run that
  //    exhausts it has attempted the finding up to `maxRounds` times, which is
  //    already a lawful terminal state.
  const cap = resolveMaxRounds(maxRounds)
  // Fail closed: an unreadable round count is treated as already at the cap.
  const used = safeCount(roundsUsed, cap)
  if (used >= cap) return { admit: false, reason: 'round-cap' }

  // 2. Nothing open to fix — the override exists only to close a must-fix, so a
  //    budget large enough to admit a round cannot reach `within-budget` here.
  if (!openMustFix) return { admit: false, reason: 'no-open-mustfix' }

  // 3. The ordinary gate: the WHOLE allowance must remain. No deadline = no cap.
  const price =
    typeof fixRoundSeconds === 'number' && Number.isFinite(fixRoundSeconds) && fixRoundSeconds > 0
      ? fixRoundSeconds
      : DEFAULT_FIX_ROUND_SECONDS
  if (remainingSeconds === null || remainingSeconds === undefined) {
    return { admit: true, reason: 'within-budget' }
  }
  // Fail closed: an unreadable remainder is treated as spent, not as unbounded.
  const remaining =
    typeof remainingSeconds === 'number' &&
    Number.isFinite(remainingSeconds) &&
    remainingSeconds > 0
      ? remainingSeconds
      : 0
  if (remaining >= price) return { admit: true, reason: 'within-budget' }

  // 4. Below the allowance. Only a located must-fix that no round has yet been
  //    dispatched against justifies the overrun; an attempted-and-failed finding
  //    is a lawful terminal state and must not spend a second allowance.
  if (!unattemptedMustFix) return { admit: false, reason: 'all-attempted' }
  // Fail closed: an unreadable overrun count is treated as already spent.
  const overrunUsed = safeCount(overrunRoundsUsed, MUSTFIX_OVERRUN_ROUNDS)
  if (overrunUsed >= MUSTFIX_OVERRUN_ROUNDS) return { admit: false, reason: 'overrun-exhausted' }
  return { admit: true, reason: 'mustfix-override' }
}

// Thin CLI (the surface the skill prose invokes):
//   node bs-review-caps.mjs rounds            → effective cap (reads BS_REVIEW_MAX_ROUNDS)
//   node bs-review-caps.mjs sentinel clean    → the clean sentinel line
//   node bs-review-caps.mjs sentinel capped N → the capped sentinel line for N rounds
//   node bs-review-caps.mjs match "<line>"    → JSON classification of a sentinel line
//   node bs-review-caps.mjs admit-fix-round '<json>'  → JSON {admit,reason} for one fix round
import { isMainModule } from './main-module.mjs'

if (isMainModule(import.meta.url)) {
  const [cmd, ...rest] = process.argv.slice(2)
  if (cmd === 'rounds') {
    process.stdout.write(`${reviewMaxRounds()}\n`)
  } else if (cmd === 'sentinel' && rest[0] === 'clean') {
    process.stdout.write(`${cleanSentinel()}\n`)
  } else if (cmd === 'sentinel' && rest[0] === 'capped') {
    // rest[1] is the actual round count reached; require a positive integer.
    // `0` is not a valid round count because the loop counter starts at 1.
    const raw = String(rest[1] ?? '').trim()
    if (!/^[1-9]\d*$/.test(raw)) {
      process.stderr.write('sentinel capped requires a positive integer round count\n')
      process.exit(2)
    }
    const rounds = Number.parseInt(raw, 10)
    process.stdout.write(`${cappedSentinel(rounds)}\n`)
  } else if (cmd === 'match') {
    process.stdout.write(`${JSON.stringify(matchSentinel(rest[0] ?? ''))}\n`)
  } else if (cmd === 'admit-fix-round') {
    // One JSON object argument, printed back as the same `{admit,reason}` the
    // function returns — so the invocation the skill prose cites cannot drift
    // from the surface the decision table tests.
    const raw = rest[0] ?? ''
    let input
    try {
      input = JSON.parse(raw)
    } catch {
      input = undefined
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      process.stderr.write('admit-fix-round requires one JSON object argument\n')
      process.exit(2)
    }
    process.stdout.write(`${JSON.stringify(admitFixRound(input))}\n`)
  } else {
    process.stderr.write(
      'usage: bs-review-caps.mjs <rounds | sentinel clean | sentinel capped <N> | match "<line>" | admit-fix-round \'<json>\'>\n',
    )
    process.exit(2)
  }
}
