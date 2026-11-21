#!/usr/bin/env node
import { parseArgs } from 'node:util'
import { linearRequest } from '../../skills-toolbox/linear-gate-lib.mjs'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { labelName, loadSkillConfig } from '../../skills-toolbox/skill-config.mjs'
import { linearApplyIssueWrites } from '../../skills-toolbox/tracker/linear.mjs'

const LEGACY_OVERLAP_MARKER = 'boss-build open-PR overlap hold:'
const RELEASE_MARKER = 'boss-build overlap hold released'

/** @typedef {{body: string, createdAt: string}} HoldComment */
/** @typedef {{createdAt: string, addedLabelIds?: string[] | null, removedLabelIds?: string[] | null}} LabelHistory */
/** @typedef {{id: string, identifier: string, labels: {nodes: {id: string, name: string}[]},
 * comments: {nodes: HoldComment[]}, history: {nodes: LabelHistory[], pageInfo: {hasNextPage: boolean}}}} HeldIssue */
/** @param {HeldIssue} issue @param {{needsHuman: string, build: string}} labels */
export function decideHoldRelease(issue, { needsHuman, build }) {
  const skip = (/** @type {string} */ reason) => ({ action: 'skip', reason })
  const holds = issue.comments.nodes.filter((comment) =>
    comment.body.includes(LEGACY_OVERLAP_MARKER),
  )
  if (!holds.length) return skip('no-hold-marker')
  if (!issue.labels.nodes.some((label) => label.name === needsHuman)) return skip('not-held')
  if (issue.history.pageInfo.hasNextPage) return skip('history-truncated')
  const heldAt = Math.max(...holds.map((comment) => Date.parse(comment.createdAt)))
  if (!Number.isFinite(heldAt)) return skip('history-truncated')
  for (const entry of issue.history.nodes) {
    if (!entry.addedLabelIds?.length && !entry.removedLabelIds?.length) continue
    const changedAt = Date.parse(entry.createdAt)
    if (!Number.isFinite(changedAt)) return skip('history-truncated')
    if (changedAt > heldAt) return skip('labels-changed-after-hold')
  }
  return { action: 'release', addLabels: [build], removeLabels: [needsHuman] }
}

const ISSUES_QUERY = `query ReleaseHeldIssues($filter: IssueFilter!, $after: String) {
  issues(first: 100, after: $after, filter: $filter) {
    nodes { id identifier labels(first: 100) { nodes { id name } pageInfo { hasNextPage } } }
    pageInfo { hasNextPage endCursor }
  }
}`
const DETAILS_QUERY = `query ReleaseHoldDetails($id: String!, $after: String) {
  issue(id: $id) {
    id identifier labels(first: 100) { nodes { id name } pageInfo { hasNextPage } }
    comments(first: 100, after: $after) { nodes { body createdAt } pageInfo { hasNextPage endCursor } }
    history(first: 250) { nodes { createdAt addedLabelIds removedLabelIds } pageInfo { hasNextPage } }
  }
}`

/** @typedef {{apiKey: string, fetchImpl?: typeof fetch}} Transport */
/** @param {Transport} transport @param {string} id @returns {Promise<HeldIssue>} */
async function readHoldDetails(transport, id) {
  /** @type {HoldComment[]} */
  const comments = []
  let after = null
  const seen = new Set()
  while (true) {
    const { issue } = await linearRequest({
      ...transport,
      query: DETAILS_QUERY,
      variables: { id, after },
    })
    if (
      !Array.isArray(issue?.comments?.nodes) ||
      !Array.isArray(issue?.history?.nodes) ||
      !Array.isArray(issue?.labels?.nodes) ||
      typeof issue.history.pageInfo?.hasNextPage !== 'boolean' ||
      typeof issue.comments.pageInfo?.hasNextPage !== 'boolean' ||
      issue.labels.pageInfo?.hasNextPage
    )
      throw new Error(`Unreadable hold evidence for ${id}`)
    comments.push(...issue.comments.nodes)
    if (!issue.comments.pageInfo.hasNextPage) return { ...issue, comments: { nodes: comments } }
    after = issue.comments.pageInfo.endCursor
    if (!after || seen.has(after)) throw new Error(`Unreadable comments cursor for ${id}`)
    seen.add(after)
  }
}

/** @param {string[]} argv
 * @param {{env?: Record<string, string|undefined>, fetchImpl?: typeof fetch, config?: object}} [deps] */
export async function runCli(argv, deps = {}) {
  let values
  try {
    const parsed = parseArgs({
      args: argv,
      strict: true,
      options: {
        team: { type: 'string', multiple: true },
        'needs-human-label': { type: 'string' },
        'build-label': { type: 'string' },
        apply: { type: 'boolean', default: false },
      },
    })
    values = parsed.values
    if (!values.team?.length || values.team.some((team) => !team.trim()))
      throw new Error(
        'usage: release-open-pr-holds.mjs --team <key> [--team <key>…] [--needs-human-label <name>] [--build-label <name>] [--apply]',
      )
    for (const name of [values['needs-human-label'], values['build-label']])
      if (name !== undefined && !name.trim()) throw new Error('Label names must not be empty')
  } catch (error) {
    return { code: 64, out: { reason: error.message } }
  }
  try {
    const config = deps.config ?? loadSkillConfig()
    const labels = {
      needsHuman: values['needs-human-label'] ?? labelName(config, 'needsHuman'),
      build: values['build-label'] ?? labelName(config, 'agentBuild'),
    }
    if (labels.needsHuman === labels.build) throw new Error('The held and build labels must differ')
    const apiKey = (deps.env ?? process.env).LINEAR_API_KEY
    if (!apiKey) throw new Error('LINEAR_API_KEY is not set')
    const transport = { apiKey, fetchImpl: deps.fetchImpl }
    /** @type {HeldIssue[]} */
    const issues = []
    const seenIssues = new Set()
    for (const team of new Set(values.team)) {
      let after = null
      const seenCursors = new Set()
      do {
        const data = await linearRequest({
          ...transport,
          query: ISSUES_QUERY,
          variables: {
            filter: {
              team: { key: { eq: team } },
              labels: { name: { eq: labels.needsHuman } },
              state: { type: { nin: ['completed', 'canceled'] } },
            },
            after,
          },
        })
        const page = data.issues
        if (!Array.isArray(page?.nodes) || typeof page.pageInfo?.hasNextPage !== 'boolean')
          throw new Error(`Unreadable issue page for ${team}`)
        for (const issue of page.nodes)
          if (!seenIssues.has(issue.id)) {
            issues.push(await readHoldDetails(transport, issue.id))
            seenIssues.add(issue.id)
          }
        if (!page.pageInfo.hasNextPage) break
        after = page.pageInfo.endCursor
        if (!after || seenCursors.has(after))
          throw new Error(`Unreadable issues cursor for ${team}`)
        seenCursors.add(after)
      } while (true)
    }
    let failed = false
    const decisions = []
    for (const issue of issues) {
      const decision = decideHoldRelease(issue, labels)
      /** @type {{outcome?: string}} */
      let result = {}
      if (values.apply && decision.action === 'release') {
        result = await linearApplyIssueWrites({
          ...transport,
          issueId: issue.id,
          addLabels: decision.addLabels,
          removeLabels: decision.removeLabels,
          comment: `Boss-build no longer holds builds on open-PR overlaps. Restored the build label.\n\n${RELEASE_MARKER}`,
          marker: RELEASE_MARKER,
        })
        if (result.outcome !== 'ok') failed = true
      }
      decisions.push({ id: issue.id, identifier: issue.identifier, ...decision, ...result })
    }
    return { code: failed ? 1 : 0, out: { apply: values.apply, decisions } }
  } catch (error) {
    return { code: 1, out: { reason: error.message } }
  }
}

if (isMainModule(import.meta.url)) {
  const { code, out } = await runCli(process.argv.slice(2))
  process.stdout.write(`${JSON.stringify(out)}\n`)
  process.exitCode = code
}
