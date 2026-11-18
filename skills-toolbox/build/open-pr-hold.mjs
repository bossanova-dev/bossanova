#!/usr/bin/env node
import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { extractKeyChangeAreas, withScanDefaults } from '../plan/plan-deps-lib.mjs'
import {
  fetchOpenPullRequests,
  listRepoRootEntries,
  overlappingPullRequests,
  readOpenPrsSnapshot,
  validateOpenPrsSnapshot,
} from '../plan/plan-open-prs.mjs'
import { labelName, loadSkillConfig, stateName } from '../skill-config.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { BLOCKER_CLEARED_STATE_TYPES } from '../tracker/blocker-clearance.mjs'

export const HOLD_MARKER_PREFIX = 'boss-build open-PR overlap hold'

/** @param {string} text */
const currentText = (text) => text.split(/^##\s+Original notes\s*\r?$/im)[0]
/** @param {string} text */
const escapeRegex = (text) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

/** Pure decision, using the planning classifier and its scan defaults.
 * @param {{config: object, issue: {identifier?: string, id?: string, description?: string}, planText: string,
 * snapshot: import('../plan/plan-open-prs.mjs').OpenPrsSnapshot, ownPr?: number,
 * rootEntries: {dirs: string[], files: string[], knownFiles?: string[]}}} input */
export function decideOpenPrHold({ config, issue, planText, snapshot, ownPr, rootEntries }) {
  const defect = validateOpenPrsSnapshot(snapshot)
  if (defect) return { route: 'not-evaluated', overlaps: [], warnings: [], reason: defect }
  const defaults = withScanDefaults(config, {})
  const options = {
    ...defaults,
    moduleRoots: [...new Set([...defaults.moduleRoots, ...rootEntries.dirs])],
    rootFiles: rootEntries.files,
    knownFiles: rootEntries.knownFiles ?? rootEntries.files,
  }
  const areas = [
    ...new Set(
      [issue.description ?? '', planText].flatMap((text) => {
        const extracted = extractKeyChangeAreas(config, text, options)
        return extracted.source === 'key-changes' ? extracted.areas : []
      }),
    ),
  ]
  if (!areas.length)
    return { route: 'not-evaluated', overlaps: [], warnings: [], reason: 'no-key-changes' }
  const subjectIdentifier = String(issue.identifier ?? issue.id ?? '')
  const recorded = [planText, issue.description ?? ''].map(currentText).join('\n')
  const recordedUrls = new Set(
    (recorded.match(/https?:\/\/[^\s<>()`"']+/g) ?? []).map((url) => url.replace(/[.,;!]+$/, '')),
  )
  const overlaps = overlappingPullRequests({
    subjectIdentifier,
    subjectAreas: areas,
    snapshot,
    overlapOptions: options,
  })
    .filter((pr) => pr.number !== ownPr)
    .map((pr) => ({
      ...pr,
      planned:
        new RegExp(`(^|[^\\w#])#${pr.number}(?![\\w])`).test(recorded) ||
        recordedUrls.has(pr.url) ||
        pr.ticketIds.some(
          (id) =>
            id !== subjectIdentifier.toUpperCase() &&
            new RegExp(`\\b${escapeRegex(id)}\\b`, 'i').test(recorded),
        ),
    }))
  const warnings = [
    ...(snapshot.listTruncated ? ['open PR list truncated; unseen PRs could overlap'] : []),
    ...snapshot.pullRequests
      .filter((pr) => pr.filesTruncated)
      .map((pr) => `PR #${pr.number} files truncated; unseen files could overlap`),
  ]
  return {
    route: overlaps.some((pr) => pr.level === 'file' && !pr.planned) ? 'hold' : 'clear',
    overlaps,
    warnings,
  }
}

/** The open PRs that hold a build: unplanned file-level overlaps, by PR number.
 * @param {{overlaps: {number: number, url: string, shared: string[], level: string, planned: boolean, ticketIds?: string[]}[]}} decision */
export const heldPullRequests = (decision) =>
  decision.overlaps
    .filter((pr) => pr.level === 'file' && !pr.planned)
    .sort((a, b) => a.number - b.number)

/** Pure split of the held PRs. A PR whose ticket is still open becomes a blocker of this issue, so
 * the build waits for that ticket. A PR with no open ticket sends this issue back to planning, and
 * so does one whose open tickets already block this issue: selection let the issue through despite
 * those edges (an In Review soft-clear or a named run), so they cannot keep it out of the build queue.
 * @param {ReturnType<typeof decideOpenPrHold>} decision
 * @param {Map<string, {type?: string}|null>} ticketStates state per PR ticket id; null when unreadable
 * @param {Set<string>} [existingBlockers] upper-cased ids already blocking this issue */
export function splitHeldPullRequests(decision, ticketStates, existingBlockers = new Set()) {
  const blockers = new Set()
  const untracked = []
  for (const pr of heldPullRequests(decision)) {
    const open = (pr.ticketIds ?? []).filter((id) => {
      const state = ticketStates.get(id)
      return state && !BLOCKER_CLEARED_STATE_TYPES.has(state.type)
    })
    if (open.some((id) => !existingBlockers.has(id.toUpperCase())))
      open.forEach((id) => blockers.add(id))
    else untracked.push(pr)
  }
  return { blockers: [...blockers].sort(), untracked }
}

/** @param {ReturnType<typeof decideOpenPrHold>} decision
 * @param {{blockers: string[], untracked: {number: number}[]}} [split] */
export function holdComment(decision, split = { blockers: [], untracked: [] }) {
  const held = heldPullRequests(decision)
  const untracked = split.untracked.map((pr) => `#${pr.number}`).join(', ')
  return [
    'Build held: open PRs change the same files as this plan.',
    ...(split.blockers.length
      ? [`Blocked by ${split.blockers.join(', ')}; the build resumes when they are done.`]
      : []),
    ...(untracked
      ? [
          `${untracked} has no open ticket, so this goes back to planning; re-plan it once that PR merges.`,
        ]
      : []),
    ...held
      .slice(0, 5)
      .map(
        (pr) =>
          `- #${pr.number} ${pr.url}: ${pr.shared.slice(0, 10).join(', ')}${pr.shared.length > 10 ? ` (+${pr.shared.length - 10} more files)` : ''}`,
      ),
    ...(held.length > 5 ? [`+${held.length - 5} more PRs`] : []),
  ].join('\n')
}

/** @param {string[]} argv
 * @param {{config?: object, adapter?: object|null, fetch?: typeof fetchOpenPullRequests, env?: Record<string, string|undefined>, cwd?: string}} [deps] */
export async function runCli(argv, deps = {}) {
  let values
  try {
    const args = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        plan: { type: 'string' },
        'issue-json': { type: 'string' },
        pr: { type: 'string' },
        'open-prs': { type: 'string' },
        apply: { type: 'boolean' },
      },
    })
    values = args.values
    const command = args.positionals[0]
    if (
      args.positionals.length !== 1 ||
      command !== 'check' ||
      !values.plan ||
      !values['issue-json'] ||
      (values.pr !== undefined && !/^[1-9]\d*$/.test(values.pr))
    )
      throw new Error(
        'usage: open-pr-hold.mjs check --plan <file> --issue-json <file> [--pr <n>] [--open-prs <file>] [--apply]',
      )
  } catch (error) {
    return { code: 64, out: { reason: error.message } }
  }
  const env = deps.env ?? process.env
  let config, issue, decision
  try {
    config = deps.config ?? loadSkillConfig({ cwd: deps.cwd ?? process.cwd() })
    issue = JSON.parse(readFileSync(values['issue-json'], 'utf8'))
    if (!issue || typeof issue !== 'object' || Array.isArray(issue))
      throw new Error('issue must be an object')
    const planText = readFileSync(values.plan, 'utf8')
    const snapshot = values['open-prs']
      ? readOpenPrsSnapshot(values['open-prs'])
      : await (deps.fetch ?? fetchOpenPullRequests)({ env })
    decision = decideOpenPrHold({
      config,
      issue,
      planText,
      snapshot,
      ownPr: values.pr ? Number(values.pr) : undefined,
      rootEntries: listRepoRootEntries(deps.cwd),
    })
  } catch (error) {
    return {
      code: 69,
      out: { route: 'not-evaluated', overlaps: [], warnings: [], reason: error.message },
    }
  }
  if (decision.route === 'not-evaluated') return { code: 69, out: decision }
  if (decision.route === 'clear') return { code: 0, out: decision }
  if (!values.apply) return { code: 3, out: decision }
  try {
    const adapter = deps.adapter === undefined ? resolveTrackerAdapter({ env }) : deps.adapter
    if (typeof adapter?.applyIssueWrites !== 'function')
      throw new Error('the tracker adapter has no applyIssueWrites capability')
    const held = heldPullRequests(decision)
    const ticketStates = new Map()
    for (const id of new Set(held.flatMap((pr) => pr.ticketIds ?? []))) {
      // Without a readable open ticket a blocker could never clear, so that PR counts as untracked.
      if (typeof adapter.readIssueState !== 'function') break
      ticketStates.set(
        id,
        await adapter.readIssueState(id).then(
          (r) => r?.state ?? null,
          () => null,
        ),
      )
    }
    const issueId = String(issue.id ?? issue.identifier)
    // Unreadable edges count as none, so the hold still writes the blocker edge.
    const existingBlockers = new Set()
    if (typeof adapter.readBlockers === 'function' && ticketStates.size)
      try {
        const [row] = await adapter.readBlockers([issueId])
        for (const blocker of row?.blockers ?? [])
          if (blocker?.id) existingBlockers.add(String(blocker.id).toUpperCase())
      } catch {}
    const split = splitHeldPullRequests(decision, ticketStates, existingBlockers)
    const replan = split.untracked.length > 0
    const buildLabel = replan ? labelName(config, 'agentBuild') : null
    const writes = {
      issueId,
      ...(split.blockers.length ? { addBlockedBy: split.blockers } : {}),
      ...(replan
        ? {
            addLabels: [labelName(config, 'agentPlan')],
            // Leaving In Progress also makes Step 12 leave the state rather than restore Todo.
            stateName: stateName(config, 'unplanned'),
          }
        : {}),
      comment: holdComment(decision, split),
      marker: `${HOLD_MARKER_PREFIX}: ${held.map((pr) => `#${pr.number}`).join(', ')};`,
    }
    // Trackers apply each write in turn and stop at the first failure. Removing the build label
    // only after the state move lands keeps a partial hold in one queue: it is still a build
    // candidate when Step 12 restores Todo, and a planning candidate once it is unplanned.
    let result = await adapter.applyIssueWrites(writes)
    if (replan && result?.outcome === 'ok')
      result = await adapter.applyIssueWrites({ issueId, removeLabels: [buildLabel] })
    const outcome = result?.outcome ?? 'failed'
    return {
      code: outcome === 'ok' ? 3 : 4,
      out: {
        ...decision,
        writes,
        outcome,
        ...(outcome === 'ok' ? {} : { reason: result?.reason ?? `hold writes ${outcome}` }),
      },
    }
  } catch (error) {
    return { code: 4, out: { ...decision, reason: error.message } }
  }
}

if (isMainModule(import.meta.url)) {
  const { code, out } = await runCli(process.argv.slice(2))
  process.stdout.write(`${JSON.stringify(out)}\n`)
  process.exitCode = code
}
