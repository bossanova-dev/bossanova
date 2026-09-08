// gate-outcome.mjs
//
// One line per gate per run. Skill gates accumulate — each one justified by an
// incident — and nothing records whether any of them has ever fired since. That
// makes every "should this gate stay?" question a judgement call against a
// plausible story, and plausible stories always favour keeping the gate. This
// module is the counting mechanism that replaces the argument: a gate invoked
// inside a skill run leaves exactly one line saying what it decided.
//
// Three properties are load-bearing, and each one is a structural choice rather
// than a tested-for hope:
//
//   1. **It never throws.** A telemetry failure must never turn a passing gate
//      into a failing run, so every filesystem and formatting path returns a
//      boolean instead of propagating. The caller's verdict and exit code are
//      untouched by whether recording worked.
//
//   2. **A line structurally cannot carry a payload.** The record is a fixed
//      four-field TSV of slug-validated tokens. A gate id or reason that does
//      not FULLY match its slug pattern is replaced wholesale with a fixed
//      placeholder — never truncated, because a truncation still emits a prefix
//      of whatever it was handed, and "whatever it was handed" is how a plan
//      body or a credential reaches a file. Full-match-or-placeholder is what
//      makes "no plan content, no credential" true by construction.
//
//   3. **It is inert when there is no run to scope to.** With neither an
//      explicit destination nor any run-id env var, `recordGateOutcome` writes
//      nothing and returns falsey. Plain `node --test` and CI runs therefore
//      produce no surprise files; only a real skill run records.
//
// Format rationale: JSON-lines was rejected. A torn final line — a run killed
// mid-append — breaks a JSON reader for that line, and the sibling reader must
// stay able to read a partially written run. A TSV of validated slugs degrades
// to "the last line is short"; every complete line still parses.
//
// Node built-ins only — cron worktrees are dependency-free. Mirrors the shape of
// bs-run-sentinel.mjs (module + exported constants pinned by its test file).

import { appendFileSync, mkdirSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'

/**
 * The closed, byte-stable outcome vocabulary. `pass` means the gate evaluated
 * and permitted the run to continue; `fire` means it refused, or could not
 * evaluate. Anything else is not an outcome and is refused rather than coerced —
 * inventing a verdict for a caller's typo is how a gate's firing rate becomes
 * fiction.
 */
export const GATE_OUTCOMES = Object.freeze(['pass', 'fire'])

/** Env var naming an explicit destination file. Wins over every derived path. */
export const GATE_OUTCOME_FILE_ENV = 'BOSS_GATE_OUTCOME_FILE'

/**
 * Ordered env vars a run-scoped default destination is derived from. First
 * non-empty wins. `BOSS_SESSION_ID` is last and is the one a real cron session
 * always carries, so the default path engages in production rather than only
 * under an explicitly configured test.
 */
export const GATE_RUN_SCOPE_ENV = Object.freeze([
  'BOSS_GATE_RUN_ID',
  'BOSS_AGENT_SESSION_ID',
  'BOSS_SESSION_ID',
])

/** Directory under the OS temp root that derived destinations live in. */
export const GATE_OUTCOME_DIR = 'boss-gate-outcomes'

/** Replacement for a gate id that fails slug validation. */
export const UNSPECIFIED_GATE_ID = 'unspecified-gate'

/** Replacement for a reason that is absent, over-long, or fails slug validation. */
export const UNSPECIFIED_REASON = 'unspecified'

/** Length ceilings. Over-length values become the placeholder — never a prefix. */
export const MAX_GATE_ID_LENGTH = 64
export const MAX_REASON_LENGTH = 48

// A slug is lowercase alphanumerics joined by single hyphens. A gate id may
// additionally use `.` as a verb separator (`plan-run-guards.premises`), because
// a multi-verb CLI's verbs are genuinely different gates with different firing
// rates and collapsing them would hide exactly the signal this exists to expose.
const SLUG_SOURCE = '[a-z0-9]+(?:-[a-z0-9]+)*'
const GATE_ID_SOURCE = `${SLUG_SOURCE}(?:\\.${SLUG_SOURCE})*`

export const GATE_ID_PATTERN = new RegExp(`^${GATE_ID_SOURCE}$`)
export const GATE_REASON_PATTERN = new RegExp(`^${SLUG_SOURCE}$`)

/**
 * The complete shape of one recorded line, without its trailing newline. The
 * "no payload" property is pinned against THIS pattern rather than against a
 * sentence: a line that matches cannot contain a newline, a tab, a space, an
 * `@`, a `/`, or any uppercase byte, which is most of what a leaked plan body or
 * credential is made of.
 */
export const GATE_OUTCOME_LINE_PATTERN = new RegExp(
  `^\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}\\.\\d{3}Z` +
    `\\t${GATE_ID_SOURCE}` +
    `\\t(?:${GATE_OUTCOMES.join('|')})` +
    `\\t${SLUG_SOURCE}$`,
)

/**
 * Full-match-or-placeholder. Deliberately NOT a sanitizer: a value that is not
 * already a slug is discarded entirely, so no prefix, suffix or transformed
 * remnant of the caller's input can reach the file.
 * @param {unknown} raw
 * @param {RegExp} pattern
 * @param {number} maxLength
 * @param {string} placeholder
 * @returns {string}
 */
function slugOrPlaceholder(raw, pattern, maxLength, placeholder) {
  if (typeof raw !== 'string') return placeholder
  if (raw.length === 0 || raw.length > maxLength) return placeholder
  return pattern.test(raw) ? raw : placeholder
}

/**
 * Format one record. Returns `null` when `outcome` is not a published
 * GATE_OUTCOMES member — an unknown outcome is refused, never guessed.
 * @param {string} gateId
 * @param {string} outcome
 * @param {string} [reason]
 * @param {{now?: Date}} [opts]
 * @returns {string|null} the line, newline-terminated
 */
export function formatGateOutcomeLine(gateId, outcome, reason, opts = {}) {
  if (!GATE_OUTCOMES.includes(outcome)) return null
  const now = opts.now instanceof Date ? opts.now : new Date()
  const timestamp = now.toISOString()
  const id = slugOrPlaceholder(gateId, GATE_ID_PATTERN, MAX_GATE_ID_LENGTH, UNSPECIFIED_GATE_ID)
  const why = slugOrPlaceholder(reason, GATE_REASON_PATTERN, MAX_REASON_LENGTH, UNSPECIFIED_REASON)
  return `${timestamp}\t${id}\t${outcome}\t${why}\n`
}

/**
 * Normalise a run id into a filesystem-safe name. Unlike the line fields this
 * DOES rewrite rather than discard: the run scope never appears in a recorded
 * line, so there is nothing to leak here, and being inert because a session id
 * carried an unexpected byte would silently disable the whole mechanism in
 * production — the failure mode this feature exists to avoid.
 * @param {string} raw
 * @returns {string|null}
 */
function runScopeSlug(raw) {
  const normalized = String(raw)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 64)
  return normalized === '' ? null : normalized
}

/**
 * Resolve the destination file, or `null` when this process has no run to scope
 * to. An explicit `BOSS_GATE_OUTCOME_FILE` always wins; otherwise the first
 * non-empty `GATE_RUN_SCOPE_ENV` var derives a per-run file under the OS temp
 * root. `null` is the inert case and is not an error.
 * @param {NodeJS.ProcessEnv} [env]
 * @returns {string|null}
 */
export function resolveGateOutcomePath(env = process.env) {
  const explicit = env?.[GATE_OUTCOME_FILE_ENV]
  if (typeof explicit === 'string' && explicit.trim() !== '') return explicit.trim()
  for (const name of GATE_RUN_SCOPE_ENV) {
    const raw = env?.[name]
    if (typeof raw !== 'string' || raw.trim() === '') continue
    const slug = runScopeSlug(raw.trim())
    if (slug) return join(tmpdir(), GATE_OUTCOME_DIR, `${slug}.tsv`)
  }
  return null
}

/**
 * Append one gate-outcome line. Never throws: every failure — an unresolvable
 * destination, an unwritable path, an unknown outcome — is reported as `false`
 * so a caller's verdict and exit code are unaffected by telemetry.
 * @param {string} gateId
 * @param {string} outcome  a GATE_OUTCOMES member
 * @param {string} [reason] a slug; anything else records as UNSPECIFIED_REASON
 * @param {{env?: NodeJS.ProcessEnv, now?: Date, path?: string}} [opts]
 * @returns {boolean} true iff a line was appended
 */
export function recordGateOutcome(gateId, outcome, reason, opts = {}) {
  try {
    const destination = opts.path ?? resolveGateOutcomePath(opts.env ?? process.env)
    if (!destination) return false
    const line = formatGateOutcomeLine(gateId, outcome, reason, opts)
    if (line === null) return false
    mkdirSync(dirname(destination), { recursive: true })
    appendFileSync(destination, line)
    return true
  } catch {
    // Deliberately swallowed. See the header: a gate that fails because its
    // telemetry failed is a worse defect than an unrecorded outcome.
    return false
  }
}

/**
 * Build a one-shot recorder for a single gate invocation.
 *
 * The latch is the mechanism that makes "exactly one outcome per invocation"
 * structural rather than a discipline every adopting guard has to re-earn. A
 * guard may call `record` at the specific branch that names a reason AND again
 * from a generic wrapper around `main()`; only the first call writes. That is
 * what makes it safe for a guard to record and then throw, and what stops a
 * multi-branch CLI from double-counting its own firing rate.
 * @param {string} gateId
 * @param {{env?: NodeJS.ProcessEnv, now?: Date, path?: string}} [opts]
 * @returns {{gateId: string, recorded: boolean, record: (outcome: string, reason?: string) => boolean}}
 */
export function createGateRecorder(gateId, opts = {}) {
  let latched = false
  return {
    gateId,
    get recorded() {
      return latched
    },
    record(outcome, reason) {
      if (latched) return false
      latched = true
      return recordGateOutcome(gateId, outcome, reason, opts)
    },
  }
}
