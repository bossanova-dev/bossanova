#!/usr/bin/env node
// Would verify admit a ticket the build stage was NAMED to build? Verify only admits tickets that
// carry the build label and pass its own label selection (`verifyTicketRules`); the build stage
// also treats the needs-human label as "not ours to build". A named start outside those rules
// refuses unless it is approved, and an approved start swaps the labels after the claim is won.
//
//   node named-admission.mjs check <issue-json-file> [--approve]
//   node named-admission.mjs apply <issue-json-file>
//
// Prints one JSON line. `check`: exit 0 = `admissible`, or `approve` (with --approve) carrying the
// label deltas; exit 3 = `refuse`. `apply`: re-runs `check --approve` and applies an `approve`
// route's writes through the tracker adapter; exit 0 only when they landed (or nothing was due).
// Exit 64 = invalid arguments, 69 = an unreadable issue or config, or writes that did not land.
// Every non-zero exit carries `reason`.

import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'

import { isMainModule } from '../main-module.mjs'
import { issueLabels, matchIssue } from '../selection.mjs'
import {
  loadSkillConfig,
  optionalLabelName,
  planDescriptionSections,
  planSections,
  verifyTicketRules,
} from '../skill-config.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'

/** The fixed marker that keeps the approval comment from being posted twice. */
export const APPROVAL_MARKER = 'boss-build named-ticket approval'

const USAGE =
  'usage: named-admission.mjs check <issue-json-file> [--approve] | apply <issue-json-file>'
const LIST_ITEM = /^ {0,3}(?:[-*+]|\d+[.)])\s+(.*)$/

/**
 * The top-level list items of the plan's needs-human section, else of its open-questions section,
 * each cut to its first line. `[]` when neither section is present.
 * @param {object} config
 * @param {unknown} description
 * @returns {string[]}
 */
export function humanReasons(config, description) {
  if (typeof description !== 'string' || description === '') return []
  const sections = planDescriptionSections(config, description)
  for (const role of ['needs-human', 'open-questions']) {
    const heading = planSections(config).find((section) => section.required === role)?.heading
    const section = heading ? sections.find((entry) => entry.heading === heading) : undefined
    if (!section) continue
    const items = section.bodyLines
      .map((line) => line.replace(/\r$/, '').match(LIST_ITEM)?.[1]?.trim())
      .filter((item) => typeof item === 'string' && item !== '')
    if (items.length > 0) return items
  }
  return []
}

const quote = (label) => JSON.stringify(label)

/**
 * Decide whether verify would admit the named ticket, and with `approve` which label writes would
 * make it admissible. Throws when the issue carries no readable labels.
 * @param {{config: object, issue: object, approve?: boolean}} input
 * @returns {object} the route object `check` prints
 */
export function decideAdmission({ config, issue, approve = false }) {
  const labels = issue && typeof issue === 'object' ? issueLabels(issue) : undefined
  if (labels === undefined) throw new Error('issue: labels are absent or unreadable')
  const id = String(issue.identifier ?? issue.id ?? 'ticket')
  const rules = verifyTicketRules(config)
  const selection = { labels: rules.selection.labels }
  const needsHuman = optionalLabelName(config, 'needsHuman')
  const missing = rules.requireLabels.filter((label) => !labels.includes(label))
  const present = needsHuman && labels.includes(needsHuman) ? [needsHuman] : []
  if (missing.length === 0 && present.length === 0 && matchIssue(issue, selection, rules).matches)
    return { route: 'admissible' }

  const reasons = humanReasons(config, issue.description)
  const after = [...labels.filter((label) => !present.includes(label)), ...missing]
  const fixed = matchIssue({ ...issue, labels: after }, selection, rules)
  if (!fixed.matches) {
    return {
      route: 'refuse',
      reason:
        `${id}: verify's label selection would not admit it (${fixed.reason}); ` +
        `${quote(rules.requireLabels[0])} alone cannot fix that, so it is not built`,
      missing,
      present,
      humanReasons: reasons,
    }
  }
  if (approve) {
    return {
      route: 'approve',
      writes: {
        issueId: String(issue.id ?? issue.identifier),
        addLabels: missing,
        removeLabels: present,
      },
      humanReasons: reasons,
    }
  }
  const facts = [
    ...missing.map((label) => `missing ${quote(label)}`),
    ...present.map((label) => `labelled ${quote(label)}`),
  ]
  const planner = reasons.length > 0 ? `; planner: ${quote(reasons[0])}` : ''
  const relabel =
    `label it ${quote(rules.requireLabels[0])}` +
    (present.length > 0 ? ` and remove ${present.map(quote).join(', ')}` : '')
  return {
    route: 'refuse',
    reason: `${id}: not admissible to build (${facts.join(', ')})${planner}; ${relabel}, or re-run with --approve`,
    missing,
    present,
    humanReasons: reasons,
  }
}

/**
 * The approval comment: who started the build and why, the planner's reasons and the label change.
 * @param {{writes: {addLabels: string[], removeLabels: string[]}, humanReasons: string[], sessionId?: string}} input
 */
export function approvalComment({ writes, humanReasons: reasons, sessionId }) {
  const changes = [
    ...writes.addLabels.map((label) => `added \`${label}\``),
    ...writes.removeLabels.map((label) => `removed \`${label}\``),
  ]
  return [
    'This build was started by explicit request, and the start was taken as approval to build it.',
    ...(reasons.length > 0
      ? ['', 'The plan asked for a person because:', ...reasons.map((r) => `- ${r}`)]
      : []),
    '',
    `Labels: ${changes.join(', ')}.`,
    ...(sessionId ? ['', `Session: ${sessionId}`] : []),
  ].join('\n')
}

/**
 * @param {string} file
 * @returns {object}
 */
function readIssue(file) {
  try {
    return JSON.parse(readFileSync(file, 'utf8'))
  } catch (err) {
    throw new Error(`issue json ${file}: ${err.message}`)
  }
}

/**
 * @param {string[]} argv the CLI's own argv
 * @param {{config?: object, adapter?: object|null, env?: Record<string, string|undefined>, cwd?: string}} [deps]
 * @returns {Promise<{code: number, out: object}>}
 */
export async function runCli(argv, deps = {}) {
  let command, file, approve
  try {
    const { values, positionals } = parseArgs({
      args: argv,
      options: { approve: { type: 'boolean', default: false } },
      allowPositionals: true,
      strict: true,
    })
    ;[command, file] = positionals
    approve = values.approve === true
    if (positionals.length !== 2 || (command !== 'check' && command !== 'apply'))
      throw new Error(USAGE)
  } catch (err) {
    return { code: 64, out: { reason: err.message === USAGE ? USAGE : `${err.message}; ${USAGE}` } }
  }
  const env = deps.env ?? process.env
  let decision
  try {
    const config = deps.config ?? loadSkillConfig({ cwd: deps.cwd ?? process.cwd() })
    decision = decideAdmission({
      config,
      issue: readIssue(file),
      approve: approve || command === 'apply',
    })
  } catch (err) {
    return { code: 69, out: { reason: err.message } }
  }
  if (decision.route === 'refuse') return { code: 3, out: decision }
  if (command === 'check' || decision.route === 'admissible') return { code: 0, out: decision }

  let adapter = deps.adapter
  if (adapter === undefined) {
    try {
      adapter = resolveTrackerAdapter({ env })
    } catch {
      adapter = null
    }
  }
  if (typeof adapter?.applyIssueWrites !== 'function') {
    return {
      code: 69,
      out: { ...decision, reason: 'the tracker adapter has no applyIssueWrites capability' },
    }
  }
  const comment = approvalComment({ ...decision, sessionId: env.BOSS_SESSION_ID })
  let result
  try {
    result = await adapter.applyIssueWrites({
      ...decision.writes,
      comment,
      marker: APPROVAL_MARKER,
    })
  } catch (err) {
    return { code: 69, out: { ...decision, reason: err?.message ?? String(err) } }
  }
  const outcome = result?.outcome ?? 'failed'
  if (outcome === 'ok') return { code: 0, out: { ...decision, outcome } }
  return {
    code: 69,
    out: { ...decision, outcome, reason: result?.reason ?? `label writes ${outcome}` },
  }
}

if (isMainModule(import.meta.url)) {
  const { code, out } = await runCli(process.argv.slice(2))
  process.stdout.write(`${JSON.stringify(out)}\n`)
  process.exit(code)
}
