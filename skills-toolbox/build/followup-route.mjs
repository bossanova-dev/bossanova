#!/usr/bin/env node
// Route each candidate follow-up by its declared source, then render the PR-body sections. The
// sections are for people reading the PR; no merge gate reads them. A plan question the build did
// not answer is returned in `questions` and never published: the run decides it or asks it.
// Pure apart from the CLI wrapper; nothing here fails closed except unreadable input, which must
// never render as "no follow-ups".
import defaultFs from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'

export const FOLLOWUP_SOURCES = Object.freeze({
  'outside-worktree':
    'A criterion of this change that cannot be checked from a worktree (production access, a deployed environment). Optional `criterion`: the acceptance criterion it checks. Listed under ## Human follow-up.',
  host: 'The build host or its environment: expired credentials, a missing tool, a review round that could not run. Optional `where`: the step or tool affected. Recorded as a host note.',
  'stage-evidence':
    'Evidence a later pipeline stage produces (`stage`, default `verify`). Recorded as an autonomous decision.',
  'plan-question':
    'One of the plan’s open questions, with `chosen` (the option built) and `alternative` (the one not taken). With `chosen`, recorded as an autonomous decision; without it, returned in `questions` for the run to decide or ask.',
  'review-finding':
    'A serious problem the review verified in code this change did not introduce (`review-verdict.mjs followups`). Optional `where`: its file:line. Listed under ## Review follow-ups for a tracker ticket.',
  'post-merge':
    'An observation that can only happen once the change is merged (a scheduled run, a rollout, a production metric). Optional `criterion`: the plan item it observes. Listed under ## After merge.',
})

const HUMAN_FOLLOW_UP = 'Human follow-up'

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
      routed.warnings.push(`${label}: plan question has no chosen option; decide or ask it`)
      routed.questions.push(text)
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
  'post-merge': ({ candidate, text, routed }) => {
    const criterion = oneLine(candidate.criterion)
    routed.afterMerge.push(criterion ? `${text} (criterion: ${criterion})` : text)
  },
})

function unclassified({ text, label, source, routed }) {
  routed.warnings.push(
    `${label}: ${source ? `unknown source "${source}"` : 'no source'}; routed to ## ${HUMAN_FOLLOW_UP}`,
  )
  routed.humanFollowUp.push(text)
}

/**
 * @param {unknown} candidates `{ text, source, criterion?, stage?, chosen?, alternative?, where? }[]`
 * @returns {{humanFollowUp: string[], questions: string[], afterMerge: string[], hostNotes: {where: string, detail: string}[], reviewFollowUps: string[], decisions: string[], warnings: string[]}}
 */
export function routeFollowUps(candidates) {
  const routed = {
    humanFollowUp: [],
    questions: [],
    afterMerge: [],
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
    const source = normaliseSource(candidate.source)
    const route = Object.hasOwn(ROUTES, source) ? ROUTES[source] : unclassified
    route({ candidate, text, label, source, routed })
  }
  return routed
}

const listOf = (value) => (Array.isArray(value) ? value : [])

/**
 * Body text for each non-empty section: `## Human follow-up`, `## After merge`, `## Host notes` and
 * `## Review follow-ups`.
 * @param {ReturnType<typeof routeFollowUps>} routed
 */
export function renderFollowUpSections(routed) {
  const lines = []
  const humanFollowUp = listOf(routed?.humanFollowUp)
  if (humanFollowUp.length)
    lines.push(`## ${HUMAN_FOLLOW_UP}`, ...humanFollowUp.map((item) => `- [ ] ${item}`), '')
  const afterMerge = listOf(routed?.afterMerge)
  if (afterMerge.length)
    lines.push('## After merge', ...afterMerge.map((item) => `- [ ] ${item}`), '')
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
