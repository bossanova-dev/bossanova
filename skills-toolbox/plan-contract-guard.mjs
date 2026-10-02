#!/usr/bin/env node

// plan-contract-guard — deterministic, LLM-untrusted guard that fails loud when the plan
// description a producer is about to write to the tracker does not actually satisfy the plan
// contract, or when the plan file about to be attached still carries drafting residue.
//
// The contract ("exactly these `##` sections, in this order, with a `Contract: v<N>` stamp") was
// until now aspirational at the producer: it was validated days later by the CONSUMER, long after a
// malformed artifact had already been published. Descriptions missing most of their required
// sections, descriptions that were a self-describing placeholder instead of markdown, unsubstituted
// angle-bracketed placeholder tokens, off-contract headings, and plan files ending in literal
// tool-call scaffolding all passed every gate that existed. This module is the missing mechanical
// enforcement, run immediately before the write.
//
// Two properties are deliberate and must survive later "simplification":
//
//   1. The placeholder scan is SCOPED to the drafter's own prose and stops at the terminal
//      `## Original notes` section. That block is reporter text required to survive byte-for-byte,
//      and reporter text legitimately quotes placeholder tokens — an unscoped scan would make a
//      plan ABOUT placeholders unpublishable.
//   2. The plan-file residue check is ANCHORED to the last non-blank line outside fenced code. A
//      plan that documents this very check names the scaffolding elements in prose and in fences;
//      an unanchored whole-file scan would reject its own specification.
//
// The section list is read through the skill-config seam (`loadSkillConfig` / `DEFAULT_CONFIG`), so
// a consuming repo widens the contract through its own config rather than by editing this file.
//
// Node builtins only — this runs in dependency-light cron worktrees.
//
// CLI: node plan-contract-guard.mjs --description <file> [--plan <file>]

import { readFileSync } from 'node:fs'
import { relative, resolve } from 'node:path'

// The citation resolver lives in its own module so a caller that needs ONLY that mechanic
// (bs-dispatch-claims.mjs, bs-review-triage.mjs) imports it alone instead of dragging this
// guard's whole closure into three published skill payloads. It is imported here, not
// re-exported: a second import path for one module is the drift R1 exists to close.
import { resolveCitationCoordinate } from './citation-coordinate.mjs'
import { createGateRecorder } from './gate-outcome.mjs'
import { isMainModule } from './main-module.mjs'
import { extractKeyChangeAreas } from './plan-deps-lib.mjs'
import {
  classifyCheckCommand,
  DEFAULT_CONFIG,
  hasCountAssertion,
  loadSkillConfig,
  markdownH2Heading,
  mergedListItems,
  parseAcceptanceCriteria,
  parsePremises,
  planDescriptionSections,
  planSectionsForDescriptionMode,
  scanFences,
  tokenizeSimpleShell,
  validatePlanDescription,
} from './skill-config.mjs'

// An unsubstituted template placeholder: an angle-bracketed SHOUTING token such as <ATTACHMENT-ID>
// or <ISSUE_ID>. Upper-case-anchored so lower-case prose (`<https://…>`, `<img>`) is not swept in.
// Note that the anchor alone is NOT sufficient: a single upper-case letter matches too, so `<T>`
// and `<N>` are indistinguishable from a placeholder by shape. Scoping, not the pattern, is what
// keeps legitimate prose publishable — see `placeholderResidue`.
const PLACEHOLDER = /<[A-Z][A-Z0-9_-]*>/

// A fenced-off inline code span. Quoting a token in backticks is how a drafter legitimately NAMES
// one — `- Contract: v<N>` is this contract's own notation, written that way in the skill body — so
// a span is blanked before the general placeholder scan. Requires a closing run on the same line,
// so an unbalanced backtick leaves the text scanned rather than hidden: this fails closed.
const INLINE_CODE_SPAN = /`+[^`\n]*`+/g

// The slot names THIS producer's own templates emit, and the only tokens scanned INSIDE inline code
// spans. That exception is load bearing: the Step 7 `## Planning` template emits
// `- Plan attachment: \`Implementation plan (<ISSUE-ID>)\`` and the backticks are PERMANENT — they
// survive substitution — so exempting spans wholesale would cancel the check on the single line
// this gate exists to protect.
//
// It is a NAMED list rather than a shape rule on purpose. Shape cannot separate a slot from
// documentation: `<PLAN_PATH>` (a real slot) and `<APPROVAL_POLICY>` (a CLI usage string a plan
// legitimately quotes) are the same shape, and a shape rule rejected roughly one plan in twenty in
// this repo's own corpus — permanently, since `placeholder-residue` has no config escape hatch.
// Naming the vocabulary keeps every real slot caught wherever it appears while leaving quoted
// documentation publishable. Outside a span, ANY `PLACEHOLDER` token is still residue.
//
// Widening this belongs behind the config seam the section list already uses; until a repo needs
// that, the producer and this list ship together and are edited together.
const TEMPLATE_SLOT_NAMES = [
  'ISSUE-ID',
  'ISSUE_ID',
  'ATTACHMENT-ID',
  'CHILD-ID',
  'PARENT',
  'BLOCKER-ID',
  'BLOCKED-ID',
  'UPSTREAM-BLOCKER-ID',
  'PLAN_PATH',
]
const TEMPLATE_SLOT = new RegExp(`<(?:${TEMPLATE_SLOT_NAMES.join('|')})>`)

// Harness tool-call scaffolding elements. A plan file whose final line is a bare closing tag for
// one of these was truncated out of a transcript rather than written as a plan body. An optional
// namespace prefix is accepted because the harness spells them both ways.
const SCAFFOLDING_ELEMENTS = ['invoke', 'parameter', 'content', 'function_calls']
const SCAFFOLDING_CLOSING_TAG = new RegExp(
  `^</\\s*(?:[A-Za-z][\\w.-]*:)?(?:${SCAFFOLDING_ELEMENTS.join('|')})\\s*>$`,
  'i',
)

// Minimum plausible size for a description: enough to catch an empty or placeholder field, small
// enough that a short summary plus short original notes passes.
const MIN_DESCRIPTION_BYTES = 60
const MIN_EPIC_PARENT_DESCRIPTION_BYTES = 60
const DESCRIPTION_MODES = new Set(['child-plan', 'epic-parent'])
const PREMISES_HEADING = '## Premises'
const ACCEPTANCE_HEADING = '## Acceptance criteria'
const KEY_CHANGES_HEADING = '## Key changes'
const RISKS_HEADING = '## Risks / unknowns'

// The sections whose `file:line` coordinates are resolved against the tree. `## Key changes` joined
// the list because a plan pins call sites there as often as it pins them in a criterion, and
// nothing was reading them. Widening is safe by CONSTRUCTION rather than by judgement — the
// citation pattern requires a `:<digits>` suffix, so a `## Key changes` bullet naming a file the
// ticket will CREATE is not a citation at all and cannot false-positive.
//
// `## Risks / unknowns` joined too: a fabricated symbol reached a published plan through
// a Risks line nothing read. It gets existence plus an OPPORTUNISTIC anchor rule
// (`stale-risk-citation`, see `riskAnchorTokens`); `## Key changes` stays existence-only, because it
// legitimately names symbols the PR will create beside the file it cites.
//
// SURVEYED before the Risks rule landed, against the authoring repo's whole archive of past plan
// bodies, each resolved against the tree at the first parent of the commit that ADDED the plan
// (HEAD already holds the symbols a plan announced, and would hide exactly the "names a symbol the
// PR will create" false-positive class). 879 documents carry `## Risks / unknowns`. Existence:
// 131 unresolvable Risks citations, 130 of them a bare basename the tree cannot resolve (the
// already-required repo-relative rule, not a new class) and 1 a line past EOF. Anchor rule: the
// first cut (any backticked identifier on the line, ±`ANCHOR_WINDOW`) raised 16 findings in 14
// documents and every finding was inspected — 12 were false positives, because a Risks bullet is a
// paragraph pairing several claims, so its identifiers describe other locations, name a qualified
// symbol, or name the declaration enclosing a cited body line. Narrowed to an ADJACENT anchor, the
// last segment of a qualified name, and a region widened to the enclosing column-0 declaration, it
// raises 3 findings in 2 documents: two coordinates that point into an unrelated function (true
// stale — re-checked at the PR's own base too), and one whose symbol the same PR's earlier commits
// had moved (it resolves at the PR base, so the drafting-time guard would not have raised it).
// Zero false positives remain. Re-run that survey before widening the rule.
//
// Deliberately NOT scanned: `## Summary`, `## Approach`, `## Testing` and every other section.
// Those are narrative, and a coordinate there is illustrative rather than load-bearing; scanning
// them would make the guard's blast radius the whole document for no measured defect.
// `## Original notes` stays out for the stronger reason that it is reporter text required to
// survive byte-for-byte — see the module header.
const CITATION_SECTIONS = [KEY_CHANGES_HEADING, PREMISES_HEADING, ACCEPTANCE_HEADING, RISKS_HEADING]

// How far either side of a cited line a premise anchor may have drifted and still count as
// resolved. Five lines absorbs the ordinary churn of an edit above the cited symbol without
// absorbing a wholesale extraction, which is the shape this rule exists to catch.
const ANCHOR_WINDOW = 5
const CITATION_PATTERN = String.raw`(?<![\w:/.-])((?:\.{1,2}\/)?(?:[\w.-]+\/)*[\w.-]+\.[A-Za-z0-9_-]+):(\d+)(?![\w.-])`
const CITATION = new RegExp(CITATION_PATTERN, 'g')
const CITATION_ONCE = new RegExp(CITATION_PATTERN)
const PR_BODY = /\b(?:PR|pull[- ]request)\s+body\b/i
const ORCHESTRATOR_OWNED = /\borchestrator-owned\b/i

function violation(code, message) {
  return { code, message: `plan-contract-guard: ${message}` }
}

/**
 * Every STATIC violation code this module can emit, sorted.
 *
 * The skill bodies enumerate these codes in prose, and a hand-maintained prose list is itself a
 * copied-forward claim that goes stale the moment a code is added — so the list is exported and
 * machine-checked rather than re-typed. The consuming repo's own gates assert that every entry
 * appears verbatim in the producer skill's pre-write gate prose and its drafting brief, and that
 * this array covers every `violation('<code>'` literal in this file.
 *
 * Deliberately NOT covered: the `couldNotEvaluate` codes (`citation-could-not-evaluate`), which are
 * a separate could-not-decide channel and are not violations; and `unreadable-input`, which the CLI
 * tags directly rather than through `violation()`. Both may still appear in the prose lists — the
 * coverage test asserts a subset relation, not equality.
 */
export const VIOLATION_CODES = [
  'duplicate-section',
  'line-spanning-emphasis',
  'merged-list-item',
  'missing-sections',
  'not-a-description',
  'placeholder-residue',
  'plan-file-residue',
  'pr-body-only-evidence',
  'premise-reused-as-criterion',
  'section-order',
  'self-falsified-literal-search',
  'stale-premise-citation',
  'stale-risk-citation',
  'subject-areas-unresolved',
  'unanchored-premise-citation',
  'unknown-section',
  'unmeasured-count-claim',
  'unresolvable-citation',
]

/**
 * The one violation family this module composes at runtime instead of pinning as a literal.
 *
 * `checkVerifyOnlyCommandVacuity` emits `vacuous-<kind>-command-<finding.code>`, where `kind` is
 * `criterion` or `premise` and `finding.code` comes from `classifyCheckCommand`'s blocking tier —
 * a set owned by another module and free to grow. Enumerating the cross-product here would be a
 * copied-forward claim of exactly the kind this export exists to retire, so the prose lists carry
 * the PREFIX and the residual is written down instead of pretended away.
 */
export const DYNAMIC_VIOLATION_CODE_PREFIXES = ['vacuous-*']

function couldNotEvaluate(code, message) {
  return { code, message: `plan-contract-guard: ${message}` }
}

function minimumDescriptionBytesForMode(mode) {
  return mode === 'epic-parent' ? MIN_EPIC_PARENT_DESCRIPTION_BYTES : MIN_DESCRIPTION_BYTES
}

/** Every line of `text` as `{ line, index }` (0-based), fences and all. */
function allLines(text) {
  return String(text ?? '')
    .split('\n')
    .map((line, index) => ({ line: line.replace(/\r$/, ''), index }))
}

/** Lines of `text` that are outside fenced code blocks, each as `{ line, index }` (0-based). */
export function linesOutsideFences(text) {
  return scanFences(text).lines
}

/**
 * True when `text` opens a fence it never closes. That is not a formatting nit here: it is the
 * shape a document TRUNCATED mid-code-block has, and it makes every following line invisible to
 * `linesOutsideFences` — including trailing residue. Callers that would otherwise trust the
 * filtered view must fall back to the raw text.
 */
export function hasUnterminatedFence(text) {
  return scanFences(text).unterminated
}

/**
 * The emitted `##` headings that ARE contract sections, in the order the description emits them.
 * Off-contract headings are excluded: `unknown-section` already reports those, and letting them
 * participate here would make one mistake trip two codes.
 */
export function emittedContractHeadings(config, description, { mode = 'child-plan' } = {}) {
  const recognised = new Set(planSectionsForDescriptionMode(config, mode).map((s) => s.heading))
  return planDescriptionSections(config, description, { mode })
    .map((s) => s.heading)
    .filter((heading) => recognised.has(heading))
}

/** True when `headings` appear in the same relative order as the contract's section list. */
export function isContractOrdered(config, headings, { mode = 'child-plan' } = {}) {
  const order = planSectionsForDescriptionMode(config, mode).map((s) => s.heading)
  let cursor = 0
  for (const heading of headings) {
    const at = order.indexOf(heading, cursor)
    if (at < 0) return false
    cursor = at + 1
  }
  return true
}

/**
 * Every placeholder token the drafter is responsible for: matches in any emitted heading, and in
 * the PROSE of every section BEFORE the terminal contract section.
 *
 * Three exclusions, each of which is a legitimate way to NAME a token rather than leave one
 * unsubstituted, and each of which this gate would otherwise reject with no config escape hatch:
 *
 *   1. The terminal section (`## Original notes`) body is verbatim reporter text — the original
 *      module-header guarantee.
 *   2. Fenced code blocks. A plan whose `## Key changes` shows the very shell block a skill runs
 *      (`NEW=".../<ISSUE-ID>.md"`) is documenting it, not emitting a placeholder — the identical
 *      reasoning that fence-scopes `planFileResidue` below.
 *   3. Inline code spans, for a ONE-SEGMENT metavariable only. `- Contract: v<N>` is this
 *      contract's own notation, written that way in the skill body and in the config docs, so a
 *      plan that describes the plan contract must stay publishable. A multi-segment TEMPLATE_SLOT
 *      is still caught inside a span, because the Step 7 template emits its slots inside permanent
 *      backticks and a blanket exemption would cancel this check exactly there.
 *
 * A BARE token in ordinary prose is still residue: at that point nothing distinguishes it from an
 * unsubstituted template slot, which is the defect this check exists for.
 */
export function placeholderResidue(config, description, { mode = 'child-plan' } = {}) {
  const sections = planDescriptionSections(config, description, { mode })
  const contractSections = planSectionsForDescriptionMode(config, mode)
  const terminalHeading = contractSections[contractSections.length - 1]?.heading
  const terminalIndex = sections.findIndex((s) => s.heading === terminalHeading)
  const scanned = terminalIndex < 0 ? sections : sections.slice(0, terminalIndex)
  const hits = []
  for (const section of sections) {
    const match = PLACEHOLDER.exec(section.heading)
    if (match) hits.push(match[0])
  }
  for (const section of scanned) {
    const body = section.bodyLines.join('\n')
    // Same unterminated-fence fallback `planFileResidue` uses, and for the same reason: an unclosed
    // fence hides every line after it, so a drafter who forgets a closing fence would blind the
    // scan for the rest of the section. Scanning the raw lines there is the fail-closed direction.
    const scan = scanFences(body)
    for (const { line } of scan.unterminated ? allLines(body) : scan.lines) {
      const prose = PLACEHOLDER.exec(line.replace(INLINE_CODE_SPAN, ' '))
      if (prose) hits.push(prose[0])
      const slot = TEMPLATE_SLOT.exec(line)
      if (slot) hits.push(slot[0])
    }
  }
  return [...new Set(hits)]
}

/**
 * The plan FILE's residue verdict: `null` when clean, otherwise a reason string. Empty is a
 * violation. Otherwise only the last non-blank line outside fenced code is inspected, so prose and
 * fenced examples that merely name the scaffolding elements pass.
 */
export function planFileResidue(plan) {
  if (String(plan ?? '').trim() === '') return 'plan file is empty'
  // An UNTERMINATED fence is the truncation shape this check exists to catch, and it hides every
  // following line from the filtered view — so a file truncated mid-fence with `</invoke>` appended
  // would read as clean. Fall back to the raw text there. It costs nothing on a well-formed plan: a
  // plan that legitimately ends inside a fence ends on the fence's own closing run, not on a tag.
  const scan = scanFences(plan)
  const candidates = (scan.unterminated ? allLines(plan) : scan.lines).filter(
    ({ line }) => line.trim() !== '',
  )
  const last = candidates[candidates.length - 1]
  if (!last) return 'plan file has no content outside fenced code blocks'
  const trimmed = last.line.trim()
  if (SCAFFOLDING_CLOSING_TAG.test(trimmed)) {
    return `plan file ends with tool-call scaffolding "${trimmed}" (line ${last.index + 1}) — the attachment must contain only the plan body`
  }
  return null
}

function sectionText(config, description, heading, mode) {
  const section = planDescriptionSections(config, description, { mode }).find(
    (s) => s.heading === heading,
  )
  return section ? section.bodyLines.join('\n') : ''
}

function scanCitationSections(config, description, mode) {
  return CITATION_SECTIONS.flatMap((heading) =>
    linesOutsideFences(sectionText(config, description, heading, mode)).map(({ line, index }) => ({
      heading,
      line,
      index,
    })),
  ).flatMap(({ heading, line, index }) => {
    const hits = []
    for (const match of line.matchAll(CITATION)) {
      hits.push({
        heading,
        citation: match[0],
        file: match[1],
        line: Number(match[2]),
        sectionLine: index + 1,
      })
    }
    return hits
  })
}

// The inline-code spans of a premise bullet that are candidate ANCHORS: a backticked token the
// drafter copied out of the cited location so the guard can confirm it is still there.
//
// The `— check:`/`— checked:` command is excluded by reading `premise.claim`, which the shared
// premise parse has already stripped it from — the exclusion is not re-derived by a second regex,
// so a change to the check-clause grammar cannot leave the two spellings disagreeing.
//
// A span that is itself a `file:line` coordinate is excluded too: a bullet whose only span is the
// coordinate it cites has named WHERE, never WHAT, so reporting it as unanchored is both the true
// diagnosis and the actionable one. Deliberately NOT excluded: any other span shape. A drafter who
// backticks an unrelated word gets a `stale-premise-citation` naming the token that failed, which
// is a readable, one-keystroke fix — not a silent pass.
function premiseAnchorSpans(premise) {
  const spans = []
  for (const match of premise.claim.matchAll(INLINE_CODE_SPAN)) {
    const token = match[0].replace(/^`+/, '').replace(/`+$/, '').trim()
    if (!token) continue
    if (CITATION_ONCE.test(token)) continue
    spans.push(token)
  }
  return spans
}

// Premise bullets paired with the coordinates they cite and the anchors they offer.
//
// Citations are read from `premise.claim` rather than the raw bullet so a `file:line` that appears
// only INSIDE the check command is not mistaken for a premise coordinate — the command is a recipe
// to re-run, not a claim about a location. That coordinate is still resolved for existence by
// `scanCitationSections`, which scans the section's raw lines.
function scanPremiseCitations(config, description) {
  const hits = []
  for (const premise of parsePremises(config, description)) {
    const anchors = premiseAnchorSpans(premise)
    for (const match of premise.claim.matchAll(CITATION)) {
      hits.push({
        anchors,
        citation: match[0],
        file: match[1],
        line: Number(match[2]),
      })
    }
  }
  return hits
}

// An identifier-shaped backticked token: a code identifier, optionally dotted and optionally
// followed by `()`. A token carrying `/` or ending in a short lowercase file extension is a PATH,
// never an anchor — a Risks line that backticks the file it cites has named WHERE, not WHAT.
const IDENTIFIER_TOKEN = /^[A-Za-z_$][\w$.]*(?:\(\))?$/
const FILE_EXTENSION_TAIL = /\.[a-z0-9]{1,5}$/
// What may sit BETWEEN an anchor and the citation it anchors: "`sym` at `f:1`", "`sym` (`f:1`)",
// "`sym`, `f:1`", "`f:1` is `sym()`", "`f:1` (`sym`)". Anything longer is a second clause, and a
// token there describes something other than the cited location.
const ANCHOR_BEFORE_GAP = /^\s*(?:\(|,|:|—|at|in)?\s*\(?\s*$/i
const ANCHOR_AFTER_GAP = /^\)?\s*(?:\(|,|:|—|=|is)?\s*\(?\s*$/i

function identifierAnchor(span) {
  const token = span.replace(/^`+/, '').replace(/`+$/, '').trim()
  if (!IDENTIFIER_TOKEN.test(token)) return null
  if (token.includes('/') || FILE_EXTENSION_TAIL.test(token)) return null
  // A qualified name (`client.BossClient.UpdateSession`) is how prose NAMES a symbol, not how the
  // source spells it, so it anchors on its last segment. The `()` goes for the same reason.
  return token.replace(/\(\)$/, '').split('.').pop() || null
}

// The anchors of one citation on a `## Risks / unknowns` line. Unlike a premise, a Risks line is
// not REQUIRED to carry an anchor, and a Risks bullet is routinely a paragraph pairing several
// claims — so only an identifier-shaped span ADJACENT to the citation (see the gap patterns) makes
// a claim about WHAT sits at that coordinate. A line with no adjacent anchor is existence-only.
function riskCitationAnchors(line) {
  const spans = [...line.matchAll(INLINE_CODE_SPAN)].map((match) => ({
    start: match.index,
    end: match.index + match[0].length,
    text: match[0],
  }))
  const hits = []
  for (const match of line.matchAll(CITATION)) {
    const inside = spans.find((span) => span.start <= match.index && match.index < span.end)
    const start = inside ? inside.start : match.index
    const end = inside ? inside.end : match.index + match[0].length
    const before = spans.filter((span) => span.end <= start).pop()
    const after = spans.find((span) => span.start >= end)
    const anchors = []
    if (before && ANCHOR_BEFORE_GAP.test(line.slice(before.end, start))) {
      const anchor = identifierAnchor(before.text)
      if (anchor) anchors.push(anchor)
    }
    if (after && ANCHOR_AFTER_GAP.test(line.slice(end, after.start))) {
      const anchor = identifierAnchor(after.text)
      if (anchor) anchors.push(anchor)
    }
    if (anchors.length > 0) {
      hits.push({ anchors, citation: match[0], file: match[1], line: Number(match[2]) })
    }
  }
  return hits
}

// `## Risks / unknowns` citations that carry an adjacent identifier anchor.
function scanRiskCitations(config, description, mode) {
  return linesOutsideFences(sectionText(config, description, RISKS_HEADING, mode)).flatMap(
    ({ line }) => riskCitationAnchors(line),
  )
}

function fixedStringNeedle(command) {
  const tokens = tokenizeSimpleShell(command)
  const commandIndex = tokens.findIndex((token) => token === 'rg' || token === 'grep')
  if (commandIndex < 0) return null
  let fixed = false
  for (let index = commandIndex + 1; index < tokens.length; index += 1) {
    const token = tokens[index]
    if (token === '--') return fixed ? (tokens[index + 1] ?? null) : null
    if (token === '-e' || token === '--regexp') {
      index += 1
      continue
    }
    if (token === '-F' || token === '--fixed-strings' || token === '--fixed-regexp') {
      fixed = true
      continue
    }
    if (/^-[A-Za-z]+$/.test(token) && token.includes('F')) {
      fixed = true
      continue
    }
    if (token.startsWith('-')) continue
    return fixed ? token : null
  }
  return null
}

/** The cited line plus `ANCHOR_WINDOW` lines either side, joined. `line` is 1-based. */
function anchorWindow(lines, line) {
  return lines.slice(Math.max(0, line - 1 - ANCHOR_WINDOW), line + ANCHOR_WINDOW).join('\n')
}

const COLUMN_ZERO_COMMENT = /^(?:\/\/|\/\*|\*|#|--)/
const COLUMN_ZERO_CLOSER = /^[}\])]/
const ENCLOSING_LOOKBACK = 400

/**
 * The lines from the cited line up to the column-0 declaration that encloses it, inclusive, with
 * any column-0 comment block met on the way. A Risks line routinely cites a line INSIDE a symbol —
 * the `domains` list of a resource, the advisory push inside a function, a paragraph of the
 * symbol's own doc comment — and names the symbol, which sits more than `ANCHOR_WINDOW` lines up.
 * Stops at a column-0 closer (the cited line is past that block) and after `ENCLOSING_LOOKBACK`.
 */
function enclosingDeclaration(lines, line) {
  const region = []
  for (let index = line - 1; index >= 0 && index >= line - ENCLOSING_LOOKBACK; index -= 1) {
    const text = lines[index]
    if (text.trim() === '' || /^\s/.test(text) || COLUMN_ZERO_COMMENT.test(text)) {
      region.push(text)
      continue
    }
    if (COLUMN_ZERO_CLOSER.test(text)) break
    region.push(text)
    break
  }
  return region.join('\n')
}

export function checkPlanCitations(
  config,
  description,
  { cwd = undefined, fs = { readFileSync }, mode = 'child-plan' } = {},
) {
  const violations = []
  const couldNotEvaluateItems = []
  let root
  try {
    root = cwd ?? process.cwd()
  } catch (error) {
    couldNotEvaluateItems.push(
      couldNotEvaluate(
        'citation-could-not-evaluate',
        `citation check could not resolve the working tree root: ${error.message}`,
      ),
    )
    return { violations, couldNotEvaluate: couldNotEvaluateItems }
  }
  if (typeof root !== 'string' || root.length === 0) {
    couldNotEvaluateItems.push(
      couldNotEvaluate(
        'citation-could-not-evaluate',
        'citation check could not resolve the working tree root',
      ),
    )
    return { violations, couldNotEvaluate: couldNotEvaluateItems }
  }
  // One read per cited file, shared by the existence pass and the anchor pass below. A plan that
  // cites the same file from several bullets is the normal shape, not the exception.
  const bodyCache = new Map()
  const readBody = (resolved) => {
    if (!bodyCache.has(resolved)) {
      try {
        bodyCache.set(resolved, { body: fs.readFileSync(resolved, 'utf8'), error: null })
      } catch (error) {
        bodyCache.set(resolved, { body: null, error })
      }
    }
    return bodyCache.get(resolved)
  }
  // The cited file's lines when the coordinate RESOLVED (file readable, line in 1..length), else
  // null. Both anchor passes below skip on null because the existence pass has already reported
  // that coordinate — one defect, one violation.
  const resolvedCitationLines = (hit) => {
    const resolved = resolveCitationCoordinate(root, hit.file, hit.line, { readBody })
    return resolved.ok ? resolved.body.split('\n') : null
  }

  for (const hit of scanCitationSections(config, description, mode)) {
    const resolved = resolveCitationCoordinate(root, hit.file, hit.line, { readBody })
    if (resolved.ok) continue
    const detail =
      resolved.code === 'escapes-root'
        ? 'escapes the working tree root'
        : resolved.code === 'bad-line'
          ? `has no line ${hit.line}`
          : resolved.code === 'unreadable'
            ? `could not be read: ${resolved.error.message}`
            : `has only ${resolved.lineCount} line(s)`
    // A `## Premises` coordinate is often stale ON PURPOSE: a premise warning that a ticket's
    // coordinates have rotted quotes the rotted `name:number`. There is no heuristic that tells
    // that apart from a fabrication, so the message names the two spellings that are not a citation.
    const remedy =
      hit.heading === PREMISES_HEADING
        ? ` — if the coordinate is quoted rather than cited, cite ${hit.file} without a line ` +
          `number, or spell it as prose ("line ${hit.line} of ${hit.file}")`
        : ''
    violations.push(
      violation(
        'unresolvable-citation',
        `${hit.heading} cites ${hit.citation}, but ${hit.file} ${detail}${remedy}`,
      ),
    )
  }

  // The premise ANCHOR pass. `## Premises` is the one section a drafter writes deliberately to
  // declare "these are the facts to re-check", so it is the one section where a coordinate must
  // resolve on CONTENT rather than on the file merely being long enough — the existence check
  // stays green after the code at the cited line has been extracted away, which is the whole
  // defect. `## Key changes` and `## Acceptance criteria` keep existence-only.
  //
  // Deliberately NOT checked here: whether the anchor is the RIGHT token for the claim (undecidable
  // from text), whether it appears exactly once in the window (a repeated token is still evidence
  // the region survived), and whether the claim the premise makes about that location is true (the
  // guard reads bytes, not meaning). A coordinate whose file is missing, unreadable or too short is
  // skipped here because the pass above has already reported it — one defect, one violation.
  //
  // MEASURED at introduction, against the authoring repo's whole archive of past plan bodies: of
  // 168 documents carrying a `## Premises` section, 8 raised 12 findings (11 stale, 1 unanchored).
  // Each was inspected: every one named a coordinate whose cited symbol had genuinely moved since
  // the document was written, and NO case was found where the anchor still sat at the cited
  // location. Four of the twelve were in the very document that specified this rule, whose own
  // coordinates its own implementation invalidated — which is the defect, caught. The archive is a
  // proxy rather than a live corpus: this guard has no consumer-side caller and runs only on a
  // description about to be authored, so a tightened rule cannot retroactively reject a published
  // plan. Re-run that survey before widening the rule, and argue against the new numbers.
  for (const hit of scanPremiseCitations(config, description)) {
    const lines = resolvedCitationLines(hit)
    if (!lines) continue
    if (hit.anchors.length === 0) {
      violations.push(
        violation(
          'unanchored-premise-citation',
          `${PREMISES_HEADING} cites ${hit.citation} but carries no anchor token: add a backticked ` +
            `token copied from that location, or cite ${hit.file} without a line number`,
        ),
      )
      continue
    }
    if (hit.anchors.some((anchor) => anchorWindow(lines, hit.line).includes(anchor))) continue
    violations.push(
      violation(
        'stale-premise-citation',
        `${PREMISES_HEADING} cites ${hit.citation}, but no line within ${ANCHOR_WINDOW} of line ` +
          `${hit.line} in ${hit.file} contains its anchor ` +
          `${hit.anchors.map((anchor) => `"${anchor}"`).join(' or ')}`,
      ),
    )
  }

  // The Risks anchor pass: the same window test, run only for coordinates that already resolved
  // (file readable, line in range) so a nonexistent line raises `unresolvable-citation` alone.
  for (const hit of scanRiskCitations(config, description, mode)) {
    const lines = resolvedCitationLines(hit)
    if (!lines) continue
    const region = `${anchorWindow(lines, hit.line)}\n${enclosingDeclaration(lines, hit.line)}`
    if (hit.anchors.some((anchor) => region.includes(anchor))) continue
    violations.push(
      violation(
        'stale-risk-citation',
        `${RISKS_HEADING} cites ${hit.citation} beside ` +
          `${hit.anchors.map((anchor) => `"${anchor}"`).join(', ')}, but no line within ` +
          `${ANCHOR_WINDOW} of line ${hit.line} in ${hit.file}, nor its enclosing declaration, ` +
          `contains any of them — re-read the ` +
          `location and fix the coordinate or the symbol, or drop the line number`,
      ),
    )
  }
  return { violations, couldNotEvaluate: couldNotEvaluateItems }
}

// An asterisk emphasis delimiter run. Underscore runs are deliberately EXCLUDED: `some_var_name`
// and `__init__` are ordinary prose in a plan that names code, and a lint that pairs those runs
// across a hard wrap would reject correct prose. The observed corrupting shape is `**…**`.
const EMPHASIS_RUN = /\*+/g

/**
 * Replace every inline code span with a same-length run of `!`, preserving column offsets.
 *
 * NOT spaces: a delimiter's neighbour in the source is a backtick, which CommonMark counts as
 * punctuation, never whitespace. A space mask made the `**` closing `**run the \`x\`**` look
 * whitespace-preceded, so it could not close, and the next span's closer paired with it one line
 * early. Any non-`*` ASCII punctuation keeps the flanking a renderer would compute.
 */
function maskInlineCodeSpans(line) {
  return line.replace(INLINE_CODE_SPAN, (span) => '!'.repeat(span.length))
}

/** Blank out a leading unordered-list marker so `* item` is not read as an emphasis delimiter. */
function maskListMarker(line) {
  return line.replace(/^([ \t]*)[-*+]([ \t])/, (_match, indent, space) => `${indent} ${space}`)
}

/**
 * Pair the emphasis delimiter runs of ONE paragraph and return the opening run of every pair whose
 * delimiters sit on different lines.
 *
 * Runs are classified by CommonMark's flanking idea rather than by shape alone: an opener must be
 * followed by non-whitespace and a closer preceded by non-whitespace. That is what keeps `2 * 3` on
 * one line and `4 * 5` on the next from pairing into a phantom span — the single most likely
 * over-detection in a plan that does arithmetic in prose. Runs pair only with runs of the SAME
 * length, and a leftover unpaired run is ignored rather than guessed at.
 */
function lineSpanningEmphasisInParagraph(lines) {
  const runs = []
  for (const { index, masked } of lines) {
    for (const match of masked.matchAll(EMPHASIS_RUN)) {
      const start = match.index
      const previous = masked.charAt(start - 1)
      const next = masked.charAt(start + match[0].length)
      runs.push({
        index,
        column: start + 1,
        length: match[0].length,
        canOpen: next !== '' && !/\s/.test(next),
        canClose: previous !== '' && !/\s/.test(previous),
      })
    }
  }
  const open = new Map()
  const findings = []
  for (const run of runs) {
    const pending = open.get(run.length)
    if (pending && run.canClose) {
      open.delete(run.length)
      if (pending.index !== run.index) findings.push(pending)
      continue
    }
    if (!pending && run.canOpen) open.set(run.length, run)
  }
  return findings
}

/**
 * Every emphasis span in an AGENT-AUTHORED section whose opening and closing delimiters sit on
 * different lines.
 *
 * A tracker's markdown normalizer can close such a span at the line break and store the remainder
 * as a literal delimiter run — one observed description holds `**7 of the 17****\n****already
 * fixed**` where the drafter wrote one span across a hard wrap at this repo's ~100-column prose
 * convention. That silently damages the only surviving copy of the description, so it is worth
 * catching before the write.
 *
 * Two exclusions are load-bearing. The terminal verbatim section is copied, not composed, and
 * rewrapping it to please a normalizer is exactly the corruption the verbatim gates exist to
 * prevent. Fenced blocks and inline code spans are not prose at all.
 *
 * The detector is deliberately NARROW — a sibling ticket owns the broader gates-reject-legitimate-
 * shapes problem, so the false-positive budget here is effectively zero and under-detection is
 * preferred to blocking correct prose.
 *
 * @returns {{ line: number, column: number }[]} 1-based positions of each opening delimiter.
 */
export function lineSpanningEmphasis(config, description, { mode = 'child-plan' } = {}) {
  const text = String(description ?? '')
  const rawLines = text.split('\n').map((line) => line.replace(/\r$/, ''))
  const outside = new Set(scanFences(text).lines.map(({ index }) => index))
  const contractSections = planSectionsForDescriptionMode(config, mode)
  const terminalHeading = contractSections[contractSections.length - 1]?.heading

  let limit = rawLines.length
  for (let index = 0; index < rawLines.length; index += 1) {
    if (!outside.has(index)) continue
    if (terminalHeading && markdownH2Heading(rawLines[index]) === terminalHeading) {
      limit = index
      break
    }
  }

  const findings = []
  let paragraph = []
  const flush = () => {
    if (paragraph.length > 0) findings.push(...lineSpanningEmphasisInParagraph(paragraph))
    paragraph = []
  }
  for (let index = 0; index < limit; index += 1) {
    if (!outside.has(index) || rawLines[index].trim() === '') {
      flush()
      continue
    }
    paragraph.push({ index, masked: maskListMarker(maskInlineCodeSpans(rawLines[index])) })
  }
  flush()
  return findings.map(({ index, column }) => ({ line: index + 1, column }))
}

export function checkLineSpanningEmphasis(config, description, { mode = 'child-plan' } = {}) {
  return lineSpanningEmphasis(config, description, { mode }).map(({ line, column }) =>
    violation(
      'line-spanning-emphasis',
      `emphasis span opened at line ${line}, column ${column} is closed on a later line — a tracker's markdown normalizer can close it at the line break and store the rest as a literal delimiter run; keep the span on one line`,
    ),
  )
}

export function checkSelfFalsifiedLiteralSearch(config, description) {
  const violations = []
  for (const criterion of parseAcceptanceCriteria(config, description)) {
    if (!criterion.check) continue
    const needle = fixedStringNeedle(criterion.check)
    if (!needle) continue
    const elsewhere = description.replace(criterion.text, '')
    if (!elsewhere.includes(needle)) continue
    violations.push(
      violation(
        'self-falsified-literal-search',
        `criterion "${criterion.text}" searches for "${needle}", but the plan itself also mandates that string`,
      ),
    )
  }
  return violations
}

export function checkPrBodyOnlyEvidence(config, description) {
  return parseAcceptanceCriteria(config, description)
    .filter((criterion) => {
      if (criterion.verifyOnly || ORCHESTRATOR_OWNED.test(criterion.text)) return false
      if (!(PR_BODY.test(criterion.text) || (criterion.check && PR_BODY.test(criterion.check))))
        return false
      return !CITATION_ONCE.test(criterion.text)
    })
    .map((criterion) =>
      violation(
        'pr-body-only-evidence',
        `criterion "${criterion.text}" names the pull-request body as its only evidence location`,
      ),
    )
}

export function checkVerifyOnlyCommandVacuity(config, description, opts = {}) {
  const violations = []
  const advisories = []
  // A check command naming a file this plan CREATES is absent today by design. The classifier is
  // context-free and cannot know that; this guard holds the plan, so it drops `path-operand-absent`
  // for an operand its own `## Key changes` names. Accepted residual: a path mistyped identically
  // in both places gets no signal — the finding was advisory anyway.
  const keyChanges = sectionText(config, description, KEY_CHANGES_HEADING, 'child-plan')
  const plannedPath = (path) =>
    typeof path === 'string' && keyChanges.includes(path.replace(/^\.\//, ''))
  const checkItem = (kind, item) => {
    if (!item.check) return
    const classified = classifyCheckCommand(item.check, { ...opts, kind })
    for (const finding of classified.blocking) {
      violations.push(
        violation(
          `vacuous-${kind}-command-${finding.code}`,
          `${kind} "${item.text}" names an unresolvable check command: ${finding.message}`,
        ),
      )
    }
    for (const finding of classified.advisory) {
      if (finding.code === 'path-operand-absent' && plannedPath(finding.path)) continue
      advisories.push({
        code: `advisory: ${finding.code}`,
        message: `plan-contract-guard: ${kind} "${item.text}" has an advisory check command risk: ${finding.message}`,
      })
    }
  }
  for (const criterion of parseAcceptanceCriteria(config, description)) {
    checkItem('criterion', criterion)
  }
  for (const premise of parsePremises(config, description)) {
    checkItem('premise', premise)
  }
  return { violations, advisories }
}

// Words that end in `s` but are not the countable noun of a quantity claim. Without this, a bare
// `\d+ \w+s` rule reads "the 3 checks pass" and "line 1939 is stale" as the same shape. Kept SMALL
// and verb-shaped on purpose: the discriminator that does the real work is adjacency plus the
// code-span strip, not an open-ended dictionary nobody can keep current.
const NOT_A_COUNTED_NOUN = new Set([
  'across',
  'always',
  'as',
  'does',
  'exists',
  'gives',
  'goes',
  'has',
  'is',
  'its',
  'less',
  'makes',
  'means',
  'plus',
  'remains',
  'returns',
  'says',
  'stays',
  'this',
  'thus',
  'versus',
  'was',
  'yes',
])

/**
 * The narrowest reading of "this claim asserts a number somebody must have measured": a bare
 * cardinal IMMEDIATELY followed by a plural noun — `6 entries`, `170 bytes`, `11 incidents`.
 *
 * Three deliberate narrowings keep the false-positive surface small, which is the whole risk this
 * check carries:
 *
 *   1. Inline code spans are stripped first. A number inside backticks is a coordinate, a version,
 *      an identifier or a literal (`skill-config.mjs:2374`, `Contract: v1`, a ticket id) — never a
 *      prose count. This single strip removes most of the surface.
 *   2. The cardinal must not be preceded by a character that makes it part of a token: `:` (a line
 *      locator), `-` (an identifier suffix), `.` (a version or decimal), `#` (an issue number),
 *      `v` (a version), or a word character.
 *   3. Adjacency is required — one run of spaces, no intervening adjective — and the noun must be
 *      plural and not one of the verb-shaped words above. A cardinal introduced by a structural
 *      noun (`Step 4 decides`, `finding 8 needs`, `Phase 3 documents`) is excluded outright: that
 *      is a locator followed by a verb, and it was the largest single false-positive class the
 *      first falsification pass over an archive of past plan bodies raised, which is why this
 *      lookbehind exists. No figure is quoted here because nothing re-measures one.
 *
 * Spelled-out numbers ("three entries") are deliberately NOT matched. A word-number detector has a
 * far larger false-positive surface and no way to tell "three entries" from "three of them"; the
 * drafting rule covers that half in prose.
 */
export function assertedNumericQuantity(claim) {
  const prose = String(claim ?? '').replace(/`[^`]*`/g, ' ')
  for (const match of prose.matchAll(
    /(?<!\b(?:step|phase|stage|tier|round|finding|item|note|section|part|rule|figure|table|version|level|option|column|line)\s)(?<![\w:.#v-])(\d+)\s+([a-z][a-z-]*s)\b/gi,
  )) {
    if (!NOT_A_COUNTED_NOUN.has(match[2].toLowerCase())) return `${match[1]} ${match[2]}`
  }
  return null
}

/** The claim half of a parsed item — everything before its ` — check: ` clause. */
function claimText(item) {
  if (typeof item?.claim === 'string') return item.claim
  return String(item?.text ?? '').split(/\s+—\s+check(?:ed)?:/)[0]
}

/**
 * A number the plan states that nothing re-measures.
 *
 * Plan prose is read downstream as fact: a regression test written from a plan encodes the count the
 * plan stated, so a count nobody measured ships as a durably wrong artifact. A quantity claim must
 * therefore name a check command that RE-MEASURES it, and "re-measures" is `hasCountAssertion` from
 * `skill-config.mjs` — the tree's existing definition, imported rather than restated, so the two
 * cannot drift.
 *
 * A claim with no check command at all is NOT reported here: that is `checkPlanCitations`' subject,
 * and one defect must not trip two codes.
 */
export function checkUnmeasuredCountClaim(config, description) {
  const violations = []
  const inspect = (kind, item) => {
    if (!item.check) return
    const quantity = assertedNumericQuantity(claimText(item))
    if (!quantity) return
    if (hasCountAssertion(item.check)) return
    violations.push(
      violation(
        'unmeasured-count-claim',
        `${kind} "${claimText(item)}" asserts "${quantity}", but its check command re-measures nothing — record a command that counts (grep -c, rg -c, wc -l, grep -q, rg -q, an explicit count or assertion), or drop the number`,
      ),
    )
  }
  for (const criterion of parseAcceptanceCriteria(config, description))
    inspect('criterion', criterion)
  for (const premise of parsePremises(config, description)) inspect('premise', premise)
  violations.push(...unmeasuredNarrativeByteFigures(config, description))
  return violations
}

// A `<digits> bytes` figure, thousands separators included so `58,499 bytes` is one figure and not
// `499 bytes`. The lookbehind is the same token-boundary idea `assertedNumericQuantity` uses.
const BYTE_FIGURE = /(?<![\w:.#+\-−/v,])(\d{1,3}(?:[, ]\d{3})+|\d+)\s+bytes\b/g
// Figures written as a TARGET, LIMIT, DELTA or ESTIMATE rather than a measurement of the tree: a cap
// ("capped at 512 bytes", "≤ 400 bytes"), a comparison ("longer than 80 bytes"), a change ("frees
// 190 bytes", "by 81, 80 and 82 bytes"), an approximation ("~250 bytes"). The number run between
// the keyword and the figure lets one keyword govern a list of figures.
const BYTE_FIGURE_BEFORE =
  /(?:\b(?:cap(?:ped|s)?|limit(?:ed|s)?)(?:\s+[\w-]+){0,3}\s+(?:at|to)|\b(?:capped|caps?|limit(?:ed)?|max(?:imum)?|at most|no more than|up to|under|below|above|over|within|longer than|shorter than|more than|less than|fewer than|greater than|exceeds?|exceeding|by|frees?|freed|trims?|trimmed|saves?|saved|shrinks?|grows?|adds?|added|costs?|cuts?|about|around|roughly|approximately|nearly|near|headroom(?:\s+(?:is|was|of|therefore|now|only))*)\b|[~≈≤≥<>±+−-])(?:\s*(?:at|to|of|near|by))?[\s*(]*(?:[\d,.]+\s*(?:,|and|or)?\s*)*$/i
// ...or followed by what makes it a margin rather than a size ("98 bytes of headroom", "30 bytes
// away", "5 bytes under it").
const BYTE_FIGURE_AFTER =
  /^\**\s*(?:(?:of\s+)?(?:[\w-]+\s+)?(?:headroom|budget|slack|margin|room|leeway)|(?:remain(?:s|ing)?\s+)?(?:below|under|above|over)|away|short|separate|spare|left|remaining|free|less|more|smaller|larger|bigger)\b/i
// A single-digit figure is an encoding fact ("`…` is 3 bytes in UTF-8"), not a measured file size.
const MIN_BYTE_FIGURE = 10
const withoutThousandsSeparators = (text) => text.replace(/(\d)[, ](?=\d{3}(?!\d))/g, '$1')

/**
 * A byte figure stated in NARRATIVE prose that nothing re-measures.
 *
 * A load-bearing size in `## Approach` ("the body is N bytes") was read downstream as fact while
 * only Premises and criteria rows were judged. Any pre-terminal contract section other than
 * `## Premises` and `## Acceptance criteria` is now scanned for the literal `<digits> bytes` shape,
 * code spans stripped, and each figure must recur in a `## Premises` bullet whose check counts
 * (`hasCountAssertion`). `## Original notes` is reporter text and is never scanned. Same code as
 * the row rule, `unmeasured-count-claim`: one defect class, one code.
 *
 * SURVEYED before landing, against the authoring repo's whole archive of past plan bodies (1291
 * documents). The first cut raised 76 findings in 42 documents; every one was inspected, and 44 were
 * not measurements at all — a cap or limit ("capped at 512 bytes", "≤ 400 bytes"), a delta or
 * margin ("frees roughly 190 bytes", "98 bytes of headroom", "3097 bytes remain below"), an estimate
 * ("~250 bytes"), an encoding fact ("3 bytes in UTF-8"), a blockquote quoting the stale claim a plan
 * corrects, or a thousands-separated size split in two. The before/after context patterns, the
 * single-digit floor, blockquote skipping and paragraph joining (so a margin word survives a hard
 * wrap) narrow those out. What remains is 32 findings in 21 documents, and every one is a stated
 * current size ("is exactly 88851 bytes", "measures 145464 bytes") with no counting premise — most
 * predate `## Premises` itself. Zero false positives remain. Re-run that survey before widening it.
 */
/**
 * The prose paragraphs of a section body, each joined onto one line so a figure and the word that
 * makes it a margin survive a hard wrap between them. Fenced code is not prose, and a blockquote is
 * QUOTED text (a plan quoting the stale claim it corrects), so both are skipped.
 */
function narrativeParagraphs(body) {
  const paragraphs = []
  let current = []
  const flush = () => {
    if (current.length > 0) paragraphs.push(current.join(' '))
    current = []
  }
  for (const { line } of scanFences(body).lines) {
    if (line.trim() === '' || /^\s*>/.test(line)) {
      flush()
      continue
    }
    current.push(line.trim())
  }
  flush()
  return paragraphs
}

function unmeasuredNarrativeByteFigures(config, description) {
  const contract = planSectionsForDescriptionMode(config, 'child-plan')
  const recognised = new Set(contract.map((section) => section.heading))
  const terminalHeading = contract[contract.length - 1]?.heading
  const measured = parsePremises(config, description)
    .filter((premise) => premise.check && hasCountAssertion(premise.check))
    .map((premise) => withoutThousandsSeparators(premise.text))
  const violations = []
  for (const section of planDescriptionSections(config, description)) {
    if (!recognised.has(section.heading) || section.heading === terminalHeading) continue
    if (section.heading === PREMISES_HEADING || section.heading === ACCEPTANCE_HEADING) continue
    const seen = new Set()
    for (const paragraph of narrativeParagraphs(section.bodyLines.join('\n'))) {
      const prose = paragraph.replace(INLINE_CODE_SPAN, ' ')
      for (const match of prose.matchAll(BYTE_FIGURE)) {
        const figure = withoutThousandsSeparators(match[1])
        if (seen.has(figure) || Number(figure) < MIN_BYTE_FIGURE) continue
        if (BYTE_FIGURE_BEFORE.test(prose.slice(0, match.index))) continue
        if (BYTE_FIGURE_AFTER.test(prose.slice(match.index + match[0].length))) continue
        const repeated = new RegExp(`(?<!\\d)${figure}(?!\\d)`)
        if (measured.some((text) => repeated.test(text))) continue
        seen.add(figure)
        violations.push(
          violation(
            'unmeasured-count-claim',
            `${section.heading} states "${figure} bytes", but no ${PREMISES_HEADING} bullet with a counting check (wc -c, grep -c, …) repeats that figure — add a premise that measures it, or drop the number`,
          ),
        )
      }
    }
  }
  return violations
}

export function checkPremiseReusedAsCriterion(config, description) {
  const premiseChecks = new Set(
    parsePremises(config, description)
      .map((premise) => premise.check?.trim())
      .filter(Boolean),
  )
  return parseAcceptanceCriteria(config, description)
    .filter((criterion) => premiseChecks.has(criterion.check?.trim()))
    .map((criterion) =>
      violation(
        'premise-reused-as-criterion',
        `criterion "${criterion.text}" reuses a premise check command; a premise observes the pre-change tree and cannot certify the post-change constraint`,
      ),
    )
}

/**
 * checkPlanContract({ description, plan, config }) -> { ok, violations, blocking, advisories, couldNotEvaluate }
 *
 * `description` is the composed plan description about to be written to the tracker. `plan` is the
 * optional plan-file body about to be attached; omit it (or pass null/undefined) to skip the
 * `plan-file-residue` check. `config` defaults to DEFAULT_CONFIG. `citationCwd` and `citationFs`
 * are dependency-injection hooks for tests; production callers should leave them unset.
 */
export function checkPlanContract({
  description,
  plan = null,
  config = DEFAULT_CONFIG,
  mode = 'child-plan',
  citationCwd = undefined,
  citationFs = undefined,
  moduleRoots = [],
} = {}) {
  if (typeof description !== 'string') {
    throw new Error(
      'plan-contract-guard: checkPlanContract({ description, plan, config }) — description must be the markdown string; arguments look swapped',
    )
  }
  const violations = []
  const couldNotEvaluateResults = []

  // An unclosed fence makes every heading after it invisible to the splitter, so the STRUCTURAL
  // checks below would all misreport: one missing backtick line was reported as six missing
  // sections, sending an unattended drafter off to re-add sections it had already written. Report
  // the actual defect instead, and skip the three checks that read the broken split — they have no
  // information to add. The placeholder scan still runs: it applies its own raw-line fallback, so
  // it stays meaningful on exactly this input.
  const unterminated = hasUnterminatedFence(description)
  if (unterminated) {
    violations.push(
      violation(
        'not-a-description',
        'description opens a fenced code block that is never closed — every heading after it is invisible, so the section checks cannot run; close the fence and re-run',
      ),
    )
  }

  // Delegates the argument-order guard: a swapped call throws the validator's named error rather
  // than being reported as a contract violation of the plan.
  const result = validatePlanDescription(config, description, { mode })

  if (!result.ok && !unterminated) {
    if (result.missing.length > 0) {
      violations.push(
        violation(
          'missing-sections',
          `description is missing ${result.missing.length} required section(s): ${result.missing.join(', ')}`,
        ),
      )
    }
    if (result.unsupportedVersion) {
      violations.push(
        violation(
          'missing-sections',
          `description is stamped Contract: v${result.version}, newer than this contract`,
        ),
      )
    }
  }

  for (const heading of unterminated ? [] : result.unknown) {
    violations.push(
      violation(
        'unknown-section',
        `description carries off-contract heading "${heading}" — remove it, or register it in planContract.sections in the repo's skill config (.boss-skills.json)`,
      ),
    )
  }

  const emitted = unterminated ? [] : emittedContractHeadings(config, description, { mode })
  // A contract heading emitted twice is ONE defect — usually a duplicated block — and reporting it
  // as `section-order` ("emitted A → B → A") sends the reader hunting for a transposition. Name the
  // repeat instead, and keep `section-order` only when the first occurrences are themselves out
  // of order.
  const firstOccurrences = emitted.filter((heading, index) => emitted.indexOf(heading) === index)
  for (const heading of firstOccurrences) {
    const count = emitted.filter((h) => h === heading).length
    if (count < 2) continue
    violations.push(
      violation(
        'duplicate-section',
        `contract section "${heading}" is emitted ${count} times — keep exactly one copy`,
      ),
    )
  }
  if (!unterminated && !isContractOrdered(config, firstOccurrences, { mode })) {
    violations.push(
      violation(
        'section-order',
        `contract sections are out of order: emitted ${emitted.join(' → ')}; expected the relative order of ${planSectionsForDescriptionMode(
          config,
          mode,
        )
          .map((s) => s.heading)
          .join(' → ')}`,
      ),
    )
  }

  for (const token of placeholderResidue(config, description, { mode })) {
    violations.push(
      violation(
        'placeholder-residue',
        `unsubstituted placeholder token "${token}" in the drafted description — substitute it before the write`,
      ),
    )
  }

  const firstHeading = planSectionsForDescriptionMode(config, mode)[0]?.heading
  if (firstHeading && !description.replace(/^\s+/, '').startsWith(firstHeading)) {
    violations.push(
      violation(
        'not-a-description',
        `description does not begin with "${firstHeading}" — it is not the plan markdown`,
      ),
    )
  }
  const bytes = Buffer.byteLength(description, 'utf8')
  const minimumDescriptionBytes = minimumDescriptionBytesForMode(mode)
  if (bytes < minimumDescriptionBytes) {
    violations.push(
      violation(
        'not-a-description',
        `description is ${bytes} bytes, under the ${minimumDescriptionBytes}-byte floor — it is a placeholder or a paraphrase, not the plan`,
      ),
    )
  }

  if (plan !== null && plan !== undefined) {
    const residue = planFileResidue(plan)
    if (residue) violations.push(violation('plan-file-residue', residue))
  }

  for (const { line, text } of unterminated ? [] : mergedListItems(config, description, { mode })) {
    violations.push(
      violation(
        'merged-list-item',
        `## Planning line ${line} starts a second list item mid-line ("${text}") — put each bullet on its own line, directly after the previous one`,
      ),
    )
  }
  violations.push(...checkLineSpanningEmphasis(config, description, { mode }))
  violations.push(...checkSelfFalsifiedLiteralSearch(config, description))
  const citation = checkPlanCitations(config, description, {
    cwd: citationCwd,
    mode,
    ...(citationFs ? { fs: citationFs } : {}),
  })
  violations.push(...citation.violations)
  couldNotEvaluateResults.push(...citation.couldNotEvaluate)
  violations.push(...checkPrBodyOnlyEvidence(config, description))
  const commandVacuity = checkVerifyOnlyCommandVacuity(config, description, { cwd: citationCwd })
  violations.push(...commandVacuity.violations)
  violations.push(...checkPremiseReusedAsCriterion(config, description))
  violations.push(...checkUnmeasuredCountClaim(config, description))
  const advisories = [...commandVacuity.advisories]
  if (!unterminated) {
    const subjectAreas = checkSubjectAreas(config, description, { mode, moduleRoots })
    violations.push(...subjectAreas.violations)
    advisories.push(...subjectAreas.advisories)
  }

  const blocking = violations.filter((entry) => BLOCKING_VIOLATION_CODES.has(entry.code))
  return {
    ok: blocking.length === 0,
    violations,
    blocking,
    advisories,
    couldNotEvaluate: couldNotEvaluateResults,
  }
}

/**
 * The violations that stop a write: the text is not a plan description at all, a required section
 * is missing, a template placeholder was never substituted, or the plan file is empty or truncated. Every other code is a quality
 * finding the CLI prints as a warning — the plan is still written.
 */
export const BLOCKING_VIOLATION_CODES = new Set([
  'missing-sections',
  'not-a-description',
  'placeholder-residue',
  'plan-file-residue',
])

/**
 * Resolve the subject's OWN change areas from the composed description, and fail when the scan
 * that Phase 4's dependency step will run could not have matched anything.
 *
 * WHY IT LIVES IN THIS GATE. The same resolution runs later, in the dependency scan, and its
 * remedy for an unresolved token is "rewrite `## Key changes` as repo-relative paths". By then the
 * plan file has already been uploaded and byte-verified, so acting on the remedy costs a delete
 * plus a re-upload of an artifact whose bytes were supposed to be frozen. This gate already runs
 * BEFORE the attachment finalize, so raising the same fact here is the whole fix: nothing reorders,
 * and no resident skill prose has to describe a new procedure.
 *
 * The two faults are reported under ONE code with the fault named in the message, because their
 * remedies differ (name concrete paths / rewrite the named tokens) but their timing does not.
 *
 * Scoped to modes whose contract actually requires `## Key changes`, and to descriptions that
 * emitted it: an epic-parent overview has no such section, and a description missing it is already
 * reported as `missing-sections` — one mistake must not trip two codes.
 *
 * `moduleRoots` is the caller's repo module list, and it reaches this gate from the CLI's
 * `--module-roots` flag so a caller can hand it the SAME list it hands the dependency scan. That
 * reachability is the point: the classifier admits a slash-free token only when it is a declared
 * root, so a gate run with an empty list while the scan runs with the repo's roots is STRICTER than
 * the scan it reports for — and its remedy names a seam the caller could not otherwise reach. Left
 * empty the classifier still degrades to admitting any slash-carrying token, which is the lenient
 * direction for the tokens it does cover.
 */
function checkSubjectAreas(config, description, { mode, moduleRoots }) {
  const required = planSectionsForDescriptionMode(config, mode).map((section) => section.heading)
  // Exactly one non-blocking advisory whenever the check runs, naming what it read — an unexamined
  // description used to read exactly like a clean one. A description missing a REQUIRED
  // `## Key changes` gets none: `missing-sections` already reports it.
  if (!required.includes(KEY_CHANGES_HEADING)) {
    return {
      violations: [],
      advisories: [
        subjectAreasAdvisory(`skipped — ${mode} mode has no ${KEY_CHANGES_HEADING} section`),
      ],
    }
  }
  if (!emittedContractHeadings(config, description, { mode }).includes(KEY_CHANGES_HEADING)) {
    return { violations: [], advisories: [] }
  }
  const { areas, unresolved, source } = extractKeyChangeAreas(config, description, { moduleRoots })
  const advisories = [
    subjectAreasAdvisory(
      `read ${source}: ${areas.length} area(s), ${unresolved.length} unresolved token(s)`,
    ),
  ]
  const faults = []
  if (areas.length === 0) {
    faults.push(
      `it resolves to NO change areas, so the dependency scan can only compare it against nothing` +
        ` — name concrete repo-relative paths there`,
    )
  }
  if (unresolved.length > 0) {
    faults.push(
      `${unresolved.length} path-shaped token(s) do not resolve to a repo-relative area ` +
        `(${unresolved.join(', ')}) — rewrite them as repo-relative paths, or declare their ` +
        `leading directory in the dependency scan's \`moduleRoots\`; for a root file, declare the ` +
        `root file itself in \`moduleRoots\` and present it as code-marked or as the lead of a split bullet`,
    )
  }
  if (faults.length === 0) return { violations: [], advisories }
  return {
    violations: [
      violation(
        'subject-areas-unresolved',
        `${KEY_CHANGES_HEADING} cannot be resolved into change areas: ${faults.join('; ')}. ` +
          'Fix it now, before the attachment is finalized: after the upload the same remedy costs a ' +
          'delete plus a re-upload of bytes that were meant to be frozen.',
      ),
    ],
    advisories,
  }
}

function subjectAreasAdvisory(detail) {
  return {
    code: 'advisory: subject-areas-source',
    message: `plan-contract-guard: subject-area check ${detail}`,
  }
}

export function parseContractGuardArgs(argv) {
  const args = {
    description: null,
    plan: null,
    mode: 'child-plan',
    moduleRoots: [],
  }
  const readFlagValue = (flag, index) => {
    const value = argv[index + 1]
    if (!value || value.startsWith('--')) {
      throw new Error(`${flag} <value> is required`)
    }
    return value
  }
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    if (flag === '--description') {
      args.description = readFlagValue(flag, i)
      i += 1
    } else if (flag === '--plan') {
      args.plan = readFlagValue(flag, i)
      i += 1
    } else if (flag === '--mode') {
      args.mode = readFlagValue(flag, i)
      if (!DESCRIPTION_MODES.has(args.mode)) {
        throw new Error(
          `--mode must be one of ${[...DESCRIPTION_MODES].join(', ')}; got "${args.mode}"`,
        )
      }
      i += 1
    } else if (flag === '--module-roots') {
      // The SAME list the dependency scan is given. Without a way to pass it, this gate ran with
      // an empty one while the scan it claims to mirror ran with the repo's roots, so a
      // `## Key changes` naming a marked bare module name was a violation here and an area there
      // — and the violation's own remedy ("declare their leading directory") named a seam no
      // caller could reach. Comma-separated, so one shell word carries the whole list.
      args.moduleRoots = readFlagValue(flag, i)
        .split(',')
        .map((root) => root.trim())
        .filter((root) => root !== '')
      i += 1
    } else {
      throw new Error(`unknown argument: ${flag}`)
    }
  }
  if (!args.description) {
    throw new Error('--description <path> is required')
  }
  return args
}

function main() {
  const { description, plan, mode, moduleRoots } = parseContractGuardArgs(process.argv.slice(2))
  // A file we cannot read is itself a violation, never a pass: an unreadable input is exactly the
  // state a broken upstream extraction produces, and that is the input a vacuous gate blesses.
  let descriptionText
  try {
    descriptionText = readFileSync(description, 'utf8')
  } catch (error) {
    console.error(
      `plan-contract-guard: cannot read description ${description}: ${error.message} [unreadable-input]`,
    )
    process.exitCode = 1
    gateRecorder.record('fire', 'unreadable-input')
    return
  }
  let planText = null
  if (plan) {
    try {
      planText = readFileSync(plan, 'utf8')
    } catch (error) {
      console.error(
        `plan-contract-guard: cannot read plan ${plan}: ${error.message} [unreadable-input]`,
      )
      process.exitCode = 1
      gateRecorder.record('fire', 'unreadable-input')
      return
    }
  }

  const config = loadSkillConfig({ cwd: process.cwd() })
  const { ok, violations, blocking, advisories, couldNotEvaluate } = checkPlanContract({
    description: descriptionText,
    plan: planText,
    config,
    mode,
    moduleRoots,
    citationCwd: process.env.PLAN_CONTRACT_GUARD_CWD,
  })
  for (const { code, message } of couldNotEvaluate) {
    console.error(`${message} [${code}]`)
  }
  for (const { code, message } of advisories) {
    console.error(`${message} [${code}]`)
  }
  for (const { code, message } of violations) {
    if (!BLOCKING_VIOLATION_CODES.has(code)) console.error(`warning: ${message} [${code}]`)
  }
  if (ok) return
  for (const { code, message } of blocking) {
    console.error(`${message} [${code}]`)
  }
  console.error(`plan-contract-guard: ${blocking.length} blocking violation(s) — do not write`)
  process.exitCode = 1
}

// One gate-outcome line per invocation. Every real contract violation records the single bucket
// reason `violations`, deliberately NOT one of the exported VIOLATION_CODES: a run can emit
// several codes at once, and picking one would invent a ranking this guard does not have. The
// `unreadable-input` branches DO record precisely, and the recorder LATCHES so the exit-code
// derived call below is a no-op for them. The latch is also what makes recording-then-throwing
// safe: the catch cannot double-count.
const gateRecorder = createGateRecorder('plan-contract-guard')

// isMainModule resolves both paths through symlinks so this fail-closed CLI gate cannot be skipped.
const invokedDirectly = isMainModule(import.meta.url)
if (invokedDirectly) {
  try {
    main()
    gateRecorder.record(process.exitCode ? 'fire' : 'pass', process.exitCode ? 'violations' : 'ok')
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
    gateRecorder.record('fire', 'guard-threw')
  }
}
