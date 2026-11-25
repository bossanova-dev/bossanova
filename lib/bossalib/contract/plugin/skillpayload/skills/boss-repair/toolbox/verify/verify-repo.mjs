// Resolve `owner/name` from the checkout's origin remote before falling back to GitHub's API.
import { readAcrossQuotas, recordDegraded, repoNameRest } from '../ci/gh-quota-alternates.mjs'

/**
 * @typedef {{ok?: boolean, output?: unknown, payload?: unknown, stderr?: unknown, exit?: number | null, error?: unknown}} ReadResult
 * @typedef {(command: string, args: string[], options?: {json?: boolean, raw?: boolean, input?: string, env?: Record<string, string>}) => ReadResult} CommandReader
 * @typedef {{read: string, transport: string, reason: string}} DegradedRead
 */

const URL_REMOTE =
  /^(?:https|ssh):\/\/(?:[^\s/@]+@)?(?:[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?|\[[0-9a-fA-F:]+\])(?::\d+)?\/([^\s?#]+)$/
const SCP_REMOTE =
  /^[^\s/@:]+@(?:[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?|\[[0-9a-fA-F:]+\]):([^\s?#]+)$/
// Matches validateProof's `owner/name` rule while preserving case.
const REPO_SLUG = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/

/** @param {unknown} value @returns {value is string} */
const text = (value) => typeof value === 'string' && value.trim() !== ''

/**
 * Parse a case-preserved `owner/name` slug from an SSH, `ssh://`, or `https://` origin URL.
 * @param {string} raw
 * @returns {string}
 */
function parseOriginUrl(raw) {
  const trimmed = raw.trim()
  if (!trimmed) return ''
  const match = URL_REMOTE.exec(trimmed) ?? SCP_REMOTE.exec(trimmed)
  if (!match) return ''
  const stripped = match[1].replace(/\/$/, '').replace(/\.git$/, '')
  if (!REPO_SLUG.test(stripped)) return ''
  const [owner, name] = stripped.split('/')
  if (owner === '.' || owner === '..' || name === '.' || name === '..') return ''
  return stripped
}

/**
 * Read `owner/name` from the checkout's `origin` git remote without hitting the network.
 * Returns `''` when the remote is unreadable or not a valid SSH/HTTPS repository URL.
 * @param {CommandReader} read
 * @returns {string}
 */
export function originRepo(read) {
  let result
  try {
    result = read('git', ['remote', 'get-url', 'origin'])
  } catch {
    return ''
  }
  if (!result?.ok || typeof result.output !== 'string') return ''
  return parseOriginUrl(result.output)
}

/**
 * Resolve `owner/name` from an explicit value, the checkout's `origin` remote, or GitHub's API
 * (GraphQL `gh repo view`, falling back to REST on rate-limit refusal).
 * @param {CommandReader} read
 * @param {string} [repo]
 * @param {DegradedRead[]} [degraded]
 * @returns {string}
 */
export function repoFor(read, repo, degraded = []) {
  if (text(repo)) return repo
  const fromOrigin = originRepo(read)
  if (fromOrigin) return fromOrigin
  const view = readAcrossQuotas(
    read,
    {
      call: ['gh', ['repo', 'view', '--json', 'nameWithOwner'], { json: true }],
      normalize: (/** @type {{nameWithOwner?: unknown} | null | undefined} */ payload) =>
        typeof payload?.nameWithOwner === 'string' ? payload.nameWithOwner : '',
    },
    repoNameRest(),
  )
  recordDegraded(view, 'repo-name', degraded)
  return view.ok && text(view.value) ? view.value : ''
}
