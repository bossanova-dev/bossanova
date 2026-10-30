import { isTrustedPlanAuthor } from './github-plan.mjs'

/**
 * Read a complete marker snapshot, refusing a scan truncated at its page bound.
 * @param {{markerPrefix?: string, updatedAfter?: string, maxPages?: number}} query
 * @param {{api: Function, identity: {owner: string, repo: string}}} context
 */
export async function githubSelectMarked(query = {}, { api, identity }) {
  if (
    !query ||
    typeof query !== 'object' ||
    Array.isArray(query) ||
    Object.keys(query).some((key) => !['markerPrefix', 'updatedAfter', 'maxPages'].includes(key))
  )
    throw new Error('github tracker selectMarked: unknown query keys')
  const { markerPrefix, updatedAfter, maxPages = 20 } = query
  if (typeof markerPrefix !== 'string' || !markerPrefix.trim())
    throw new Error('github tracker selectMarked: markerPrefix must be non-empty')
  if (!Number.isInteger(maxPages) || maxPages < 1)
    throw new Error('github tracker selectMarked: maxPages must be a positive integer')
  if (
    updatedAfter !== undefined &&
    updatedAfter !== null &&
    (typeof updatedAfter !== 'string' || !Number.isFinite(Date.parse(updatedAfter)))
  )
    throw new Error('github tracker selectMarked: updatedAfter is not a timestamp')
  /** @type {Array<{id: string, identifier: string, title: string, description: string, createdAt: string, resolution: 'done'|'canceled'|null, resolvedAt: string|null}>} */
  const result = []
  for (let page = 1; page <= maxPages; page++) {
    const rows = api(`repos/${identity.owner}/${identity.repo}/issues`, [
      '--method',
      'GET',
      '-f',
      'state=all',
      '-f',
      'per_page=100',
      '-f',
      `page=${page}`,
      ...(updatedAfter ? ['-f', `since=${updatedAfter}`] : []),
    ])
    if (!Array.isArray(rows)) throw new Error('github tracker: malformed marker page')
    for (const row of rows) {
      if (
        !row ||
        typeof row !== 'object' ||
        Array.isArray(row) ||
        !Number.isSafeInteger(row.number) ||
        row.number < 1 ||
        typeof row.title !== 'string' ||
        (row.body !== null && typeof row.body !== 'string') ||
        !['open', 'closed'].includes(row.state) ||
        typeof row.created_at !== 'string' ||
        (row.closed_at !== null && typeof row.closed_at !== 'string') ||
        typeof row.author_association !== 'string'
      )
        throw new Error('github tracker: malformed marker issue entry')
      if (
        Object.hasOwn(row, 'pull_request') ||
        !isTrustedPlanAuthor(row) ||
        !(row.body ?? '').includes(markerPrefix)
      )
        continue
      result.push({
        id: `GH-${row.number}`,
        identifier: `GH-${row.number}`,
        title: row.title,
        description: row.body ?? '',
        createdAt: row.created_at,
        resolution:
          row.state === 'open'
            ? null
            : ['not_planned', 'duplicate'].includes(row.state_reason)
              ? 'canceled'
              : 'done',
        resolvedAt: row.state === 'open' ? null : row.closed_at,
      })
    }
    if (rows.length < 100) return result
  }
  throw new Error('github tracker: marker snapshot incomplete')
}
