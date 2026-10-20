// Pure callback-target selection for boss-epic. This remains dependency-free so
// the installed skill can choose a scoped callback target without host access.
//
// Repository identities arrive in two shapes: session candidates carry a
// canonical origin URL (`repo_origin_url`), while a child PR repository is the
// `owner/repo` slug `boss callback --repo` accepts. Both are normalized to a
// lowercased slug before comparison.
//
// Deliberate design decision: normalization *ignores the host* and keys only on
// the last two path segments, so two same-named repositories on different hosts
// compare equal. That is acceptable because boss callbacks are GitHub-only
// (`githubcallback`); comparing hosts would reintroduce the slug-vs-URL mismatch
// this normalization exists to fix.

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
 * it has a non-empty chat id and a repository identity that normalizes to the
 * same `owner/repo` slug as the child PR repository. The verified epic
 * orchestrator takes priority; the verified child session is the fallback.
 *
 * The returned `repo` is always the normalized slug, never a candidate's raw
 * origin URL, because it is handed straight to `boss callback --repo`.
 *
 * @param {{
 *   childPrRepo?: string,
 *   orchestrator?: EpicCallbackCandidate | null,
 *   child?: EpicCallbackCandidate | null,
 * }} input Child PR repository plus candidate session identities.
 * @returns {EpicCallbackTarget | null} The explicit callback scope, or null when unverified.
 */
export function selectEpicCallbackTarget({ childPrRepo, orchestrator, child } = {}) {
  const wantedRepo = normalizeRepositoryIdentity(childPrRepo)
  if (wantedRepo === null) return null

  for (const candidate of [orchestrator, child]) {
    if (isVerifiedCandidate(candidate, wantedRepo)) {
      return { chatId: candidate.chatId, repo: wantedRepo }
    }
  }

  return null
}

/**
 * Normalize a repository identity to a lowercased `owner/repo` slug.
 *
 * Accepts a bare slug, `http(s)://host/owner/repo`, `ssh://git@host/owner/repo`,
 * and the scp-like `git@host:owner/repo` form (which `new URL()` cannot parse),
 * each with an optional `.git` suffix and trailing slashes. The host is
 * deliberately ignored — see the module comment. Never throws for any input.
 *
 * @param {unknown} value Raw repository identity (slug, origin URL, or clone URL).
 * @returns {string | null} The `owner/repo` slug, or null when the input yields no slug.
 */
export function normalizeRepositoryIdentity(value) {
  if (typeof value !== 'string') return null

  const text = value.trim()
  if (text.length === 0) return null

  const scpLike = /^[^/@\s]+@[^/:\s]+:(.+)$/.exec(text)
  if (scpLike) return toOwnerRepoSlug(scpLike[1])

  const schemeIndex = text.indexOf('://')
  if (schemeIndex === -1) return toOwnerRepoSlug(text)

  const authorityAndPath = text.slice(schemeIndex + 3)
  const pathIndex = authorityAndPath.indexOf('/')
  if (pathIndex === -1) return null

  return toOwnerRepoSlug(authorityAndPath.slice(pathIndex + 1))
}

function toOwnerRepoSlug(path) {
  const trimmed = stripGitSuffix(path.trim())
  const segments = trimmed.split('/').filter((segment) => segment.length > 0)
  if (segments.length < 2) return null

  const [owner, repo] = segments
  if (/\s/.test(owner) || /\s/.test(repo)) return null

  return `${owner}/${repo}`.toLowerCase()
}

function stripGitSuffix(path) {
  const withoutSlashes = path.replace(/\/+$/, '')
  if (!withoutSlashes.toLowerCase().endsWith('.git')) return withoutSlashes

  return withoutSlashes.slice(0, -'.git'.length).replace(/\/+$/, '')
}

function isVerifiedCandidate(candidate, wantedRepo) {
  return (
    hasIdentity(candidate?.chatId) && normalizeRepositoryIdentity(candidate?.repo) === wantedRepo
  )
}

function hasIdentity(value) {
  return typeof value === 'string' && value.trim().length > 0
}
