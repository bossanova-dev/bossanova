#!/usr/bin/env node

// Deterministic guards for boss-plan's run boundaries. These checks sit between
// untrusted drafting output and tracker writeback, so they return structured
// violations and keep the CLI shape small enough for skill bash blocks.

import { readFileSync } from 'node:fs'

import { checkPlanContract } from './plan-contract-guard.mjs'
import { selectImplementationPlanAttachment } from './plan-attachment.mjs'
import { planScratchToken } from './plan-scratch-paths.mjs'
import { createGateRecorder } from './gate-outcome.mjs'
import { isMainModule } from './main-module.mjs'
import {
  DEFAULT_CONFIG,
  loadSkillConfig,
  stateName,
  validatePlanDescription,
} from './skill-config.mjs'

export const PREMISE_LIMIT = 10

const ALLOWED_METADATA_KEYS = new Set([
  'planPath',
  'labels',
  'agentFriendly',
  'estimate',
  'priority',
  'openQuestions',
  'descriptionSummary',
])

const VALID_ESTIMATES = new Set([0, 1, 2, 3, 5])

function entry(code, field, message, extra = {}) {
  return { code, field, message: `plan-run-guards: ${message}`, ...extra }
}

function hasAtomic5Justification(descriptionSummary) {
  const sections = String(descriptionSummary ?? '').split(/\n(?=##\s)/)
  const planning = sections.find((section) => section.trimStart().startsWith('## Planning'))
  return /^[-*]\s*Atomic-5:\s*\S/m.test(planning ?? '')
}

// `descriptionSummary` is the one returned field whose exact bytes are gated downstream
// (`plan-image-guard.mjs --require-verbatim` over its `## Original notes` block), and the headless
// dispatch return channel is not byte-preserving. So the value is a discriminated union: today's
// inline string, or a by-reference `{ path }` naming a declared `description` scratch artifact —
// the kind of file the drafter already assembled and the write-back already reads.
//
// The widening is fail-closed by construction, which is what keeps it from being a blanket pass:
// a reference is accepted only for the `description` family, and only a caller that supplies
// `resolveDescription` gets the reference resolved at all. A caller that widens the accepted shape
// without hydrating it is refused (`description-summary-unresolved`) rather than skipping the
// contract check, and a resolved reference is held to exactly the contract an inline string is —
// the check runs over the BYTES, never over the path.
//
// What that check does NOT establish is run identity: `planScratchToken` proves family membership
// under SOME `run-<id>/`, not that the path is THIS run's. Nothing here compares it against the
// run scratch id or the `planPath` issue, so a reference to a peer run's description artifact
// validates. The residual is bounded downstream rather than here: Phase 4 derives `$BODY` from the
// run-scratch template rather than from this returned value, and the image-parity, verbatim and
// plan-contract STOP gates — plus the write-back itself — all read that template-derived copy.
// A divergent path therefore mis-targets only this guard's own verdict, and a false pass from it
// is re-caught by the plan-contract gate running `checkPlanContract` over the bytes actually
// written. Binding the reference here would need an expected-artifact input this CLI does not
// have today; do not "fix" it by making Phase 4 consume the returned path instead, which would
// move the gates OFF the run-scoped artifact and turn a bounded residual into a live one.
function readDescriptionSummary(value, resolveDescription) {
  if (typeof value === 'string') {
    return value.trim() === '' ? { kind: 'invalid' } : { kind: 'text', text: value }
  }
  // Arrays and `null` are `typeof 'object'` but are not attempts at the reference form; they are
  // the same plain `invalid` a number is, with no reference reason worth printing.
  if (!value || typeof value !== 'object' || Array.isArray(value)) return { kind: 'invalid' }

  const badReference = (reason) => ({ kind: 'bad-reference', reason })
  const keys = Object.keys(value)
  if (keys.length !== 1 || keys[0] !== 'path' || typeof value.path !== 'string') {
    return badReference(
      'a by-reference descriptionSummary must be exactly {"path": "<this run\'s description artifact>"}',
    )
  }
  const token = planScratchToken(value.path)
  if (!token.ok) return badReference(token.reason)
  const families = token.families ?? []
  if (token.kind !== 'artifact' || families.length !== 1 || families[0] !== 'description') {
    return badReference(
      `${value.path} is not a declared description artifact — it resolves to ` +
        `${families.length > 0 ? families.join(', ') : token.kind}, and only the \`description\` ` +
        'scratch family may carry a by-reference descriptionSummary',
    )
  }
  if (typeof resolveDescription !== 'function') return { kind: 'unresolved', path: value.path }
  let text
  try {
    text = resolveDescription(value.path)
  } catch (error) {
    return { kind: 'unreadable', path: value.path, reason: error?.message ?? String(error) }
  }
  if (typeof text !== 'string' || text.trim() === '') {
    return { kind: 'unreadable', path: value.path, reason: 'resolved to no description bytes' }
  }
  return { kind: 'text', text }
}

export function validateDraftMetadata(
  metadata,
  { config = DEFAULT_CONFIG, resolveDescription, moduleRoots = [] } = {},
) {
  const missing = []
  const invalid = []
  const violations = []

  const object =
    metadata && typeof metadata === 'object' && !Array.isArray(metadata) ? metadata : null
  if (!object) {
    violations.push(entry('metadata-not-object', 'metadata', 'metadata must be a JSON object'))
    return { ok: false, missing, invalid, violations }
  }

  for (const key of ALLOWED_METADATA_KEYS) {
    if (!Object.hasOwn(object, key)) missing.push(key)
  }
  for (const key of Object.keys(object)) {
    if (!ALLOWED_METADATA_KEYS.has(key)) {
      violations.push(entry('unknown-key', key, `metadata carries unknown top-level key "${key}"`))
    }
  }

  // Read the union ONCE, before the estimate check, so the Atomic-5 justification is looked for in
  // the same bytes the contract check runs over whichever arm of the union carried them.
  const summary = Object.hasOwn(object, 'descriptionSummary')
    ? readDescriptionSummary(object.descriptionSummary, resolveDescription)
    : null
  const descriptionText = summary?.kind === 'text' ? summary.text : null

  if (Object.hasOwn(object, 'planPath')) {
    if (typeof object.planPath !== 'string' || object.planPath.trim() === '')
      invalid.push('planPath')
  }
  if (Object.hasOwn(object, 'labels')) {
    if (!Array.isArray(object.labels) || object.labels.some((label) => typeof label !== 'string')) {
      invalid.push('labels')
    }
  }
  if (Object.hasOwn(object, 'agentFriendly')) {
    if (typeof object.agentFriendly !== 'boolean') invalid.push('agentFriendly')
  }
  if (Object.hasOwn(object, 'estimate')) {
    if (!Number.isInteger(object.estimate) || !VALID_ESTIMATES.has(object.estimate)) {
      invalid.push('estimate')
    } else if (
      object.estimate === 5 &&
      // Only arms that actually carried bytes can be judged here. An unhydrated or unreadable
      // reference already reports its own cause under `descriptionSummary`; re-reporting it as a
      // missing justification sends the reader to edit a `## Planning` section that may well be
      // correct, in a file this run never opened.
      (summary === null || summary.kind === 'text' || summary.kind === 'invalid') &&
      !hasAtomic5Justification(descriptionText)
    ) {
      invalid.push('estimate')
      violations.push(
        entry(
          'missing-atomic-5',
          'estimate',
          'estimate 5 requires an "- Atomic-5:" justification under ## Planning',
        ),
      )
    }
  }
  if (Object.hasOwn(object, 'priority')) {
    if (!Number.isInteger(object.priority) || object.priority < 1 || object.priority > 4) {
      invalid.push('priority')
    }
  }
  if (Object.hasOwn(object, 'openQuestions')) {
    if (
      !Array.isArray(object.openQuestions) ||
      object.openQuestions.some((question) => typeof question !== 'string')
    ) {
      invalid.push('openQuestions')
    }
  }
  if (summary) {
    if (summary.kind === 'invalid') {
      invalid.push('descriptionSummary')
    } else if (summary.kind === 'bad-reference') {
      invalid.push('descriptionSummary')
      violations.push(
        entry('description-summary-bad-reference', 'descriptionSummary', summary.reason),
      )
    } else if (summary.kind === 'unresolved') {
      violations.push(
        entry(
          'description-summary-unresolved',
          'descriptionSummary',
          `descriptionSummary names ${summary.path} but this caller supplied no resolveDescription, ` +
            'so the description contract could not be checked over the referenced bytes — ' +
            'hydrate the reference rather than accepting it unchecked',
        ),
      )
    } else if (summary.kind === 'unreadable') {
      violations.push(
        entry(
          'description-summary-unreadable',
          'descriptionSummary',
          `descriptionSummary names ${summary.path}, which could not be read: ${summary.reason}`,
        ),
      )
    } else {
      // `moduleRoots` rides along so this caller's contract check classifies a `## Key changes`
      // token exactly as the dependency scan given the same roots would. Dropping it here made the
      // check stricter than the scan it reports for, on a seam no caller could reach.
      const contract = checkPlanContract({ description: summary.text, config, moduleRoots })
      for (const violation of contract.violations) {
        violations.push(
          entry(
            `description-summary-${violation.code}`,
            'descriptionSummary',
            violation.message.replace(/^plan-contract-guard:\s*/, ''),
            { source: violation },
          ),
        )
      }
    }
  }

  return {
    ok: missing.length === 0 && invalid.length === 0 && violations.length === 0,
    missing,
    invalid: [...new Set(invalid)],
    violations,
  }
}

export function planIdempotencePrecheck({ issue, config = DEFAULT_CONFIG } = {}) {
  const reasons = []
  const attachments = Array.isArray(issue?.attachments) ? issue.attachments : []

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

export function premiseDrift(premises, liveStates) {
  const list = Array.isArray(premises) ? premises : []
  const states =
    liveStates && typeof liveStates === 'object' && !Array.isArray(liveStates) ? liveStates : {}
  const drifted = []
  const unresolved = []
  let verified = 0

  for (const premise of list) {
    const id = typeof premise?.id === 'string' ? premise.id : ''
    if (!id) continue
    if (!Object.hasOwn(states, id)) {
      unresolved.push(id)
      continue
    }
    verified += 1
    const plannedState = premise.state
    const currentState = states[id]
    if (plannedState !== currentState) drifted.push({ id, plannedState, currentState })
  }

  return {
    ok: drifted.length === 0 && unresolved.length === 0,
    drifted,
    unresolved,
    verified,
    declared: list.length,
  }
}

function readJSON(file) {
  return JSON.parse(readFileSync(file, 'utf8'))
}

// The `idempotence` verb reads its file as the BARE issue object. A `{issue:{…}}` wrapper — the
// natural mistake, because the function it feeds takes `{issue}` — leaves every field it reads
// undefined, so all three reasons fire at once and the verb prints `action:"plan"` with a full reason
// list. That output is byte-identical to the verdict a genuinely unplanned ticket produces, so the
// guard reads as working while having evaluated nothing at all.
//
// Scoped to an object whose ONLY own key is `issue`: a real issue payload that happens to carry an
// `issue` field alongside its own is not this mistake and is passed through untouched.
function assertBareIssuePayload(value, file) {
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

function printViolations(result) {
  for (const field of result.missing ?? []) {
    process.stderr.write(`plan-run-guards: missing ${field}\n`)
  }
  for (const field of result.invalid ?? []) {
    process.stderr.write(`plan-run-guards: invalid ${field}\n`)
  }
  for (const violation of result.violations ?? []) {
    process.stderr.write(`${violation.code}: ${violation.message}\n`)
  }
}

/** Run one verb and report the exit code alongside the gate id and reason to record for it. */
// The dependency scan's repo module roots, as a comma-separated `--module-roots` value. Optional
// and positional-agnostic: every verb here takes positional inputs, so the flag is read out of the
// whole argv rather than from a fixed slot. Absent, the contract check keeps its old empty list.
function parseModuleRootsFlag(argv) {
  const index = argv.indexOf('--module-roots')
  if (index === -1) return []
  return String(argv[index + 1] ?? '')
    .split(',')
    .map((root) => root.trim())
    .filter((root) => root !== '')
}

function runGuardVerb(argv) {
  const [command, first, second] = argv
  // The verbs are gates in their own right: each is invoked at a different point in a planning run
  // and refuses for different reasons, so the retirement decision this record feeds needs their
  // firing rates apart rather than summed — which is why the recorded gate id carries the verb
  // (`plan-run-guards.premises`). Every dispatch branch below claims its verb as its FIRST
  // statement, so the verb is already correct if that branch later throws; `usage` is the standing
  // answer for an argv that matched no branch at all. Deriving it from the branch rather than from
  // a second list of verb names is what stops a verb added to only one of the two from recording
  // its outcomes under the wrong gate id.
  let verb = 'usage'
  try {
    if (command === 'metadata' && first) {
      verb = 'metadata'
      // This verb IS the orchestrator's boundary, so it is where the by-reference
      // `descriptionSummary` gets hydrated: the guard itself stays pure and refuses an
      // unhydrated reference, and the disk read lives here where the run's scratch is.
      const result = validateDraftMetadata(readJSON(first), {
        config: loadSkillConfig(),
        resolveDescription: (file) => readFileSync(file, 'utf8'),
        moduleRoots: parseModuleRootsFlag(argv),
      })
      printViolations(result)
      return {
        verb,
        code: result.ok ? 0 : 1,
        reason: result.ok ? 'ok' : 'invalid-metadata',
      }
    }
    if (command === 'idempotence' && first) {
      verb = 'idempotence'
      const payload = readJSON(first)
      // Raised through the existing `unreadable-input` catch below, so the CLI exits non-zero with a
      // named reason rather than printing a plausible verdict it never computed.
      assertBareIssuePayload(payload, first)
      const result = planIdempotencePrecheck({ issue: payload, config: loadSkillConfig() })
      process.stdout.write(`${JSON.stringify(result)}\n`)
      // This verb never refuses — it reports whether planning is still needed. Both answers are a
      // `pass`; the reason carries which one, so the record stays informative without inventing a
      // fire that the caller never saw.
      return { verb, code: 0, reason: result.action === 'noop' ? 'noop' : 'plan' }
    }
    if (command === 'premises' && first && second) {
      verb = 'premises'
      const premises = readJSON(first)
      const liveStates = readJSON(second)
      const result = premiseDrift(premises, liveStates)
      const overLimit = Array.isArray(premises) && premises.length > PREMISE_LIMIT
      process.stderr.write(`premises: verified ${result.verified} of ${result.declared}\n`)
      if (overLimit) {
        process.stderr.write(
          `premise-limit: plan-run-guards: premises length ${premises.length} exceeds ${PREMISE_LIMIT}\n`,
        )
      }
      for (const item of result.drifted) {
        process.stderr.write(
          `premise-drift: plan-run-guards: ${item.id} was ${item.plannedState}, is now ${item.currentState}\n`,
        )
      }
      for (const id of result.unresolved) {
        process.stderr.write(`premise-unresolved: plan-run-guards: ${id} could not be read\n`)
      }
      // Reasons reuse the stderr code vocabulary above rather than inventing a second set.
      let reason = 'ok'
      if (overLimit) reason = 'premise-limit'
      else if (result.unresolved.length > 0) reason = 'premise-unresolved'
      else if (result.drifted.length > 0) reason = 'premise-drift'
      return {
        verb,
        code: overLimit || result.unresolved.length > 0 ? 1 : 0,
        reason,
      }
    }
  } catch (error) {
    process.stderr.write(`unreadable-input: plan-run-guards: ${error?.message ?? error}\n`)
    return { verb, code: 1, reason: 'unreadable-input' }
  }
  process.stderr.write(
    'usage: plan-run-guards.mjs metadata <metadata.json> [--module-roots <a,b>] | idempotence <issue.json> | premises <premises.json> <live-states.json>\n',
  )
  return { verb, code: 2, reason: 'unknown-verb' }
}

export function runCli(argv) {
  const { verb, code, reason } = runGuardVerb(argv)
  // One line per invocation, recorded from the single place every verb returns through,
  // so a new verb cannot be added without an outcome.
  createGateRecorder(`plan-run-guards.${verb}`).record(code === 0 ? 'pass' : 'fire', reason)
  return code
}

if (isMainModule(import.meta.url)) {
  process.exitCode = runCli(process.argv.slice(2))
}
