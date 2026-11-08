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
import { labelName, loadSkillConfig, optionalLabelName } from '../skill-config.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'

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

/** @param {{overlaps: {number: number, url: string, shared: string[], level: string, planned: boolean}[]}} decision */
export function holdComment(decision) {
  const held = decision.overlaps
    .filter((pr) => pr.level === 'file' && !pr.planned)
    .sort((a, b) => a.number - b.number)
  return [
    'Build held: unplanned file overlap with open PRs. Record the intended sequencing in the plan or description before restoring the build label.',
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
    const needsHuman = optionalLabelName(config, 'needsHuman')
    if (!needsHuman) throw new Error('needsHuman label role is not configured')
    const adapter = deps.adapter === undefined ? resolveTrackerAdapter({ env }) : deps.adapter
    if (typeof adapter?.applyIssueWrites !== 'function')
      throw new Error('the tracker adapter has no applyIssueWrites capability')
    const numbers = decision.overlaps
      .filter((pr) => pr.level === 'file' && !pr.planned)
      .map((pr) => pr.number)
      .sort((a, b) => a - b)
    const writes = {
      issueId: String(issue.id ?? issue.identifier),
      removeLabels: [labelName(config, 'agentBuild')],
      addLabels: [needsHuman],
      comment: holdComment(decision),
      marker: `${HOLD_MARKER_PREFIX}: ${numbers.map((n) => `#${n}`).join(', ')};`,
    }
    const result = await adapter.applyIssueWrites(writes)
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
