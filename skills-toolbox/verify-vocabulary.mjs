export const VERIFY_CONTEXT = 'boss/verify'

// The description prefixes are a contract with the `boss/verify` status renderer and with candidate
// discovery: `verifying…` is a live claim, `waiting:` is an unclaimed head, `needs human:` is a
// policy park, and `verified` / `defect:` are terminal. Change them here and nowhere else.
export const VERIFY_DESCRIPTIONS = Object.freeze({
  verifying: 'verifying…',
  waiting: 'waiting:',
  needsHuman: 'needs human:',
  verified: 'verified',
  verifiedApproved: 'verified (approved)',
  defect: 'defect:',
  // Appended to a `defect:` description when the defect was also handed to a human (auto-repair
  // off or unreadable), so `approval` can tell a parked defect from one repair is working on.
  defectParkedSuffix: ', needs human',
})

// The `candidates` skip reason for a PR outside its `--pr-urls`: the PR's session lives on another
// machine, so a sweep run with `--local-sessions-only` reads nothing of it.
export const REMOTE_SESSION_REASON = 'remote-session'
