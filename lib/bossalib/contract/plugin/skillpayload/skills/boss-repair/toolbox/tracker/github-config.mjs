// GitHub identity and workflow roles. Kept import-free for the config loader.
export const GITHUB_DEFAULT_STATES = Object.freeze({
  unplanned: 'boss:unplanned',
  planned: 'boss:todo',
  inProgress: 'boss:in-progress',
  inReview: 'boss:in-review',
  done: 'closed:completed',
  canceled: 'closed:not-planned',
  duplicate: 'closed:duplicate',
})
export const GITHUB_CLOSING_ROLES = Object.freeze({
  done: 'completed',
  canceled: 'not planned',
  duplicate: 'duplicate',
})

/** @param {string} value */
export function parseRepository(value) {
  if (
    typeof value !== 'string' ||
    !/^(?:[A-Za-z0-9.-]+\/)?[A-Za-z0-9-]+\/[A-Za-z0-9._-]+$/.test(value)
  ) {
    throw new Error('github tracker: repository must be [HOST/]OWNER/REPO')
  }
  const parts = value.split('/')
  const [host, owner, repo] = parts.length === 3 ? parts : ['github.com', ...parts]
  return { host, owner, repo, repository: value }
}

/** @param {Record<string,string>} [overrides] */
export function githubStates(overrides = {}) {
  const states = { ...GITHUB_DEFAULT_STATES, ...overrides }
  const names = Object.values(states)
  if (names.some((name) => typeof name !== 'string' || !name.trim()))
    throw new Error('github tracker: state names must be non-empty')
  if (new Set(names).size !== names.length) throw new Error('github tracker: duplicate state names')
  return states
}

export const GITHUB_DEFAULT_LABELS = Object.freeze({
  agentBuild: 'agent-build',
  needsHuman: 'needs-human',
  agentPlan: 'agent-plan',
  agentQuestion: 'agent-question',
  epic: 'epic',
})
export const GITHUB_PRIORITY_LABELS = Object.freeze(
  [1, 2, 3, 4].map((value) => `boss:priority-${value}`),
)
export const GITHUB_ESTIMATE_POINTS = Object.freeze([0, 1, 2, 3, 5, 8])
export const GITHUB_ESTIMATE_LABELS = Object.freeze(
  GITHUB_ESTIMATE_POINTS.map((value) => `boss:estimate-${value}`),
)

/** @param {{states?: Record<string,string>, labels?: Record<string,string>}} [block] */
export function githubRequiredLabels(block = {}) {
  const entries = [
    ...Object.entries(githubStates(block.states))
      .filter(([role]) => !Object.hasOwn(GITHUB_CLOSING_ROLES, role))
      .map(([role, name]) => ({ kind: 'state', role, name })),
    ...Object.entries({ ...GITHUB_DEFAULT_LABELS, ...block.labels }).map(([role, name]) => ({
      kind: 'pipeline',
      role,
      name,
    })),
    ...[...GITHUB_PRIORITY_LABELS, ...GITHUB_ESTIMATE_LABELS].map((name) => ({
      kind: 'ranking',
      role: name.slice('boss:'.length),
      name,
    })),
  ]
  const seen = new Set()
  return entries.filter(({ name }) => {
    const key = name.toLowerCase()
    if (seen.has(key)) return false
    seen.add(key)
    return true
  })
}
