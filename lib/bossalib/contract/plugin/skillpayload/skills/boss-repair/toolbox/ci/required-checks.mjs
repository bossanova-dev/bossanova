// ci/required-checks.mjs — the one definition of a repository's required checks and of whether a
// required check has run on a head.
//
// A `.boss-skills.json` `ci.required` block names check-run names that must have RUN on a PR head
// before it can be green:
//
//   "ci": { "required": { "checks": ["go-test"], "exemptBases": ["staging"] } }
//
// `checks` are exact names; `exemptBases` are PR base branches the requirement does not apply to.
// An absent block, or an empty `checks`, requires nothing. The Go peer is
// `lib/bossalib/kit/vcs/checks_verdict.go` (`RequiredChecks`, `EvaluateChecksRequiring`).
//
// "Ran" means a non-skipped instance: a name is satisfied when an entry with that exact name is
// pending, unclassified, failed, passed, or completed with a conclusion other than SKIPPED or
// NEUTRAL. A name that is absent, or present only as skipped/neutral, has not run — a draft
// guard that skips a heavy job still attaches a skipped check under its name, and counting that
// presence as a run would read a job nobody started as requested.
//
// Pure and offline; Node builtins only (none needed).

const NOT_RUN_CONCLUSIONS = new Set(['SKIPPED', 'NEUTRAL'])

function describe(value) {
  return value === undefined ? 'undefined' : JSON.stringify(value)
}

function nameList(value, field) {
  if (value === undefined) return []
  if (!Array.isArray(value)) {
    throw new TypeError(
      `ci.required.${field} must be an array of check names, got ${describe(value)}`,
    )
  }
  for (const name of value) {
    if (typeof name !== 'string' || name.trim() === '') {
      throw new TypeError(
        `ci.required.${field} entries must be non-empty strings, got ${describe(name)}`,
      )
    }
  }
  return value.map((name) => name.trim())
}

// parseRequiredChecks validates a `ci.required` block and returns `{checks, exemptBases}`. An
// absent block (undefined or null) is `{checks: [], exemptBases: []}`. A malformed block throws a
// TypeError whose message names `ci.required`.
export function parseRequiredChecks(block) {
  if (block === undefined || block === null) return { checks: [], exemptBases: [] }
  if (typeof block !== 'object' || Array.isArray(block)) {
    throw new TypeError(`ci.required must be an object, got ${describe(block)}`)
  }
  for (const key of Object.keys(block)) {
    if (key !== 'checks' && key !== 'exemptBases') {
      throw new TypeError(`ci.required has unknown property ${describe(key)}`)
    }
  }
  return {
    checks: nameList(block.checks, 'checks'),
    exemptBases: nameList(block.exemptBases, 'exemptBases'),
  }
}

// requiredChecksFor returns the names required on a PR into `baseRef`: `[]` when nothing is
// required or the base is exempt. `block` is a raw `ci.required` block or a parsed one.
export function requiredChecksFor(block, baseRef = '') {
  const { checks, exemptBases } = parseRequiredChecks(block)
  if (checks.length === 0) return []
  const base = typeof baseRef === 'string' ? baseRef.replace(/^refs\/heads\//, '') : ''
  if (exemptBases.includes(base)) return []
  return checks
}

// entryRan reports whether one normalized check entry (`{name, kind, conclusion, bucket}`, as
// pr-check-state normalizes them) counts as a run. Only a `skipped` entry can fail to count, and
// only when it says it did not run: SKIPPED or NEUTRAL, or a bare SKIPPING bucket with no
// conclusion. STALE classifies as skipped but is a result a check reaches only by running.
export function entryRan(entry) {
  if (entry?.kind !== 'skipped') return true
  const conclusion = String(entry.conclusion ?? '')
    .trim()
    .toUpperCase()
  if (conclusion === '') return false
  return !NOT_RUN_CONCLUSIONS.has(conclusion)
}

// notRequestedChecks returns, in requirement order, the required names with no run among
// `entries`.
export function notRequestedChecks(entries, required) {
  if (!Array.isArray(required) || required.length === 0) return []
  const ran = new Set()
  for (const entry of Array.isArray(entries) ? entries : []) {
    if (typeof entry?.name === 'string' && entryRan(entry)) ran.add(entry.name)
  }
  return [...new Set(required)].filter((name) => !ran.has(name))
}
