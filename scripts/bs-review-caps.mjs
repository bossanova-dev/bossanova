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
// `scripts/bs-review-caps.test.mjs`; keep them byte-identical.
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
 * @param {number} rounds
 * @returns {string}
 */
export function cappedSentinel(rounds) {
  return `${CAPPED_PREFIX} open must-fix findings remain after ${rounds} rounds.`
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

// Thin CLI (the surface the skill prose invokes):
//   node scripts/bs-review-caps.mjs rounds            → effective cap (reads BS_REVIEW_MAX_ROUNDS)
//   node scripts/bs-review-caps.mjs sentinel clean    → the clean sentinel line
//   node scripts/bs-review-caps.mjs sentinel capped N → the capped sentinel line for N rounds
//   node scripts/bs-review-caps.mjs match "<line>"    → JSON classification of a sentinel line
if (import.meta.url === `file://${process.argv[1]}`) {
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
  } else {
    process.stderr.write(
      'usage: bs-review-caps.mjs <rounds | sentinel clean | sentinel capped <N> | match "<line>">\n',
    )
    process.exit(2)
  }
}
