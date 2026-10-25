import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { selectImplementationPlanAttachment } from '../plan-attachment.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import {
  DEFAULT_CONFIG,
  loadSkillConfig,
  stateName,
  validatePlanDescription,
} from '../skill-config.mjs'

/**
 * Is the fetched payload the issue the run actually selected?
 *
 * The precheck's other three conjuncts all interrogate the payload it was handed; none of them asks
 * whether that payload is the right ticket. A mispicked id therefore plans a *different* ticket
 * while every guard reports a clean run. `selectedID` closes that: it is compared against both the
 * UUID (`issue.id`) and the human identifier (`issue.identifier`), because callers legitimately
 * hold either one.
 *
 * Case-insensitive by design. A UUID is lower-case hex and an identifier is upper-case (`ABC-123`),
 * so folding case can never make two genuinely different ids compare equal — it only removes a
 * spurious mismatch from a caller that normalised the selector differently.
 *
 * Absent (or blank) `selectedID` returns false and the conjunct never fires, so every pre-existing
 * caller keeps a byte-identical result.
 */
function fetchedIssueIdentityMismatch(issue, selectedID) {
  const selected = typeof selectedID === 'string' ? selectedID.trim().toLowerCase() : ''
  if (!selected) return false
  const candidates = [issue?.id, issue?.identifier]
    .filter((value) => typeof value === 'string' && value.trim() !== '')
    .map((value) => value.trim().toLowerCase())
  // No usable id on the payload at all is not a *match* either: a fetch that returned nothing
  // identifiable is exactly the case this conjunct exists to refuse to call clean.
  return !candidates.includes(selected)
}

/** @param {{issue?: Record<string, any>, selectedID?: string|null, config?: typeof DEFAULT_CONFIG}} [options] */
export function planIdempotencePrecheck({
  issue,
  selectedID = null,
  config = DEFAULT_CONFIG,
} = {}) {
  const reasons = []
  const attachments = Array.isArray(issue?.attachments) ? issue.attachments : []

  // Pushed FIRST because it subsumes the rest: if this is the wrong ticket, every verdict the other
  // three conjuncts reach is about a ticket nobody asked to plan. It can only ever add a reason, so
  // it can only ever push the run toward `plan` — never toward `noop`.
  if (fetchedIssueIdentityMismatch(issue, selectedID)) reasons.push('fetched-issue-id-mismatch')

  let planned = null
  try {
    planned = stateName(config, 'planned')
  } catch {
    planned = null
  }
  const currentState =
    issue?.status ??
    issue?.stateName ??
    (typeof issue?.state === 'string' ? issue.state : issue?.state?.name)
  if (!planned || currentState !== planned) reasons.push('state-not-planned')

  let descriptionValid = false
  try {
    descriptionValid = validatePlanDescription(config, issue?.description ?? '').ok
  } catch {
    descriptionValid = false
  }
  if (!descriptionValid) reasons.push('description-invalid')

  const issueID = issue?.id || issue?.identifier
  if (!issueID || !selectImplementationPlanAttachment(attachments, issueID)) {
    reasons.push('plan-attachment-missing')
  }

  return {
    action: reasons.length === 0 ? 'noop' : 'plan',
    reasons,
  }
}

// The `idempotence` verb reads its file as the BARE issue object. A `{issue:{…}}` wrapper — the
// natural mistake, because the function it feeds takes `{issue}` — leaves every field it reads
// undefined, so all three reasons fire at once and the verb prints `action:"plan"` with a full reason
// list. That output is byte-identical to the verdict a genuinely unplanned ticket produces, so the
// guard reads as working while having evaluated nothing at all.
//
// Scoped to an object whose ONLY own key is `issue`: a real issue payload that happens to carry an
// `issue` field alongside its own is not this mistake and is passed through untouched.
export function assertBareIssuePayload(value, file) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return
  const keys = Object.keys(value)
  if (keys.length === 1 && keys[0] === 'issue') {
    throw new Error(
      // No module prefix here: this throw is caught by the `unreadable-input` handler below, which
      // supplies `plan-run-guards: ` itself. Prefixing again printed
      // `unreadable-input: plan-run-guards: plan-run-guards: …` — a stutter no sibling diagnostic in
      // this file has.
      `idempotence <issue.json> (the BARE issue object) — ${file} contains a {issue:{…}} wrapper; every field would read as undefined and the verdict would be action:"plan" with every reason fired, which is exactly what an unplanned ticket looks like. Pass the issue object itself.`,
    )
  }
}

const issueState = (issue) =>
  issue?.status ??
  issue?.stateName ??
  (typeof issue?.state === 'string' ? issue.state : issue?.state?.name)

export function runPlanRecheck(argv) {
  try {
    const { values, positionals } = parseArgs({
      args: argv,
      allowPositionals: true,
      strict: true,
      options: {
        'selected-id': { type: 'string' },
        'claim-token': { type: 'string' },
        comments: { type: 'string' },
        'own-attachment-id': { type: 'string', multiple: true },
      },
    })
    if (
      positionals.length !== 2 ||
      !values['selected-id'] ||
      Boolean(values['claim-token']) !== Boolean(values.comments)
    )
      throw new Error(
        'recheck <selection.json> <fresh.json> --selected-id <id> [--claim-token <t> --comments <file>] [--own-attachment-id <id>]…',
      )
    const read = (file) => JSON.parse(readFileSync(file, 'utf8'))
    const [selection, fresh] = positionals.map((file) => {
      const issue = read(file)
      assertBareIssuePayload(issue, file)
      return issue
    })
    const selectedID = values['selected-id']
    const config = loadSkillConfig()
    const reasons = []
    if (
      fetchedIssueIdentityMismatch(fresh, selectedID) ||
      fetchedIssueIdentityMismatch(selection, selectedID)
    )
      reasons.push('fetched-issue-id-mismatch')
    if (planIdempotencePrecheck({ issue: fresh, selectedID, config }).action === 'noop')
      reasons.push('planned-by-peer')
    if (issueState(fresh) !== issueState(selection)) reasons.push('state-moved')
    const priorIDs = new Set((selection.attachments ?? []).map((attachment) => attachment.id))
    // Only exact ids retained from this run's finalization are exempt; the selection stays immutable.
    const ownIDs = new Set(values['own-attachment-id'] ?? [])
    const attachments = (fresh.attachments ?? []).filter((attachment) => !ownIDs.has(attachment.id))
    const plan = selectImplementationPlanAttachment(attachments, fresh.identifier ?? fresh.id)
    if (
      (plan && !priorIDs.has(plan.id)) ||
      attachments.some(
        (attachment) =>
          attachment.title === `Epic spec (${fresh.identifier ?? fresh.id})` &&
          !priorIDs.has(attachment.id),
      )
    )
      reasons.push('peer-attachment')
    if (values['claim-token']) {
      const adapter = resolveTrackerAdapter(process.env)
      if (!adapter.claimScopes?.includes('plan'))
        throw new Error('tracker adapter missing claimScopes plan capability')
      const won = adapter.resolveClaim(read(values.comments), values['claim-token'], {
        scope: 'plan',
      })
      if (won === false) reasons.push('claim-lost')
      else if (won === null) reasons.push('claim-missing')
    }
    process.stdout.write(
      `${JSON.stringify({ action: reasons.length ? 'stop' : 'proceed', reasons })}\n`,
    )
    return { code: reasons.length ? 3 : 0, reason: reasons[0] ?? 'ok' }
  } catch (error) {
    process.stderr.write(`unreadable-input: plan-run-guards: ${error.message}\n`)
    return { code: 2, reason: 'unreadable-input' }
  }
}
