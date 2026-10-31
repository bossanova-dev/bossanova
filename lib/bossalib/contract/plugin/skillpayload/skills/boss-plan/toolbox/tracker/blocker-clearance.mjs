// Import-free blocker policy shared by tracker and orchestration helpers.
export const BLOCKER_CLEARED_STATE_TYPES = new Set(['completed', 'canceled'])

export function prUrlsOf(blocker) {
  const nodes = blocker?.attachments?.nodes ?? []
  return [
    ...new Set(
      nodes.flatMap((node) => {
        const url = node?.url
        const match =
          typeof url === 'string' &&
          url.match(/^https?:\/\/github\.com\/[^/\s]+\/[^/\s]+\/pull\/\d+(?=[/?#]|$)/i)
        return match ? [match[0]] : []
      }),
    ),
  ]
}

/** Extract path tokens from a caller-selected Key changes section; this leaf stays import-free. */
export function dependentPathsFrom(section) {
  if (typeof section !== 'string') return []
  return [
    ...new Set(
      [...section.matchAll(/`([^`]+)`/g)].flatMap(([, token]) => {
        const path = token.replace(/^(\.\/)+/, '')
        if (
          /\s/.test(path) ||
          path.startsWith('/') ||
          path.includes(':') ||
          path.split('/').includes('..') ||
          path.startsWith('-')
        )
          return []
        return [path]
      }),
    ),
  ]
}

function sameState(a, b) {
  const normalize = (name) =>
    typeof name === 'string' ? name.toLowerCase().replace(/[\s_-]/g, '') : ''
  return Boolean(normalize(a)) && normalize(a) === normalize(b)
}

function overlaps(dependentPaths, files) {
  return dependentPaths.some((raw) => {
    const path = raw.replace(/^(\.\/)+/, '')
    const directory = path.endsWith('/') || !path.split('/').at(-1).includes('.')
    return files.some((rawFile) => {
      const file = rawFile.replace(/^(\.\/)+/, '')
      return directory ? file.startsWith(path.replace(/\/$/, '') + '/') : file === path
    })
  })
}

/** @param {object} blocker
 * @param {{prFacts?: Map<string, {state?: string, mergedAt?: string|null, error?: string, isDraft?: boolean, green?: boolean, files?: string[]|null}>, clearedStateTypes?: Set<string>, mode?: string, inReviewState?: string, dependentPaths?: string[]}} [options] */
export function classifyBlocker(
  blocker,
  {
    prFacts,
    clearedStateTypes = BLOCKER_CLEARED_STATE_TYPES,
    mode = 'strict',
    inReviewState,
    dependentPaths = [],
  } = {},
) {
  if (clearedStateTypes.has(blocker?.state?.type)) return { clearance: 'cleared', reason: 'state' }
  const urls = prUrlsOf(blocker)
  if (!urls.length) return { clearance: 'uncleared', reason: 'no-pr' }
  if (prFacts === undefined) return { clearance: 'needs-pr-facts', reason: 'pr-facts-needed' }
  const facts = urls.map((url) => prFacts.get(url))
  if (facts.some((fact) => fact && !fact.error && (fact.state === 'MERGED' || fact.mergedAt))) {
    return { clearance: 'cleared', reason: 'merged-pr' }
  }
  if (facts.some((fact) => !fact || fact.error || !['OPEN', 'CLOSED'].includes(fact.state))) {
    return { clearance: 'uncleared', reason: 'pr-unknown' }
  }
  const open = facts.filter((fact) => fact.state === 'OPEN')
  if (
    ['selection', 'gate'].includes(mode) &&
    sameState(blocker?.state?.name, inReviewState) &&
    open.length > 0 &&
    open.every((fact) => fact.isDraft === false && fact.green === true) &&
    (mode === 'gate' ||
      (dependentPaths.length > 0 &&
        open.every(
          (fact) =>
            Array.isArray(fact.files) &&
            fact.files.every((path) => typeof path === 'string' && path.length > 0) &&
            !overlaps(dependentPaths, fact.files),
        )))
  ) {
    return { clearance: 'review-clearable', reason: 'green-disjoint-pr' }
  }
  return { clearance: 'uncleared', reason: 'unmerged-pr' }
}

export function blockerVerdict(blockers, opts) {
  const outcomes = blockers.map((blocker) => classifyBlocker(blocker, opts).clearance)
  if (outcomes.includes('uncleared')) return 'blocked'
  return outcomes.includes('needs-pr-facts') ? 'unknown' : 'unblocked'
}

export function softClearedBlockers(blockers, opts) {
  return blockers.flatMap((blocker) =>
    classifyBlocker(blocker, opts).clearance === 'review-clearable'
      ? prUrlsOf(blocker)
          .filter((url) => opts.prFacts.get(url)?.state === 'OPEN')
          .map((pr) => ({ id: blocker.id, pr }))
      : [],
  )
}
