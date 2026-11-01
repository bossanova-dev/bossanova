export const VERIFY_CONTEXT = 'boss/verify'

// The description prefixes are a contract with the `boss/verify` status renderer and with candidate
// discovery: `verifying…` is a live claim, `waiting:` is an unclaimed head, `needs human:` is a
// policy park, `cannot assess:` is a head still unassessed after reruns that the next run re-judges, and
// `verified` / `defect:` are terminal. Change them here and nowhere else.
export const VERIFY_DESCRIPTIONS = Object.freeze({
  verifying: 'verifying…',
  waiting: 'waiting:',
  cannotAssess: 'cannot assess:',
  needsHuman: 'needs human:',
  verified: 'verified',
  verifiedApproved: 'verified (approved)',
  defect: 'defect:',
  blocked: 'blocked:',
  // Appended to a `defect:` description when the defect was also handed to a human (auto-repair
  // off or unreadable), so `approval` can tell a parked defect from one repair is working on.
  defectParkedSuffix: ', needs human',
})

// The `candidates` skip reason for a PR outside its `--pr-urls`: the PR's session lives on another
// machine, so a sweep run with `--local-sessions-only` reads nothing of it.
export const REMOTE_SESSION_REASON = 'remote-session'

export const isSendBack = (description) =>
  typeof description === 'string' &&
  [VERIFY_DESCRIPTIONS.defect, VERIFY_DESCRIPTIONS.blocked].some((prefix) =>
    description.startsWith(prefix),
  )

export const isCannotAssess = (status) =>
  typeof status?.description === 'string' &&
  status.description.startsWith(VERIFY_DESCRIPTIONS.cannotAssess)

// A claim older than this is presumed abandoned and may be taken over.
export const CLAIM_STALE_AFTER_MS = 30 * 60 * 1000

// Classifies the latest `boss/verify` status on a head relative to this run's claim token.
// `kind` is `none` (no status), `mine`, `foreign` (a live claim by another run), `stale` (a
// foreign claim older than CLAIM_STALE_AFTER_MS, or with an unreadable timestamp), `waiting`, or
// `terminal` (anything else: verified, defect, needs human).
export function claimState(status, { token = '', now = Date.now() } = {}) {
  if (!status || typeof status !== 'object') return { kind: 'none' }
  const description = typeof status.description === 'string' ? status.description : ''
  if (status.state === 'pending' && description.startsWith(VERIFY_DESCRIPTIONS.verifying)) {
    const claimed = description.slice(VERIFY_DESCRIPTIONS.verifying.length).trim()
    if (typeof token === 'string' && token.trim() !== '' && claimed === token)
      return { kind: 'mine', token: claimed }
    const at = Date.parse(status.updatedAt ?? '')
    // An unreadable timestamp is treated as stale: a claim nobody can age would otherwise hold the
    // head forever, and a double run costs tokens, never a double merge.
    const stale = !Number.isFinite(at) || now - at > CLAIM_STALE_AFTER_MS
    return { kind: stale ? 'stale' : 'foreign', token: claimed }
  }
  if (isCannotAssess(status)) return { kind: 'waiting' }
  if (status.state === 'pending' && description.startsWith(VERIFY_DESCRIPTIONS.waiting))
    return { kind: 'waiting' }
  return { kind: 'terminal' }
}
