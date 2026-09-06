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

import { isMainModule } from './main-module.mjs'
import {
  classifyCheckCommand,
  DEFAULT_CONFIG,
  loadSkillConfig,
  markdownH2Heading,
  parseAcceptanceCriteria,
  parsePremises,
  planFileFloor,
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

// Minimum plausible sizes for real plan descriptions. Epic parents have only four required
// sections, so the child-plan floor would reject the terse but valid parent overview shape.
const MIN_DESCRIPTION_BYTES = 200
const MIN_EPIC_PARENT_DESCRIPTION_BYTES = 80
const DESCRIPTION_MODES = new Set(['child-plan', 'epic-parent'])
const PLAN_FILE_EXEMPTIONS = new Set(['epic-parent-overview', 'adopted-child-redraft', 'consumer'])
const PREMISES_HEADING = '## Premises'
const ACCEPTANCE_HEADING = '## Acceptance criteria'
const CITATION_PATTERN = String.raw`(?<![\w:/.-])((?:\.{1,2}\/)?(?:[\w.-]+\/)*[\w.-]+\.[A-Za-z0-9_-]+):(\d+)(?![\w.-])`
const CITATION = new RegExp(CITATION_PATTERN, 'g')
const CITATION_ONCE = new RegExp(CITATION_PATTERN)
const PR_BODY = /\b(?:PR|pull[- ]request)\s+body\b/i
const ORCHESTRATOR_OWNED = /\borchestrator-owned\b/i

function violation(code, message) {
  return { code, message: `plan-contract-guard: ${message}` }
}

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

function planFileHeadingSections(plan) {
  const scan = scanFences(plan)
  const sourceLines = allLines(plan)
  const outside = new Set(scan.lines.map(({ index }) => index))
  const sections = []
  let current = null
  let inTerminal = false
  for (const { line, index } of sourceLines) {
    const heading = markdownH2Heading(line)
    if (!inTerminal && outside.has(index) && heading) {
      current = { heading, bodyLines: [] }
      sections.push(current)
      if (heading === '## Original notes') inTerminal = true
      continue
    }
    if (current) current.bodyLines.push(line)
  }
  return { sections, unterminated: scan.unterminated }
}

function planFileSectionHasBody(section) {
  return section.bodyLines.some((line) => line.replace(INLINE_CODE_SPAN, '').trim().length > 0)
}

/**
 * Producer-side plan-file structure floor.
 *
 * This rule rejects a promoted single-ticket plan file that has been flattened into the plan
 * description's contract headings. This check reads only the plan attachment body supplied to
 * `checkPlanContract`, not any drafter report about that body. Accepted false negative: a drafter
 * can satisfy the structural floor with a junk extra heading; content quality belongs to review.
 */
export function checkPlanFileStructure(config, plan, { exemption = null } = {}) {
  if (exemption !== null && exemption !== undefined) {
    if (!PLAN_FILE_EXEMPTIONS.has(exemption)) {
      return {
        ok: false,
        headingsInspected: 0,
        violations: [
          violation(
            'plan-file-structure-exemption',
            `unrecognised plan-file structure exemption "${exemption}" — expected one of ${[
              ...PLAN_FILE_EXEMPTIONS,
            ].join(', ')}`,
          ),
        ],
      }
    }
    return { ok: true, headingsInspected: 0, violations: [] }
  }

  const floor = planFileFloor(config)
  const scan = planFileHeadingSections(plan)
  const sections = scan.sections
  const headings = sections.map((section) => section.heading)
  const headingsInspected = headings.length
  const violations = []
  const describedHeadings = new Set(
    planSectionsForDescriptionMode(config, 'child-plan').map((s) => s.heading),
  )
  const additional = headings.filter((heading) => !describedHeadings.has(heading))

  if (headingsInspected === 0) {
    violations.push(
      violation(
        'plan-file-structure',
        'plan file structure inspected 0 heading(s); expected contract headings plus the configured plan-file floor',
      ),
    )
  }

  if (scan.unterminated) {
    violations.push(
      violation(
        'plan-file-structure',
        `plan file structure inspected ${headingsInspected} heading(s) but contains an unterminated fenced code block`,
      ),
    )
  }

  const present = new Map(sections.map((section) => [section.heading, section]))
  const missingContract = planSectionsForDescriptionMode(config, 'child-plan')
    .filter((section) => section.required === 'always')
    .map((section) => section.heading)
    .filter((heading) => !present.has(heading))
  if (missingContract.length > 0) {
    violations.push(
      violation(
        'plan-file-structure',
        `plan file structure inspected ${headingsInspected} heading(s) but is missing contract section(s): ${missingContract.join(', ')}`,
      ),
    )
  }

  for (const heading of floor.requiredHeadings) {
    const section = present.get(heading)
    if (!section) {
      violations.push(
        violation(
          'plan-file-structure',
          `plan file structure inspected ${headingsInspected} heading(s) but is missing required plan-file heading "${heading}"`,
        ),
      )
      continue
    }
    if (!planFileSectionHasBody(section)) {
      violations.push(
        violation(
          'plan-file-structure',
          `plan file structure inspected ${headingsInspected} heading(s) but required plan-file heading "${heading}" is empty`,
        ),
      )
    }
  }

  if (additional.length < floor.minimumAdditionalHeadings) {
    violations.push(
      violation(
        'plan-file-structure',
        `plan file structure inspected ${headingsInspected} heading(s) with ${additional.length} heading(s) outside planContract.sections; expected at least ${floor.minimumAdditionalHeadings}`,
      ),
    )
  }

  return {
    ok: violations.length === 0,
    headingsInspected,
    additionalHeadingCount: additional.length,
    violations,
  }
}

function sectionText(config, description, heading, mode) {
  const section = planDescriptionSections(config, description, { mode }).find(
    (s) => s.heading === heading,
  )
  return section ? section.bodyLines.join('\n') : ''
}

function scanCitationSections(config, description, mode) {
  return [PREMISES_HEADING, ACCEPTANCE_HEADING]
    .flatMap((heading) =>
      linesOutsideFences(sectionText(config, description, heading, mode)).map(
        ({ line, index }) => ({
          heading,
          line,
          index,
        }),
      ),
    )
    .flatMap(({ heading, line, index }) => {
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

function resolveCitationPath(root, file) {
  const resolvedRoot = resolve(root)
  const resolvedFile = resolve(resolvedRoot, file)
  const rel = relative(resolvedRoot, resolvedFile)
  if (rel === '' || rel.startsWith('..') || rel.split(/[\\/]/).includes('..')) return null
  return resolvedFile
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
  for (const hit of scanCitationSections(config, description, mode)) {
    const resolved = resolveCitationPath(root, hit.file)
    if (!resolved) {
      violations.push(
        violation(
          'unresolvable-citation',
          `${hit.heading} cites ${hit.citation}, but ${hit.file} escapes the working tree root`,
        ),
      )
      continue
    }
    let body
    try {
      body = fs.readFileSync(resolved, 'utf8')
    } catch (error) {
      violations.push(
        violation(
          'unresolvable-citation',
          `${hit.heading} cites ${hit.citation}, but ${hit.file} could not be read: ${error.message}`,
        ),
      )
      continue
    }
    const lineCount = body.split('\n').length
    if (hit.line > lineCount) {
      violations.push(
        violation(
          'unresolvable-citation',
          `${hit.heading} cites ${hit.citation}, but ${hit.file} has only ${lineCount} line(s)`,
        ),
      )
    }
  }
  return { violations, couldNotEvaluate: couldNotEvaluateItems }
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
  const checkItem = (kind, item) => {
    if (!item.check) return
    const classified = classifyCheckCommand(item.check, opts)
    for (const finding of classified.blocking) {
      violations.push(
        violation(
          `vacuous-${kind}-command-${finding.code}`,
          `${kind} "${item.text}" names an unresolvable check command: ${finding.message}`,
        ),
      )
    }
  }
  for (const criterion of parseAcceptanceCriteria(config, description)) {
    checkItem('criterion', criterion)
  }
  for (const premise of parsePremises(config, description)) {
    checkItem('premise', premise)
  }
  return violations
}

/**
 * checkPlanContract({ description, plan, config }) -> { ok, violations, couldNotEvaluate }
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
  planFileExemption = null,
  citationCwd = undefined,
  citationFs = undefined,
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
  if (!unterminated && !isContractOrdered(config, emitted, { mode })) {
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
    const structure = checkPlanFileStructure(config, plan, { exemption: planFileExemption })
    violations.push(...structure.violations)
  }

  violations.push(...checkSelfFalsifiedLiteralSearch(config, description))
  const citation = checkPlanCitations(config, description, {
    cwd: citationCwd,
    mode,
    ...(citationFs ? { fs: citationFs } : {}),
  })
  violations.push(...citation.violations)
  couldNotEvaluateResults.push(...citation.couldNotEvaluate)
  violations.push(...checkPrBodyOnlyEvidence(config, description))
  violations.push(...checkVerifyOnlyCommandVacuity(config, description, { cwd: citationCwd }))

  return {
    ok: violations.length === 0 && couldNotEvaluateResults.length === 0,
    violations,
    couldNotEvaluate: couldNotEvaluateResults,
  }
}

export function parseContractGuardArgs(argv) {
  const args = { description: null, plan: null, mode: 'child-plan', planFileExemption: null }
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
    } else if (flag === '--plan-file-exemption') {
      args.planFileExemption = readFlagValue(flag, i)
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
  const { description, plan, mode, planFileExemption } = parseContractGuardArgs(
    process.argv.slice(2),
  )
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
      return
    }
  }

  const config = loadSkillConfig({ cwd: process.cwd() })
  const { ok, violations, couldNotEvaluate } = checkPlanContract({
    description: descriptionText,
    plan: planText,
    config,
    mode,
    planFileExemption,
    citationCwd: process.env.PLAN_CONTRACT_GUARD_CWD,
  })
  for (const { code, message } of couldNotEvaluate) {
    console.error(`${message} [${code}]`)
  }
  if (ok) return
  for (const { code, message } of violations) {
    console.error(`${message} [${code}]`)
  }
  const unknown = couldNotEvaluate.length
  const suffix = unknown ? `; ${unknown} check(s) could not be evaluated` : ''
  console.error(
    `plan-contract-guard: ${violations.length} contract violation(s)${suffix} — do not write`,
  )
  process.exitCode = 1
}

// isMainModule resolves both paths through symlinks so this fail-closed CLI gate cannot be skipped.
const invokedDirectly = isMainModule(import.meta.url)
if (invokedDirectly) {
  try {
    main()
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
