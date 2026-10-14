#!/usr/bin/env node
// Route each candidate follow-up by its declared source, then render the PR-body sections.
// `## Human follow-up` and `## Open questions` park a PR at verify, so only an item about the
// change itself may land there; host problems, stage-owned evidence and plan questions the build
// already answered go elsewhere. An item the caller did not classify parks exactly as before.
// Pure apart from the CLI wrapper; nothing here fails closed except unreadable input, which must
// never render as "no follow-ups".
import defaultFs from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from './main-module.mjs'

export const FOLLOWUP_SOURCES = Object.freeze({
  'outside-worktree':
    'A criterion of this change that cannot be checked from a worktree (production access, a deployed environment). Optional `criterion`: the acceptance criterion it checks. Parks the PR under ## Human follow-up.',
  host: 'The build host or its environment: expired credentials, a missing tool, a review round that could not run. Optional `where`: the step or tool affected. Recorded as a host note; never parks the PR.',
  'stage-evidence':
    'Evidence a later pipeline stage produces (`stage`, default `verify`). Recorded as an autonomous decision; never parks the PR.',
  'plan-question':
    'One of the plan’s open questions, with `chosen` (the option built) and `alternative` (the one not taken). With `chosen`, recorded as an autonomous decision; without it, parks the PR under ## Open questions.',
  'review-finding':
    'A serious problem the review verified in code this change did not introduce (`review-verdict.mjs followups`). Optional `where`: its file:line. Listed under ## Review follow-ups for a tracker ticket; never parks the PR.',
})

const RESERVED_PHRASE = /do not merge/i
const LEDGER_HEADINGS = Object.freeze({
  humanFollowUp: 'Human follow-up',
  openQuestions: 'Open questions',
})

const oneLine = (value) =>
  (typeof value === 'string' ? value : '')
    .replace(/\s+/g, ' ')
    .trim()
    .replace(/^-\s+(\[[ xX]\]\s+)?/, '')
    .trim()

const normaliseSource = (value) =>
  typeof value === 'string'
    ? value
        .trim()
        .toLowerCase()
        .replace(/[_\s]+/g, '-')
    : ''

function candidateList(input, warnings) {
  if (Array.isArray(input)) return input
  if (Array.isArray(input?.candidates)) return input.candidates
  if (input && typeof input === 'object') return [input]
  warnings.push('candidates is not a list; nothing routed')
  return []
}

// Optional fields a route interpolates into the PR body; each is checked for the reserved phrase.
const EMITTED_FIELDS = Object.freeze(['criterion', 'where', 'stage', 'chosen', 'alternative'])

// One route per FOLLOWUP_SOURCES key; the test pins that the two key sets are identical.
export const ROUTES = Object.freeze({
  'outside-worktree': ({ candidate, text, label, routed }) => {
    const criterion = oneLine(candidate.criterion)
    if (!criterion) routed.warnings.push(`${label}: outside-worktree item names no criterion`)
    routed.humanFollowUp.push(criterion ? `${text} (criterion: ${criterion})` : text)
  },
  host: ({ candidate, text, routed }) => {
    const where = oneLine(candidate.where)
    routed.hostNotes.push(where ? { where, detail: text } : { where: text, detail: '' })
  },
  'stage-evidence': ({ candidate, text, routed }) => {
    const stage = oneLine(candidate.stage) || 'verify'
    routed.decisions.push(`${text} — evidence left to the \`${stage}\` stage`)
  },
  'plan-question': ({ candidate, text, label, routed }) => {
    const chosen = oneLine(candidate.chosen)
    if (!chosen) {
      routed.warnings.push(`${label}: plan question has no chosen option; left open`)
      routed.openQuestions.push(text)
      return
    }
    const alternative = oneLine(candidate.alternative) || 'none recorded'
    routed.decisions.push(
      `Plan question: ${text} — built: ${chosen}; alternative not taken: ${alternative}`,
    )
  },
  'review-finding': ({ text, routed }) => {
    routed.reviewFollowUps.push(text)
  },
})

function parkUnclassified({ text, label, source, routed }) {
  routed.warnings.push(
    `${label}: ${source ? `unknown source "${source}"` : 'no source'}; routed to ## ${LEDGER_HEADINGS.humanFollowUp}`,
  )
  routed.humanFollowUp.push(text)
}

/**
 * @param {unknown} candidates `{ text, source, criterion?, stage?, chosen?, alternative?, where? }[]`
 * @returns {{humanFollowUp: string[], openQuestions: string[], hostNotes: {where: string, detail: string}[], reviewFollowUps: string[], decisions: string[], warnings: string[]}}
 */
export function routeFollowUps(candidates) {
  const routed = {
    humanFollowUp: [],
    openQuestions: [],
    hostNotes: [],
    reviewFollowUps: [],
    decisions: [],
    warnings: [],
  }
  const { warnings } = routed
  for (const [index, raw] of candidateList(candidates, warnings).entries()) {
    const label = `candidate ${index + 1}`
    const candidate = typeof raw === 'string' ? { text: raw } : raw
    const text = oneLine(candidate?.text)
    if (!text) {
      warnings.push(`${label}: empty text; dropped`)
      continue
    }
    const fields = [text, ...EMITTED_FIELDS.map((field) => oneLine(candidate[field]))]
    if (fields.some((field) => RESERVED_PHRASE.test(field)))
      warnings.push(`${label}: rephrase "do not merge"; it is reserved for the partial marker`)
    const source = normaliseSource(candidate.source)
    const route = Object.hasOwn(ROUTES, source) ? ROUTES[source] : parkUnclassified
    route({ candidate, text, label, source, routed })
  }
  return routed
}

const ledgerSection = (heading, items) => [
  `## ${heading}`,
  ...(items.length ? items.map((item) => `- [ ] ${item}`) : ['- none']),
  '',
]

const listOf = (value) => (Array.isArray(value) ? value : [])

/**
 * Body text for `## Human follow-up`, `## Open questions` and, when non-empty, `## Host notes` and
 * `## Review follow-ups`.
 * @param {ReturnType<typeof routeFollowUps>} routed
 */
export function renderFollowUpSections(routed) {
  const lines = [
    ...ledgerSection(LEDGER_HEADINGS.humanFollowUp, listOf(routed?.humanFollowUp)),
    ...ledgerSection(LEDGER_HEADINGS.openQuestions, listOf(routed?.openQuestions)),
  ]
  const hostNotes = listOf(routed?.hostNotes)
  if (hostNotes.length) {
    lines.push('## Host notes')
    for (const note of hostNotes)
      lines.push(`- ${note.detail ? `${note.where}: ${note.detail}` : note.where}`)
    lines.push('')
  }
  const reviewFollowUps = listOf(routed?.reviewFollowUps)
  if (reviewFollowUps.length)
    lines.push('## Review follow-ups', ...reviewFollowUps.map((t) => `- ${t}`), '')
  return lines.join('\n')
}

export const USAGE =
  'usage: followup-route.mjs route --candidates <json-file> [--notes-out <json-file>]'

class UsageError extends Error {}

function parseCli(argv) {
  let parsed
  try {
    parsed = parseArgs({
      args: argv,
      options: { candidates: { type: 'string' }, 'notes-out': { type: 'string' } },
      allowPositionals: true,
    })
  } catch (error) {
    throw new UsageError(error.message)
  }
  const { positionals, values } = parsed
  const [command] = positionals
  if (positionals.length !== 1 || command !== 'route')
    throw new UsageError(`unknown verb: ${positionals.join(' ') || '(none)'}`)
  if (!values.candidates) throw new UsageError('route requires --candidates')
  return values
}

/**
 * Exit 0 with `{...routed, sections}` on stdout; exit 2 on a usage error or unreadable input.
 * @param {string[]} argv
 * @param {{fs?: typeof defaultFs, stdout?: (s: string) => void, stderr?: (s: string) => void}} [deps]
 */
export function main(argv, deps = {}) {
  const fs = deps.fs ?? defaultFs
  const stdout = deps.stdout ?? ((text) => process.stdout.write(text))
  const stderr = deps.stderr ?? ((text) => process.stderr.write(text))
  let values
  try {
    values = parseCli(argv)
  } catch (error) {
    stderr(`followup-route: ${error.message}\n${USAGE}\n`)
    return 2
  }
  let candidates
  try {
    candidates = JSON.parse(fs.readFileSync(values.candidates, 'utf8'))
  } catch (error) {
    stderr(`followup-route: cannot read --candidates: ${error.message}\n`)
    return 2
  }
  const routed = routeFollowUps(candidates)
  if (values['notes-out']) {
    try {
      fs.writeFileSync(values['notes-out'], `${JSON.stringify(routed.hostNotes)}\n`)
    } catch (error) {
      routed.warnings.push(`cannot write --notes-out: ${error.message}`)
    }
  }
  stdout(`${JSON.stringify({ ...routed, sections: renderFollowUpSections(routed) })}\n`)
  return 0
}

if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
