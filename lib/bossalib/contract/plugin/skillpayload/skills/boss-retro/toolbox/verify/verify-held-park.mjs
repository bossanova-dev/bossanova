// Whether a standing park on a head holds when it is re-judged with no approval, send-back or new
// results. The judge reports a held park as it is, so nothing is re-posted.

/**
 * The policy fingerprint carries this, so a change to when a park holds re-judges every park once.
 * 2: a park no longer holds while an applicable extension has no recorded run on its head; cores
 * before it re-recorded the fingerprint over such parks, which then never re-judged.
 */
export const PARK_HOLD_REVISION = 2

// Merge failures stand independently of verify policy.
const NON_POLICY_CODES = new Set(['no-session', 'worktree-diverged', 'worktree-unsynced'])

/**
 * The applicable extensions with no recorded run on this head: those the newest extension record
 * (`[{extension, outcome}]`, or null when none) does not name. A malfunction park is itself the
 * record that its extensions ran, and a park naming a merge failure stands whatever they say, so
 * both name none.
 */
export function extensionsNotRun(codes, { applicable = [], record = null } = {}) {
  if (codes.some((code) => code === 'extension-failed' || NON_POLICY_CODES.has(code))) return []
  const ran = new Set(Array.isArray(record) ? record.map((entry) => entry.extension) : [])
  return applicable.filter((name) => !ran.has(name))
}

/**
 * Does the current base still raise every code of a standing park? `fresh` comes from the zero-token
 * judge; collector-only `cannot-assess` stands only while that judge still raises it. Other stale
 * codes stand when an extension that ran before the park (`extensionsRan`) may still raise them.
 * The clean-run `record` names which `applicable` extension raised each code; a malfunction park,
 * or a park without a record, conservatively stands while any extension still applies.
 */
export function parkStillRaised(
  codes,
  { fresh = [], repairing = false, extensionsRan = true, applicable = [], record = null } = {},
) {
  const live = new Set(applicable)
  const judged = codes.includes('extension-failed') ? null : record
  const byExtension = (code) =>
    extensionsRan &&
    code !== 'cannot-assess' &&
    (Array.isArray(judged)
      ? judged.some((e) => e.outcome === code && live.has(e.extension))
      : live.size > 0)
  return codes.every(
    (code) =>
      fresh.includes(code) ||
      (code === 'repair-exhausted' ? repairing : NON_POLICY_CODES.has(code) || byExtension(code)),
  )
}
