import { classifyReadCall } from './verify-gate.mjs'
import { pathParkFacts } from './verify-path-parks.mjs'

export const REPO = 'acme/widgets'
export const BASE = 'b'.repeat(40)
export const headOf = (pr) => String(pr).padStart(2, '0').repeat(20)
const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
const fail = () => ({ ok: false, exit: 1, output: '' })

const marker = (id, body) => ({
  id,
  user: { login: 'operator' },
  author_association: 'MEMBER',
  created_at: `2026-10-01T00:00:0${id}Z`,
  body,
})
// The verify judge already ran on this head and raised nothing (`W10` is `[]`).
export const judgeRecord = (pr) =>
  marker(1, `<!-- boss-judge-record head=${headOf(pr)} areas=W10 -->`)
export const extensionRecord = (pr, judged) =>
  marker(2, `<!-- boss-extension-record head=${headOf(pr)} judged=${judged} -->`)
export const parkStatus = (description) => ({
  context: 'boss/verify',
  state: 'pending',
  description,
  updated_at: '2026-10-01T00:00:09Z',
})

/**
 * A GitHub serving open PRs of acme/widgets: each `{number, paths, comments, statuses, state}`.
 * Every call it sees is logged; every call that would write is also kept in `writes`.
 */
export function forge({ prs, baseConfig = null, extensions = [] }) {
  const calls = []
  const writes = []
  const byNumber = new Map(prs.map((pr) => [pr.number, pr]))
  const read = (command, args = []) => {
    calls.push([command, args])
    if (command === 'boss') {
      if (!['ls', 'show'].includes(args[0])) writes.push([command, args])
      return fail()
    }
    if (command !== 'gh') return fail()
    if (classifyReadCall(command, args) !== 'cached') {
      writes.push([command, args])
      return ok({})
    }
    if (args[0] === 'pr') {
      const pr = byNumber.get(Number(args[2]))
      if (!pr) return fail()
      return ok({
        state: pr.state ?? 'OPEN',
        number: pr.number,
        url: `https://github.com/${REPO}/pull/${pr.number}`,
        title: `feat: widget ${pr.number}`,
        body: `Adds widget ${pr.number}.`,
        isDraft: false,
        additions: 10,
        deletions: 2,
        headRefOid: headOf(pr.number),
        baseRefName: 'main',
        mergeStateStatus: 'CLEAN',
        statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'SUCCESS' }],
      })
    }
    const path = args.find((arg) => /^repos\/|^graphql$/.test(arg)) ?? args[1]
    if (path === 'graphql') {
      if (args.some((arg) => String(arg).includes('pullRequests(states: OPEN')))
        return ok({
          data: {
            repository: {
              pullRequests: {
                pageInfo: { hasNextPage: false, endCursor: '' },
                nodes: prs
                  .filter((pr) => (pr.state ?? 'OPEN') === 'OPEN')
                  .map((pr) => ({ number: pr.number, headRefOid: headOf(pr.number) })),
              },
            },
          },
        })
      return ok({
        data: {
          repository: { pullRequest: { reviews: { nodes: [] }, timelineItems: { nodes: [] } } },
        },
      })
    }
    const number = Number(/\/(?:pulls|issues)\/(\d+)\//.exec(path)?.[1])
    if (/\/pulls\/\d+\/files/.test(path))
      return ok([byNumber.get(number).paths.map((filename) => ({ filename, status: 'added' }))])
    if (path.includes('/comments?')) return ok([byNumber.get(number).comments ?? []])
    if (path === `repos/${REPO}/commits/main`) return ok({ sha: BASE })
    if (path.endsWith('/permission')) return ok({ permission: 'admin' })
    if (path === `repos/${REPO}/contents/.boss-skills.json?ref=${BASE}`)
      return baseConfig
        ? ok({
            type: 'file',
            encoding: 'base64',
            content: Buffer.from(JSON.stringify(baseConfig)).toString('base64'),
          })
        : { ...fail(), payload: { status: '404' } }
    const atHead = /\/contents\/(.+)\?ref=([0-9a-f]{40})$/.exec(path)
    const content = prs.find((pr) => headOf(pr.number) === atHead?.[2])?.contents?.[atHead[1]]
    if (content !== undefined)
      return ok({
        type: 'file',
        encoding: 'base64',
        content: Buffer.from(content).toString('base64'),
      })
    if (path.includes('/contents/')) return { ...fail(), payload: { status: '404' } }
    if (path.includes('/statuses')) {
      const pr = prs.find((entry) => path.includes(headOf(entry.number)))
      return ok([
        [
          { context: 'boss/build', state: 'success', updated_at: '2026-10-01T00:00:00Z' },
          ...(pr?.statuses ?? []),
        ],
      ])
    }
    if (path.includes('actions/runs?'))
      return ok([{ workflow_runs: [{ name: 'CI', status: 'completed', conclusion: 'success' }] }])
    if (path.includes('/check-runs'))
      return ok([
        {
          total_count: 1,
          check_runs: [{ name: 'ci', status: 'completed', conclusion: 'success' }],
        },
      ])
    if (path.endsWith('/status')) return ok({ statuses: [] })
    return fail()
  }
  const deps = {
    read,
    now: () => Date.parse('2026-10-02T00:00:00Z'),
    discover: () => ({ extensions, skipped: [] }),
    trustedTree: () => ({ ok: true, root: '/trusted' }),
    treeAreaFacts: pathParkFacts,
    loadConfig: () => ({}),
    exists: () => false,
  }
  return { deps, calls, writes }
}
