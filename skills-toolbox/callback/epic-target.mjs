// Pure callback-target selection for boss-epic. This remains dependency-free so
// the installed skill can choose a scoped callback target without host access.

/**
 * @typedef {Object} EpicCallbackCandidate
 * @property {string} chatId Chat that receives the callback wake-up.
 * @property {string} repo Repository identity recorded for that chat.
 */

/**
 * @typedef {Object} EpicCallbackTarget
 * @property {string} chatId Chat passed to `boss callback --chat`.
 * @property {string} repo Repository passed to `boss callback --repo`.
 */

/**
 * Choose the callback target for a child PR. A candidate is verified only when
 * it has a non-empty chat id and repository identity, and that repository is
 * exactly the child PR repository. The verified epic orchestrator takes
 * priority; the verified child session is the fallback.
 *
 * @param {{
 *   childPrRepo?: string,
 *   orchestrator?: EpicCallbackCandidate | null,
 *   child?: EpicCallbackCandidate | null,
 * }} input Child PR repository plus candidate session identities.
 * @returns {EpicCallbackTarget | null} The explicit callback scope, or null when unverified.
 */
export function selectEpicCallbackTarget({ childPrRepo, orchestrator, child } = {}) {
  if (!hasIdentity(childPrRepo)) return null

  for (const candidate of [orchestrator, child]) {
    if (isVerifiedCandidate(candidate, childPrRepo)) {
      return { chatId: candidate.chatId, repo: candidate.repo }
    }
  }

  return null
}

function isVerifiedCandidate(candidate, childPrRepo) {
  return (
    hasIdentity(candidate?.chatId) && hasIdentity(candidate?.repo) && candidate.repo === childPrRepo
  )
}

function hasIdentity(value) {
  return typeof value === 'string' && value.trim().length > 0
}
