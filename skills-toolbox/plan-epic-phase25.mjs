// skills-toolbox/plan-epic-phase25.mjs
//
// The deterministic core of boss-plan Phase 2.5 — the epic-parent preconditions
// and the first-write sequence of step 4. Everything here is PURE:
// each function inspects caller-supplied payloads and returns a decision or an
// ordered write plan; nothing reads a file, opens a socket, loads config, or
// throws. The caller executes.
//
// PROJECT-AGNOSTIC BY CONSTRUCTION. This module ships inside the published
// `boss-plan` core, which is installed into every user's global skill directory
// across thousands of unrelated repos. So it hard-codes no tracker name, no
// state role word and no label role word: `plannedState`, `unplannedState`,
// `epicLabel` and `labelsToStrip` are all caller-supplied, resolved once by the
// caller through the tracker adapter / skill config. In particular `EPIC_LABEL`
// is deliberately NOT imported — a gate that knew the literal label name would
// bake one tracker's vocabulary into the core.
//
// ---------------------------------------------------------------------------
// The operation-name partition
// ---------------------------------------------------------------------------
// `epicPhase25WritePlan` emits `op` names drawn from two disjoint groups. The
// split is not cosmetic: the test asserts it in both directions so an adapter
// rename breaks this plan loudly instead of emitting an op nothing can execute.
//
//   ADAPTER-BACKED — `moveState`, `preparePlanAttachment`,
//   `finalizePlanAttachment`, `deletePlanAttachment`. Every one is a member of
//   `REQUIRED_TRACKER_OPERATIONS ∪ OPTIONAL_TRACKER_OPERATIONS` in
//   `./tracker/adapter.mjs`. Note `moveState`'s adapter tool is `save_issue`:
//   the adapter vocabulary has NO dedicated label-write operation, so every
//   field/label mutation goes through that op. That is why the label strip in
//   stage 1 is a `moveState` whose args carry no state at all.
//
//   NON-ADAPTER — `putPlanAttachment` and `createChild`. These must NOT be
//   added to the adapter. `putPlanAttachment` is a real exported function in
//   `./plan-attachment.mjs` (the raw signed-URL HTTP PUT between prepare and
//   finalize); it is not an MCP tool. `createChild` has no adapter operation at
//   all — children are created through the `save_issue` MCP tool — and the name
//   is existing fake-tracker vocabulary from `plan-epic-lib.demo.mjs`.

import {
  parseEpicSpec,
  specAttachmentTitle,
  specAttachmentFilename,
  SPEC_ATTACHMENT_MIME,
  topoOrderChildren,
  epicChildMarker,
} from './plan-epic-lib.mjs'

// The legacy inline spec marker's PREFIX only — enough to tell "the description
// mentions the marker" from "the description carries a readable spec". The two
// full marker grammars (base64 and raw-JSON) live in plan-epic-lib.mjs and are
// applied by `parseEpicSpec`; this module never re-derives them, it only needs
// to know whether the string was mentioned at all so it can report the
// quoted-but-unparseable case distinctly.
const EPIC_SPEC_MARKER_MENTION_RE = /<!--\s*boss-plan-epic-spec:/

// A plan artifact — attachment or link — is identified by this title PREFIX and
// by nothing else. See `stalePlanAttachmentSweep` for why the prefix is the
// whole rule.
const PLAN_ARTIFACT_TITLE_PREFIX = 'Implementation plan'

// The three ops of the spec upload, in execution order. Exported nowhere: the
// acceptance criterion pins this module's export set to the four functions.
const SPEC_UPLOAD_OPS = ['preparePlanAttachment', 'putPlanAttachment', 'finalizePlanAttachment']

const isPlainObject = (value) =>
  typeof value === 'object' && value !== null && !Array.isArray(value)

const isNonEmptyString = (value) => typeof value === 'string' && value.trim() !== ''

const asArray = (value) => (Array.isArray(value) ? value : [])

// --- issue-shape normalization ----------------------------------------------
// A tracker returns an issue's state and labels in more than one shape, and the
// caller is told to feed this module the enumeration it already has rather than
// to pre-normalize. Reading one shape would make the gate's `'noop'` branch
// unreachable on real data — it would abort a perfectly conforming epic while
// naming a false cause for every conjunct. This is SHAPE polymorphism, not
// tracker vocabulary, so it stays project-agnostic; `bs-epic-lib.mjs` carries
// the same normalization for the same reason (it is not vendored here, so this
// module cannot borrow it).
//
//   state  — `{state: {name}}` (raw GraphQL), `{status}` (MCP), `{stateName}`
//            (already normalized), or a bare `{state: <name>}` string. The name
//            itself is never compared to a literal here — it is matched against
//            the caller-supplied `plannedState`.
//   labels — `{nodes: [{name}]}` (raw GraphQL), `[{name}]` (MCP), or bare
//            strings (already normalized).
const readState = (issue) => {
  if (!isPlainObject(issue)) return null
  const raw = isPlainObject(issue.state) ? issue.state.name : issue.state
  for (const candidate of [raw, issue.status, issue.stateName]) {
    if (isNonEmptyString(candidate)) return candidate
  }
  return null
}

const readLabels = (issue) => {
  if (!isPlainObject(issue)) return []
  const list = Array.isArray(issue.labels?.nodes)
    ? issue.labels.nodes
    : Array.isArray(issue.labels)
      ? issue.labels
      : []
  return list
    .map((label) => (typeof label === 'string' ? label : isPlainObject(label) ? label.name : null))
    .filter(isNonEmptyString)
}

/**
 * Decide whether a `get_issue`-shaped payload is an EXISTING epic parent, and
 * which store proved it. Never throws, whatever shape it is handed.
 *
 * The rule is STORE-SPECIFIC, and the asymmetry is the point:
 *
 *   ATTACHMENT store — PRESENCE decides, never parse success. An attachment
 *   titled exactly `specAttachmentTitle(issue.id)` makes this an epic parent
 *   even if its body is absent, truncated or garbage, because nothing but
 *   Phase 2.5 ever creates one. Requiring a successful parse here would let a
 *   half-written upload demote a real, already-decomposed epic parent to a
 *   plain ticket — and the single-ticket fallback would then re-plan a finished
 *   epic as a normal buildable ticket, on top of its own children.
 *
 *   DESCRIPTION store — the OPPOSITE. A description counts only when it both
 *   mentions the legacy inline marker AND `parseEpicSpec` actually returns a
 *   spec. A description is reporter-writable prose and a reporter can quote the
 *   marker string (boss-plan's own SKILL.md does, in the sentence stating this
 *   rule). Treating the quote as presence would classify a brand-new ticket as
 *   an unreadable epic parent and abort it loudly on every sweep, leaving it
 *   permanently unplannable by any automated run.
 *
 * The attachment store wins when both are present: it is the current store and
 * the legacy inline one is frozen and read-only.
 *
 * @param {{id?: string, description?: string, attachments?: object[]}} issue
 * @returns {{isEpicParent: boolean, source: ('attachment'|'inline'|null),
 *            specAttachmentId: (string|null), ambiguous: boolean, reasons: string[]}}
 */
export function detectEpicParent(issue) {
  const reasons = []
  const result = {
    isEpicParent: false,
    source: null,
    specAttachmentId: null,
    ambiguous: false,
    reasons,
  }

  if (!isPlainObject(issue)) {
    reasons.push('issue payload is not an object — neither spec store could be read')
    return result
  }

  const issueId = isNonEmptyString(issue.id) ? issue.id : null
  if (issueId == null) {
    reasons.push(
      'issue payload carries no usable id — the spec attachment title is id-scoped, so the attachment store could not be read',
    )
  }
  if (!Array.isArray(issue.attachments)) {
    reasons.push('issue payload carries no attachments array — the attachment store was empty')
  }

  // --- attachment store: presence decides -----------------------------------
  const wantedTitle = issueId == null ? null : specAttachmentTitle(issueId)
  const matches =
    wantedTitle == null
      ? []
      : asArray(issue.attachments).filter((a) => isPlainObject(a) && a.title === wantedTitle)

  if (matches.length > 1) {
    // Two current-titled specs: one of them is stale and there is no safe way to
    // tell which. Report the epic, withhold the id, let the caller abort.
    result.isEpicParent = true
    result.source = 'attachment'
    result.ambiguous = true
    reasons.push(
      `${matches.length} attachments carry the title "${wantedTitle}" — ambiguous spec store, never guess which is current`,
    )
    return result
  }

  if (matches.length === 1) {
    result.isEpicParent = true
    result.source = 'attachment'
    result.specAttachmentId = isNonEmptyString(matches[0].id) ? matches[0].id : null
    reasons.push(
      `an attachment titled "${wantedTitle}" is present — presence decides for the attachment store, the body is not parsed here`,
    )
    return result
  }

  // --- description store: parse success decides -----------------------------
  if (typeof issue.description !== 'string') {
    reasons.push('no "Epic spec (…)" attachment and no string description — not an epic parent')
    return result
  }

  if (!EPIC_SPEC_MARKER_MENTION_RE.test(issue.description)) {
    reasons.push(
      'no "Epic spec (…)" attachment and the description carries no legacy inline boss-plan-epic-spec marker — not an epic parent',
    )
    return result
  }

  let spec = null
  try {
    spec = parseEpicSpec(issue.description)
  } catch {
    // parseEpicSpec documents a no-throw contract; belt-and-braces so this
    // function's own no-throw contract does not depend on that one holding.
    spec = null
  }

  if (spec == null) {
    reasons.push(
      'the description QUOTES a legacy inline boss-plan-epic-spec marker but parseEpicSpec could not read a spec from it — a description is reporter-writable prose, so a quoted marker is not evidence and this is NOT treated as an epic parent',
    )
    return result
  }

  result.isEpicParent = true
  result.source = 'inline'
  reasons.push(
    'the description carries a legacy inline boss-plan-epic-spec marker that parseEpicSpec read successfully',
  )
  return result
}

/**
 * The unreadable-spec recovery gate: when a ticket is an epic parent whose spec
 * cannot be read (or whose attachment-sourced spec fails identity), decide
 * between "enumerate + no-op" and "abort loudly".
 *
 * `action` is ONLY EVER `'noop'` or `'abort'`. There is deliberately no third
 * value: falling through to the single-ticket path must not be expressible in
 * the return type, because a fall-through re-plans a finished or partial epic
 * as a normal buildable ticket — giving it an implementation-plan artifact and
 * an agent-friendly label, on top of children that already exist.
 *
 * `'noop'` requires ALL of: the parent sits in `plannedState`; the parent
 * carries `epicLabel`; there is at least one child; every child sits in
 * `plannedState`; every child has a plan artifact (an `attachments` OR `links`
 * entry whose title starts with `Implementation plan`). Anything else aborts,
 * and every failed conjunct is named separately — an operator reading the abort
 * log needs the whole picture, not the first thing that tripped.
 *
 * An unresolved `plannedState`/`epicLabel` aborts too: a caller that forgot to
 * resolve the role through its tracker adapter must not silently receive a
 * `'noop'` that skips the epic entirely. Fail closed.
 *
 * @param {{parent?: object, children?: object[], plannedState?: string, epicLabel?: string}} input
 * @returns {{action: ('noop'|'abort'), reasons: string[]}}
 */
export function epicSpecRecoveryGate(input) {
  const args = isPlainObject(input) ? input : {}
  const { parent, children, plannedState, epicLabel } = args
  const reasons = []

  const haveState = isNonEmptyString(plannedState)
  const haveLabel = isNonEmptyString(epicLabel)
  if (!haveState) {
    reasons.push(
      'plannedState was not resolved to a non-empty string — failing closed rather than skipping the epic',
    )
  }
  if (!haveLabel) {
    reasons.push(
      'epicLabel was not resolved to a non-empty string — failing closed rather than skipping the epic',
    )
  }

  if (!isPlainObject(parent)) {
    reasons.push('the parent issue payload is not an object — its state and labels are unknown')
  } else {
    // Only compare when the role resolved; otherwise every comparison would
    // fail and bury the real (already-recorded) cause in noise.
    if (haveState && readState(parent) !== plannedState) {
      reasons.push(
        `the parent is in state "${String(readState(parent))}", not the planned state "${plannedState}"`,
      )
    }
    if (haveLabel && !readLabels(parent).includes(epicLabel)) {
      reasons.push(`the parent does not carry the "${epicLabel}" label`)
    }
  }

  if (!Array.isArray(children)) {
    reasons.push('the enumerated children collection is not an array')
  }
  const list = asArray(children)
  if (list.length < 1) {
    reasons.push('the epic has zero enumerated children — nothing proves it was ever decomposed')
  }

  list.forEach((childIssue, index) => {
    const label =
      isPlainObject(childIssue) && isNonEmptyString(childIssue.id)
        ? childIssue.id
        : `child #${index + 1}`
    if (!isPlainObject(childIssue)) {
      reasons.push(`${label}: the child payload is not an object`)
      return
    }
    if (haveState && readState(childIssue) !== plannedState) {
      reasons.push(
        `${label}: in state "${String(readState(childIssue))}", not the planned state "${plannedState}"`,
      )
    }
    if (!hasPlanArtifact(childIssue)) {
      reasons.push(
        `${label}: has no "${PLAN_ARTIFACT_TITLE_PREFIX} …" attachment or link — no plan artifact`,
      )
    }
  })

  if (reasons.length > 0) return { action: 'abort', reasons }
  return {
    action: 'noop',
    reasons: [
      `the parent is planned, carries "${epicLabel}", and all ${list.length} children are planned with a plan artifact — enumerate and no-op`,
    ],
  }
}

/**
 * True when the issue carries a plan artifact in either collection. Tolerates
 * the same `{nodes: […]}` wrapping `readLabels` does, for the same reason.
 */
function hasPlanArtifact(issue) {
  const isPlanTitled = (entry) =>
    isPlainObject(entry) &&
    typeof entry.title === 'string' &&
    entry.title.startsWith(PLAN_ARTIFACT_TITLE_PREFIX)
  const collection = (value) => (Array.isArray(value?.nodes) ? value.nodes : asArray(value))
  return (
    collection(issue.attachments).some(isPlanTitled) || collection(issue.links).some(isPlanTitled)
  )
}

/**
 * The ids of stale implementation-plan attachments to delete, in input order,
 * excluding `keepAttachmentId`. Never throws on a non-array/absent collection.
 *
 * The predicate is PREFIX-SCOPED AND NOTHING ELSE: a `title` that is a string
 * starting with `Implementation plan`. It deliberately does not consult content
 * type, filename, or an "includes the issue id" fallback, and it is deliberately
 * NOT built on `selectImplementationPlanAttachment` from `./plan-attachment.mjs`
 * — whose Markdown fallback matches any attachment whose title merely INCLUDES
 * the issue id — and `Epic spec (<ISSUE-ID>)` includes it. That helper is
 * saved today only by the incidental fact that a spec attachment is declared
 * `application/json`; the day one is uploaded as `text/markdown`, reusing it
 * here would silently delete the epic's only spec, which is exactly the
 * destruction this function exists to prevent.
 *
 * @param {object[]} attachments  the issue's attachments, as returned by get_issue
 * @param {{keepAttachmentId?: string}} [options]  the attachment to preserve
 * @returns {string[]} attachment ids to delete, in input order
 */
export function stalePlanAttachmentSweep(attachments, options = {}) {
  const keepAttachmentId = isPlainObject(options) ? options.keepAttachmentId : undefined
  const stale = []
  for (const attachment of asArray(attachments)) {
    if (!isPlainObject(attachment)) continue
    if (typeof attachment.title !== 'string') continue
    if (!attachment.title.startsWith(PLAN_ARTIFACT_TITLE_PREFIX)) continue
    if (!isNonEmptyString(attachment.id)) continue
    if (attachment.id === keepAttachmentId) continue
    stale.push(attachment.id)
  }
  return stale
}

/**
 * The ordered tracker-write plan for Phase 2.5 step 4's FIRST-WRITE SEQUENCE.
 * Modelled on the `epicWiringPlan(spec, createdIdByKey)` precedent: this emits
 * a plan, the caller executes it — so the ordering is unit-testable without a
 * single tracker write.
 *
 * SCOPE BOUNDARY: steps 5-7 (intra-epic wiring, the parent commit, deferred
 * exposure, the planned flip) stay in prose and are NOT emitted here.
 *
 * Stages, strictly in this order:
 *
 *   1. `label-strip` — one `moveState` carrying `stripLabels` and NO state.
 *      A pure label strip: non-destructive, cheap, reversible, and sufficient on
 *      its own, because it breaks one conjunct of boss-build's
 *      planned + agent-friendly + plan-artifact selection. Emitted even when
 *      there is nothing to strip, so the sequence has a fixed shape.
 *   2. `spec-upload` — prepare, put, finalize.
 *   3. `stale-delete` — one `deletePlanAttachment` per stale id. THESE ARE THE
 *      DESTRUCTIVE OPS AND THEY COME STRICTLY AFTER EVERY STAGE-2 OP. Deleting
 *      before the spec upload succeeded would destroy the parent's only plan
 *      artifact with no restore path — a crash in that window leaves an epic
 *      parent with nothing at all.
 *   4. `create-children` — one `createChild` per child in `topoOrderChildren`
 *      order, carrying the identity and scheduling fields the spec actually
 *      persists: `key`, `title`, `estimate`, `priority` and the resume `marker`.
 *
 * `labelsToStrip` is PARENT-SCOPED — it is stage 1's `stripLabels` and nothing
 * else. This emitter deliberately does NOT emit a child `labels` field: a
 * child's label set is not derivable from the spec. `serializeEpicSpec` persists
 * only `agentFriendly`/`agentQuestion`, never a `labels` array, so any
 * subtraction performed here would run against a field that round-tripped spec
 * data never carries — a no-op dressed as a rule. Worse, emitting a computed
 * `labels` would invite a caller to treat it as complete and create every child
 * with an EMPTY label set, dropping the content labels and the `agent-question`
 * signal the skill mandates at creation. Child labels stay with the caller,
 * which is the only place that has them.
 *
 * Malformed input degrades rather than throwing, and degrades CLOSED on the two
 * fields whose absence would otherwise write somewhere unintended: an unusable
 * `parentId` yields an EMPTY plan (every op addresses the parent, so there is no
 * safe partial), and an unusable `unplannedState` yields zero `create-children`
 * while stages 1-3 stand (creating a child with no state lands it in the
 * tracker's default, which may be the PLANNED state — exposing an unplanned
 * shell to `boss-build`). A non-object spec, or one `topoOrderChildren` cannot
 * order (it throws on a cycle), likewise yields zero `create-children`.
 *
 * EACH ENTRY IS `{stage, op, args, runtimeArgs}`, and `args` is NOT a complete
 * adapter call payload — it is the statically-knowable subset, under the
 * adapter's OWN key names (`{id}` for `moveState`/`deletePlanAttachment`,
 * `{issue, …}` for the attachment ops), so an executor can spread it into the
 * call rather than translating. `runtimeArgs` names, per op, exactly what only
 * the executor can supply because it does not exist until the previous op has
 * run: `size` for the prepare, `file`/`uploadURL`/`headers` for the raw PUT,
 * `assetUrl` for the finalize. Naming them is the point — a plan that emitted
 * plausible-looking but wrong keys, or silently omitted the runtime ones, would
 * read as executable while being unusable, which is precisely the failure this
 * module exists to make impossible. A test cross-checks both halves against the
 * adapter's own operation summaries, so a rename or an added argument breaks
 * loudly here.
 *
 * @param {{parentId?: string, spec?: object, unplannedState?: string,
 *          staleAttachmentIds?: string[], labelsToStrip?: string[]}} input
 * @returns {Array<{stage: string, op: string, args: object, runtimeArgs: string[]}>}
 */
export function epicPhase25WritePlan(input) {
  const args = isPlainObject(input) ? input : {}
  // Fail closed: every emitted op addresses the parent, so an unresolved id has
  // no safe partial plan — emit nothing rather than ops aimed at "".
  if (!isNonEmptyString(args.parentId)) return []
  const parentId = args.parentId
  const labelsToStrip = asArray(args.labelsToStrip).filter(isNonEmptyString)
  const staleAttachmentIds = asArray(args.staleAttachmentIds).filter(isNonEmptyString)
  const plan = []

  // 1. Strip the exposure labels from the parent. No state field: this is a
  //    label write, and `moveState` (tool: `save_issue`) is the only adapter op
  //    that can carry one — the adapter has no dedicated label-write operation.
  //
  //    `stripLabels` sits OUTSIDE `args` on purpose. `save_issue` has no
  //    "remove these labels" argument: its `labels` REPLACES the whole set, so
  //    the value to send is `currentLabels − stripLabels`, and this module is
  //    never given `currentLabels`. Emitting a `removeLabels` key inside `args`
  //    would name a parameter the tool does not have — an executor spreading it
  //    into the call either errors or, if unknown keys are dropped, silently
  //    sends `{id}` alone, so the strip no-ops and the parent keeps its
  //    exposure label through the whole create→wire→expose window, which is the
  //    exact hole this stage exists to close. So: the executor reads the
  //    current set (`readLabels`), subtracts `stripLabels`, and sends the
  //    result as `labels`.
  plan.push({
    stage: 'label-strip',
    op: 'moveState',
    args: { id: parentId },
    stripLabels: [...labelsToStrip],
    runtimeArgs: [],
  })

  // 2. Upload the spec attachment. Each op carries ONLY its own statically
  //    known arguments, under the adapter's own key names; `runtimeArgs` names
  //    what the executor must add from the previous op's response.
  plan.push({
    stage: 'spec-upload',
    op: 'preparePlanAttachment',
    args: {
      issue: parentId,
      filename: specAttachmentFilename(),
      contentType: SPEC_ATTACHMENT_MIME,
    },
    runtimeArgs: ['size'],
  })
  plan.push({
    stage: 'spec-upload',
    op: 'putPlanAttachment',
    args: {},
    runtimeArgs: ['file', 'uploadURL', 'headers'],
  })
  plan.push({
    stage: 'spec-upload',
    op: 'finalizePlanAttachment',
    args: { issue: parentId, title: specAttachmentTitle(parentId) },
    runtimeArgs: ['assetUrl'],
  })

  // 3. Only now, with the spec safely stored, delete the stale plan artifacts.
  //    `delete_attachment` is keyed by the ATTACHMENT id alone — it takes no
  //    issue id.
  for (const attachmentId of staleAttachmentIds) {
    plan.push({
      stage: 'stale-delete',
      op: 'deletePlanAttachment',
      args: { id: attachmentId },
      runtimeArgs: [],
    })
  }

  // 4. Create the children in dependency order. An unresolved `unplannedState`
  //    fails closed here: a child created with no state lands in the tracker's
  //    default, which may be the planned one.
  let ordered = []
  try {
    ordered =
      isPlainObject(args.spec) && isNonEmptyString(args.unplannedState)
        ? topoOrderChildren(args.spec)
        : []
  } catch {
    ordered = []
  }
  for (const childNode of ordered) {
    if (!isPlainObject(childNode)) continue
    plan.push({
      stage: 'create-children',
      op: 'createChild',
      args: {
        parentId,
        state: args.unplannedState,
        key: childNode.key,
        title: childNode.title,
        estimate: childNode.estimate,
        priority: childNode.priority,
        marker: epicChildMarker(childNode.key),
      },
      runtimeArgs: [],
    })
  }

  return plan
}
