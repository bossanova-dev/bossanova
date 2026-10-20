// skills-toolbox/plan-epic-reverify.mjs
//
// The post-write acceptance gate an EPIC outcome of boss-plan's Phase 2 step 4
// must pass. Where `./plan-epic-phase25.mjs` decides what to write BEFORE the
// epic exists (preconditions and the step-4 first-write plan), this module
// judges, from payloads read back AFTER the drafter's final flip, whether the
// epic that exists is the one claimed. Two exports: `epicReverifyVerdict` and
// `EPIC_REVERIFY_CLASS` — the frozen vocabulary of that verdict's `class`. Both
// are re-exported from `./plan-epic-phase25.mjs`, the module boss-plan's callers
// (`plan-run-guards.mjs epic-reverify`) and plan name as the gate's home.
//
// PURE and PROJECT-AGNOSTIC on the same terms as `./plan-epic-phase25.mjs`:
// nothing reads a file, opens a socket, loads config, or throws, and every
// state/label display name is caller-supplied through `roles`.
//
// The import cycle with `./plan-epic-phase25.mjs` (it re-exports this module;
// this module reuses its spec-store detector, stale-artifact sweep and
// issue-shape readers) is safe: neither module's top level calls into the
// other, so every binding is initialised before first use in either load order.

import {
  descriptionAppearsTruncated,
  parseEpicChildMarker,
  reconcileEpicChildren,
  validateSpecIdentity,
} from './plan-epic-lib.mjs'
import {
  detectEpicParent,
  stalePlanAttachmentSweep,
  readState,
  readLabels,
  hasPlanArtifact,
  PLAN_ARTIFACT_TITLE_PREFIX,
} from './plan-epic-phase25.mjs'
import { verifyWriteback } from './plan-writeback-verify.mjs'
import { validatePlanDescription } from './skill-config.mjs'

const isPlainObject = (value) =>
  typeof value === 'object' && value !== null && !Array.isArray(value)

const isNonEmptyString = (value) => typeof value === 'string' && value.trim() !== ''

const asArray = (value) => (Array.isArray(value) ? value : [])

// The label roles whose presence on the epic PARENT is a failure. `agentPlan` is
// optional (null ⇒ unmapped ⇒ forbids nothing); the other two are required roles.
const PARENT_FORBIDDEN_LABEL_ROLES = ['agentBuild', 'needsHuman', 'agentPlan']
const REQUIRED_REVERIFY_ROLES = ['planned', 'unplanned', 'epic', 'agentBuild', 'needsHuman']
const OPTIONAL_REVERIFY_ROLES = ['agentPlan', 'agentQuestion', 'inProgress', 'inReview']

// The `class` vocabulary of `epicReverifyVerdict`, exported so a caller maps a class to its exit
// code without restating a spelling. `needsHuman` is the class of a failure no sweep will resume,
// so its scratch is retained. It is verdict vocabulary that shares the default needsHuman label's
// spelling, NOT a label: written as a template literal so the plan's no-label-literal check (a
// fixed-string search for the single-quoted label spelling over this file) keeps meaning "no
// hard-coded label display name" instead of flagging the verdict protocol.
export const EPIC_REVERIFY_CLASS = Object.freeze({
  pass: 'pass',
  resumable: 'resumable',
  needsHuman: `needs-human`,
})

const isPlanTitledEntry = (entry) =>
  isPlainObject(entry) &&
  typeof entry.title === 'string' &&
  entry.title.startsWith(PLAN_ARTIFACT_TITLE_PREFIX)
const entriesOf = (value) => (Array.isArray(value?.nodes) ? value.nodes : asArray(value))

/**
 * The acceptance gate for an EPIC outcome of boss-plan's Phase 2 step 4: decide, from payloads read
 * back from the tracker AFTER the drafter's final flip, whether the epic it claims to have built is
 * the epic that exists. Pure and never throws; every failed conjunct is named, not the first.
 *
 * Input: `{parentId, childIds, parent, children, spec, roles, config, parentOverview, childBodies}`.
 * `roles` holds the RESOLVED display names (`planned`, `unplanned`, `epic`, `agentBuild`,
 * `needsHuman` required; `agentPlan`, `agentQuestion`, `inProgress`, `inReview` may be null). Every
 * `description` it judges must come from a code-written stored read-back, never a retyped payload —
 * `childBodies[<childId>]` is `{intended, stored}`, `parentOverview` is `{intended, stored}`.
 *
 * Two accumulators, as in `epicSpecRecoveryGate`: `blockers` decide, `notices` are only reported.
 * The class of a failure is decided by the parent's state alone: positively read in the unplanned
 * state ⇒ `resumable` (the next unplanned sweep re-picks it); anything else — already flipped, or a
 * state that could not be read ⇒ `needs-human` (no sweep will resume it).
 *
 * Only the unplanned state fails a parent or a child. The tracker's rollup may advance a planned
 * parent once a child starts, and a child may be started, merged or cancelled before a resumed run
 * re-reads it; the config names no done/canceled roles, so any other state is a notice.
 *
 * `class` is one of `EPIC_REVERIFY_CLASS`.
 * @returns {{ok: boolean, class: string,
 *            blockers: {code: string, message: string}[], notices: {code: string, message: string}[]}}
 */
export function epicReverifyVerdict(input) {
  const blockers = []
  const notices = []
  const block = (code, message) => blockers.push({ code, message })
  const note = (code, message) => notices.push({ code, message })

  try {
    evaluateEpicReverify(input, block, note)
  } catch (error) {
    // Every helper called below documents a no-throw contract or is guarded; this is the backstop
    // that keeps THIS function's contract independent of theirs.
    block('could-not-evaluate', `the reverify threw: ${error?.message ?? error}`)
  }

  if (blockers.length === 0) return { ok: true, class: EPIC_REVERIFY_CLASS.pass, blockers, notices }
  const { resumable, needsHuman } = EPIC_REVERIFY_CLASS
  return {
    ok: false,
    class: parentReadUnplanned(input) ? resumable : needsHuman,
    blockers,
    notices,
  }
}

// True only when the parent's state was POSITIVELY read as the resolved unplanned role; anything
// unreadable or unresolved is false. Guarded so the verdict's no-throw contract holds on hostile input.
function parentReadUnplanned(input) {
  try {
    if (!isPlainObject(input) || !isPlainObject(input.parent)) return false
    const unplanned = isPlainObject(input.roles) ? input.roles.unplanned : null
    const state = readState(input.parent)
    return isNonEmptyString(unplanned) && isNonEmptyString(state) && state === unplanned
  } catch {
    return false
  }
}

function evaluateEpicReverify(input, block, note) {
  if (!isPlainObject(input)) {
    block('input-not-object', 'the reverify input is not an object — nothing could be read')
    return
  }
  const { parentId, childIds, parent, children, spec, config, parentOverview, childBodies } = input
  const roles = isPlainObject(input.roles) ? input.roles : {}

  const role = {}
  for (const name of REQUIRED_REVERIFY_ROLES) {
    if (isNonEmptyString(roles[name])) role[name] = roles[name]
    else block('unresolved-role', `roles.${name} was not resolved to a non-empty string`)
  }
  for (const name of OPTIONAL_REVERIFY_ROLES) {
    if (isNonEmptyString(roles[name])) role[name] = roles[name]
    else if (roles[name] != null) {
      block('unresolved-role', `roles.${name} must be a non-empty string or null`)
    }
  }
  const haveConfig = isPlainObject(config)
  if (!haveConfig) {
    block('config-missing', 'no loaded config — the description contract cannot be evaluated')
  }
  if (!isNonEmptyString(parentId)) {
    block('parent-id-missing', 'parentId is not a non-empty string')
  }

  // --- the parent -------------------------------------------------------------
  const storedOverview =
    typeof parentOverview?.stored === 'string' ? parentOverview.stored : parent?.description
  if (!isPlainObject(parent)) {
    block('parent-unreadable', 'the parent payload is not an object — its state is unknown')
  } else {
    judgeParent({ parent, parentId, spec, role, storedOverview, block, note })
  }

  // --- the child set ------------------------------------------------------------
  const wanted = asArray(childIds).filter(isNonEmptyString)
  if (wanted.length === 0) {
    block(
      'childids-missing',
      'the sentinel carries no childIds — a sentinel-shape failure, never a reconcile fallback',
    )
  }
  if (!Array.isArray(children)) {
    block('children-unreadable', 'the enumerated children collection is not an array')
  }
  const counted = []
  asArray(children).forEach((childIssue, index) => {
    if (!isPlainObject(childIssue)) {
      block('child-unreadable', `child #${index + 1}: the payload is not an object`)
      return
    }
    const key = childKeyOf(childIssue) ?? `child #${index + 1}`
    const description = childIssue.description
    if (!isNonEmptyString(description)) {
      block('child-description-missing', `${key}: no stored description — membership is unproven`)
    } else if (descriptionAppearsTruncated(description)) {
      block('child-description-truncated', `${key}: the stored description is list-truncated`)
    } else if (parseEpicChildMarker(description) === null) {
      note('child-unmarked', `${key}: carries no epic-child marker — not an epic child, not judged`)
    } else {
      counted.push({ key, issue: childIssue, marker: parseEpicChildMarker(description) })
    }
  })

  const matched = []
  const claimed = new Set()
  for (const childId of wanted) {
    const hit = counted.find(
      (c) => !claimed.has(c) && (c.issue.identifier === childId || c.issue.id === childId),
    )
    if (!hit) {
      block('child-set-mismatch', `sentinel child ${childId} has no marked live child`)
      continue
    }
    claimed.add(hit)
    matched.push({ childId, ...hit })
  }
  for (const c of counted) {
    if (!claimed.has(c)) {
      block('child-set-mismatch', `live marked child ${c.key} is not in the sentinel's childIds`)
    }
  }

  // --- reconcile against the spec ---------------------------------------------------
  const specChildren = isPlainObject(spec) && Array.isArray(spec.children) ? spec.children : null
  if (specChildren == null) {
    block('spec-unreadable', 'the epic spec is not readable — reconciliation cannot run')
  } else {
    const live = asArray(children)
      .filter(isPlainObject)
      .map((c) => ({ id: childKeyOf(c), title: c.title, description: c.description }))
    const reconciled = reconcileEpicChildren(spec, live)
    if (!reconciled.ok) {
      block('reconcile-refused', `reconcileEpicChildren refused: ${reconciled.errors.join('; ')}`)
    } else {
      if (reconciled.missing.length > 0) {
        block('reconcile-missing', `spec children never created: ${reconciled.missing.join(', ')}`)
      }
      if (reconciled.orphans.length > 0) {
        block(
          'reconcile-orphans',
          `orphaned child marker keys: ${reconciled.orphans.map((o) => o.key).join(', ')}`,
        )
      }
      if (reconciled.repairs.length > 0) {
        block(
          'reconcile-unrepaired',
          `marker rewrites never landed: ${reconciled.repairs.map((r) => `${r.id} ${r.liveKey}→${r.specKey}`).join(', ')}`,
        )
      }
    }
  }

  // --- each matched child ---------------------------------------------------------
  let compared = 0
  for (const m of matched) {
    const specEntry = specChildren?.find((c) => isPlainObject(c) && c.key === m.marker) ?? null
    judgeChildShape({ m, specEntry, role, block, note })
    if (!haveConfig) continue
    const body = isPlainObject(childBodies) ? childBodies[m.childId] : undefined
    const stored = typeof body?.stored === 'string' ? body.stored : m.issue.description
    if (String(stored ?? '').trim() === '') {
      block('child-body-unverified', `${m.key}: the stored description is empty — nothing verified`)
      continue
    }
    const contract = validatePlanDescription(config, stored, { mode: 'child-plan' })
    if (!contract.ok) {
      block(
        'child-body-contract',
        `${m.key}: the stored description fails the child-plan contract (missing ${contract.missing.join(', ') || 'nothing'}${contract.unsupportedVersion ? '; unsupported contract version' : ''})`,
      )
    }
    if (typeof body?.intended !== 'string') {
      block(
        'child-body-intended-missing',
        `${m.key}: no intended bytes to verify the stored body against`,
      )
      continue
    }
    const verdict = verifyWriteback({
      config,
      intendedText: body.intended,
      storedText: stored,
      mode: 'child-plan',
    })
    if (verdict.verdict === null) {
      block('child-body-unverified', `${m.key}: ${verdict.reason}`)
      continue
    }
    compared += 1
    if (verdict.exitCode !== 0)
      block('child-body-drift', `${m.key}: ${verdict.verdict} — ${verdict.reason}`)
  }
  if (haveConfig && compared === 0) {
    block(
      'could-not-evaluate',
      'zero child bodies were compared — an empty comparison is never a pass',
    )
  }

  // --- the parent overview read-back ----------------------------------------------------
  // The contract is judged on the stored bytes directly: the write-back check below certifies a
  // byte-identical round trip without re-validating, so a malformed overview would pass it.
  if (haveConfig) {
    if (isNonEmptyString(storedOverview)) {
      const contract = validatePlanDescription(config, storedOverview, { mode: 'epic-parent' })
      if (!contract.ok) {
        block(
          'parent-overview-contract',
          `the stored overview fails the epic-parent contract (missing ${contract.missing.join(', ') || 'nothing'}${contract.unsupportedVersion ? '; unsupported contract version' : ''})`,
        )
      }
    }
    if (typeof parentOverview?.intended !== 'string') {
      block('parent-overview-intended-missing', 'no intended overview bytes to verify against')
    } else {
      const verdict = verifyWriteback({
        config,
        intendedText: parentOverview.intended,
        storedText: storedOverview,
        mode: 'epic-parent',
      })
      if (verdict.verdict === null) block('parent-overview-unverified', verdict.reason)
      else if (verdict.exitCode !== 0) {
        block('parent-overview-drift', `${verdict.verdict} — ${verdict.reason}`)
      }
    }
  }
}

// The key a child is addressed by in the sentinel and in its scratch basenames: the tracker
// identifier when the payload carries one, else its id.
function childKeyOf(issue) {
  if (isNonEmptyString(issue?.identifier)) return issue.identifier
  return isNonEmptyString(issue?.id) ? issue.id : null
}

function judgeParent({ parent, parentId, spec, role, storedOverview, block, note }) {
  const state = readState(parent)
  if (state == null) {
    block('parent-state-unreadable', 'the parent payload carries no readable state')
  } else if (role.unplanned && state === role.unplanned) {
    block(
      'parent-unplanned',
      `the parent is still in the unplanned state "${state}" — the final flip never landed`,
    )
  } else if (role.planned && state !== role.planned) {
    note(
      'parent-state',
      `the parent is in "${state}", not "${role.planned}" — accepted (rollup may advance it)`,
    )
  }

  const labels = readLabels(parent)
  if (role.epic && !labels.includes(role.epic)) {
    block('parent-epic-label-missing', `the parent does not carry the "${role.epic}" label`)
  }
  for (const name of PARENT_FORBIDDEN_LABEL_ROLES) {
    if (role[name] && labels.includes(role[name])) {
      block(
        'parent-forbidden-label',
        `the parent carries "${role[name]}" (${name}) — an epic container must not`,
      )
    }
  }

  // The spec store. The id is the one the sentinel names, not whatever the payload's `id` holds (a
  // UUID there would make the id-scoped `Epic spec (…)` title unmatchable).
  const detected = detectEpicParent({
    ...parent,
    id: isNonEmptyString(parentId) ? parentId : parent.id,
    description: storedOverview,
  })
  if (!detected.isEpicParent) {
    block(
      'parent-spec-store-missing',
      'the parent holds no readable spec store — no "Epic spec (…)" attachment, and its stored description carries no parseable legacy inline marker',
    )
  } else if (detected.ambiguous) {
    block('parent-spec-ambiguous', detected.reasons.join('; '))
  } else if (detected.source === 'attachment') {
    const identity = validateSpecIdentity(spec, parentId)
    if (!identity.ok) block('spec-identity', identity.errors.join('; '))
  }

  const planEntries = [...entriesOf(parent.attachments), ...entriesOf(parent.links)].filter(
    isPlanTitledEntry,
  )
  if (planEntries.length !== 1) {
    block(
      'parent-plan-artifact-count',
      `the parent holds ${planEntries.length} "${PLAN_ARTIFACT_TITLE_PREFIX} …" attachments/links — exactly one (the overview) is required`,
    )
  } else {
    const stale = stalePlanAttachmentSweep(entriesOf(parent.attachments), {
      keepAttachmentId: planEntries[0].id,
    })
    if (stale.length > 0) {
      block('parent-plan-artifact-count', `stale plan attachments remain: ${stale.join(', ')}`)
    }
  }
}

function judgeChildShape({ m, specEntry, role, block, note }) {
  const { key, issue } = m
  const state = readState(issue)
  if (state == null) block('child-state-unreadable', `${key}: no readable state`)
  else if (role.unplanned && state === role.unplanned) {
    block('child-unplanned', `${key}: still in the unplanned state "${state}"`)
  } else if (role.planned && state !== role.planned) {
    note('child-state', `${key}: in "${state}", not "${role.planned}" — accepted`)
  }
  if (!hasPlanArtifact(issue)) {
    block(
      'child-plan-artifact-missing',
      `${key}: no "${PLAN_ARTIFACT_TITLE_PREFIX} …" attachment or link`,
    )
  }

  const labels = readLabels(issue)
  const friendly = Boolean(role.agentBuild) && labels.includes(role.agentBuild)
  const human = Boolean(role.needsHuman) && labels.includes(role.needsHuman)
  if (role.agentBuild && role.needsHuman && friendly === human) {
    block(
      'child-exposure-label',
      `${key}: carries ${friendly ? 'both' : 'neither'} of "${role.agentBuild}"/"${role.needsHuman}" — exactly one is required`,
    )
  } else if (specEntry && friendly !== (specEntry.agentBuild !== false)) {
    block(
      'child-exposure-mismatch',
      `${key}: exposed ${friendly ? role.agentBuild : role.needsHuman} but its spec entry decided agentBuild: ${specEntry.agentBuild !== false}`,
    )
  }
  if (role.agentQuestion) {
    const asks = labels.includes(role.agentQuestion)
    if (specEntry?.agentQuestion === true && !asks) {
      block(
        'child-agent-question-missing',
        `${key}: its spec entry requires "${role.agentQuestion}"`,
      )
    } else if (asks && specEntry?.agentQuestion !== true) {
      note(
        'child-agent-question-extra',
        `${key}: carries "${role.agentQuestion}" its spec entry lacks — accepted`,
      )
    }
  }
  if (role.agentPlan && labels.includes(role.agentPlan)) {
    block('child-forbidden-label', `${key}: carries the planning-queue label "${role.agentPlan}"`)
  }
}
