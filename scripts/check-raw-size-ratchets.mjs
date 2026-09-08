#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { insideStringLiteral } from './check-vacuous-regions.mjs'

// A size ratchet that measures its own artifact and compares the result by hand is the shape
// BOS-768 removed from nine call sites, and this gate stops it coming back.
//
// Why the hand-written shape is worse than it looks. Every one of the nine spelled a
// one-sided comparison against a pin, which reds when the artifact grows and says NOTHING
// when it shrinks. Measured at conversion time, seven of the nine pins sat ABOVE the real
// artifact — 6, 8, 67, 83, 91, 81 and 311 bytes above — and every one of those gaps was
// growth the artifact could take back with the suite still green. The same shape also makes
// a real trim worthless: a reduction does not clear anything, it just widens the gap. The
// fail-closed replacement is `assertExactSize` in scripts/size-ratchet-lib.mjs, which
// compares for equality, so a shrink reds too and the only way to clear it is to bank the
// saving in the constant.
//
// THREE RULES, ALL STRUCTURAL. The first two flag the MEASUREMENT, not the comparison. That
// is a deliberate narrowing: a detector for the comparison itself — an assertion whose operand
// is compared against a SCREAMING_SNAKE constant — was prototyped over this exact scope and
// produced eight false positives, none of them size gates. They were string fixtures
// containing `<TICKET`, byte-OFFSET ordering assertions (`gateAt > PHASE_4_SECTION`), and a
// batch-size bound (`b.length <= GO_BATCH`). A detector that guesses is a detector that
// quietly stops detecting, so the general comparison rule was dropped rather than shipped with
// a standing false-positive tax and the opt-outs that would follow. That decision stands, and
// rule 3 below does NOT reopen it. What it still costs is stated in RESIDUAL — this gate is
// not the whole invariant, and does not claim to be.
//
// Rule 1, `raw-byte-measure`: `Buffer.byteLength(` inside a scanned file. `measureFile` from
// size-ratchet-lib exists precisely so a size gate never measures for itself, and it throws
// on a missing, unreadable or empty artifact rather than returning 0 — which the raw call
// cannot do, because `Buffer.byteLength('')` is a perfectly good 0 that satisfies any upper
// bound. A gate whose artifact vanished should be red, not green.
//
// Rule 2, `raw-line-measure`: the `.split(<newline>).length` line-count idiom inside a
// scanned file, which is how the CLAUDE.md ceiling counted lines before the conversion. Same
// argument: `measureFile(p, { unit: 'lines' })` counts and fails closed in one call.
//
// Rule 3, `raw-budget-compare` (BOS-1208): a comparison operator applied DIRECTLY to the
// result of the size-lib measurement helper, in either operand position. Routing the
// measurement through the helper and then hand-writing the comparison is the same leak one
// step later — the helper's fail-closed measurement is preserved and its asymmetric price is
// thrown away, so a shrink stops being free and a raise stops costing a recorded reason.
// `assertDescendingBudget` (or `assertExactSize`, where an equality pin is the right price) is
// what the operand belongs in.
//
// This rule does NOT reintroduce the general comparison detector and shares no shape with the
// eight false positives recorded above: the operand here must literally be a call to the
// measurement helper, which exists nowhere but a size gate. `gateAt > PHASE_4_SECTION`,
// `b.length <= GO_BATCH` and the `<TICKET` string fixtures are all invisible to it, because
// none of them measures anything. The helper's arguments nest parentheses
// (`measure…(path.join(rootDir, skillPath))`), so a flat regex would stop at the wrong `)`;
// the scan balances parens instead. Its blind spot is the mirror of that narrowness, and is
// recorded in RESIDUAL.
//
// Parser-free, so prose is scanned too. Neither pattern is spelled verbatim anywhere in this
// file's own comments, and the scope below excludes this file and its test regardless — see
// SCAN_EXCLUSIONS.
const RAW_BYTE_MEASURE = /(?<![\w$])Buffer\s*\.\s*byteLength\s*\(/g

// Built from pieces rather than written literally so this file does not match its own rule if
// the scope is ever widened to include it. `\n` here is the two source characters backslash-n
// as they appear inside the quoted argument, not a newline.
const RAW_LINE_MEASURE = new RegExp(
  String.raw`\.\s*split\s*\(\s*(['"\`])\\n\1\s*\)\s*\.\s*length`,
  'g',
)

// Assembled rather than written literally, for the same reason RAW_LINE_MEASURE is: a scanned
// copy of this file must not match its own rule.
const MEASURE_CALL = ['measure', 'File'].join('')

// Comparison operators the budget shape can be spelled with, longest alternative first so
// `===` is never read as `==` and `>=` is never read as `>`. `=>`, `!=` and `!==` are listed
// only so the trailing-operand scan can RECOGNISE and reject them: an arrow function is how
// the measurement helper is legitimately wrapped in a callback, and it ends in the same
// character a `>` comparison does.
const COMPARE_AFTER = /^\s*(===|==|<=|>=|<|>)/
const COMPARE_BEFORE = /(===|!==|==|!=|<=|>=|=>|<|>)$/
const NOT_A_COMPARISON = new Set(['=>', '!=', '!=='])

/** Index of the `)` closing the `(` at `openIndex`, or -1 if the source is unbalanced. */
function closingParen(contents, openIndex) {
  let depth = 0
  for (let i = openIndex; i < contents.length; i += 1) {
    if (contents[i] === '(') depth += 1
    else if (contents[i] === ')') {
      depth -= 1
      if (depth === 0) return i
    }
  }
  return -1
}

/**
 * Find every comparison written directly against a measurement-helper call.
 *
 * Paren-balancing rather than regex, because the helper's argument is routinely a nested call.
 * The balance is naive about parentheses inside string literals in those arguments; a path
 * containing one would end the scan early and lose the hit, which is a miss rather than a
 * false positive and is recorded in RESIDUAL.
 *
 * @param {string} contents Whole file text.
 * @returns {{index: number, text: string}[]} Hits, in source order.
 */
function findBudgetCompares(contents) {
  const needle = `${MEASURE_CALL}(`
  const hits = []
  let from = 0
  for (;;) {
    const start = contents.indexOf(needle, from)
    if (start === -1) break
    from = start + needle.length
    // `someOtherMeasureFile(` is a different identifier, not this helper.
    if (start > 0 && /[\w$]/.test(contents[start - 1])) continue
    const end = closingParen(contents, start + needle.length - 1)
    if (end === -1) continue

    const after = COMPARE_AFTER.exec(contents.slice(end + 1, end + 65))
    if (after) {
      hits.push({ index: start, text: `${MEASURE_CALL}(…) ${after[1]}` })
      continue
    }
    const before = COMPARE_BEFORE.exec(contents.slice(0, start).replace(/\s+$/, ''))
    if (before && !NOT_A_COMPARISON.has(before[1])) {
      hits.push({ index: start, text: `${before[1]} ${MEASURE_CALL}(…)` })
    }
  }
  return hits
}

// A rule carries EITHER a `pattern` (a global regex) or a `scan` (a function returning the
// same hit shape). The budget rule cannot be a regex — its operand nests parentheses — and
// `findRawSizeRatchets` normalises both into the one `{line, rule, remedy, text}` offender.
const RULES = [
  {
    name: 'raw-byte-measure',
    pattern: RAW_BYTE_MEASURE,
    remedy: 'use measureFile() from scripts/size-ratchet-lib.mjs',
  },
  {
    name: 'raw-line-measure',
    pattern: RAW_LINE_MEASURE,
    remedy: "use measureFile(path, { unit: 'lines' }) from scripts/size-ratchet-lib.mjs",
  },
  {
    name: 'raw-budget-compare',
    remedy:
      'pass the measurement to assertDescendingBudget() (or assertExactSize()) from ' +
      'scripts/size-ratchet-lib.mjs rather than comparing it by hand',
    scan: findBudgetCompares,
  },
]

/** Normalise a global-regex rule into the same hit shape a `scan` rule returns. */
function regexHits(pattern, contents) {
  pattern.lastIndex = 0
  return [...contents.matchAll(pattern)].map((match) => ({ index: match.index, text: match[0] }))
}

// The escape hatch, on the offending line or the line immediately before it. The reason is
// REQUIRED and must be non-empty — an unexplained opt-out is the next unbanked ratchet.
const OPT_OUT = /\/\/\s*size-ratchet-ok:(.*)$/

// SCOPE — the files that hold committed-artifact size ratchets, and deliberately NOT every
// `.mjs` in the repo. `Buffer.byteLength` has ~28 legitimate uses outside this scope: output
// truncation budgets, stderr tail caps, embed limits. Those bound DYNAMIC content at runtime
// and have nothing to do with a pin on a committed file, so flagging them would be a
// standing false-positive tax paid by unrelated work.
//
// The scope is a name rule plus an explicit list, both printed with the success line so the
// verdict can never read wider than what was actually looked at.
export const SCANNED_DIR = 'scripts'
export const SCANNED_NAME = /skill.*\.test\.mjs$/
export const SCANNED_EXTRA = ['scripts/check-agent-test-guidance.test.mjs']

// This gate's own test must carry both forbidden shapes verbatim as fixture text — that text
// is what proves the detector fires — and size-ratchet-lib's test legitimately calls
// `Buffer.byteLength` to assert what `measureFile` returns. Neither matches SCANNED_NAME
// today, so both entries are belt-and-braces against a later widening of the scope rather
// than load-bearing now.
export const SCAN_EXCLUSIONS = [
  'scripts/check-raw-size-ratchets.test.mjs',
  'scripts/size-ratchet-lib.test.mjs',
]

// RESIDUAL — what a green run here does NOT establish, stated in the gate rather than left
// for a reader to discover:
//
//   1. It does not prove any pin is CORRECT, only that no gate in scope measures by hand. A
//      call site can pass `measured` and `expected` through assertExactSize with a wrong
//      number and this gate is silent; the test suite is what catches that.
//   2. Rule 3 catches a comparison written DIRECTLY on the helper call. It does not catch the
//      same comparison one variable later — bind the measurement to a name first and compare
//      that name, and nothing here fires. Closing that would need the general comparison
//      detector whose eight false positives are recorded above, which is a worse trade.
//   3. It does not prove a budget DESCENDS — and neither does anything else. This scan cannot
//      see it, and `assertDescendingBudget` reds on the review date without checking that the
//      budget then fell by `stepDown`, because it keeps no record of the previous `reviewBy`.
//      Moving the date forward alone clears the red. Review is what closes that, not a gate.
//   4. It looks at one directory and one filename pattern. A size ratchet written in a file
//      that does not match SCANNED_NAME is not scanned at all.
//   5. The three rules match THREE SPELLINGS, not the concept of measuring. Hand-rolled
//      measurement written any other way — `fs.statSync(p).size`, `readFileSync(p).length`,
//      `[...text].length`, `split(/\r?\n/).length`, or a helper that wraps any of them — is
//      invisible here, with no opt-out marker to make the omission visible either. This is the
//      same failure mode the ticket exists to remove, one level up: a structural detector's
//      verdict is bounded by the shapes it enumerates, so read the rule list, not the headline.
export const RESIDUAL =
  'a green run means no gate in scope measures bytes or lines by hand, and none compares a ' +
  'measurement directly against a budget, IN THE THREE SPELLINGS these rules match — not ' +
  'that any pin is correct, not that a budget actually descends, not that a comparison over ' +
  'an intermediate variable holding a measureFile() result is routed through the library, ' +
  'not that a size ratchet outside SCANNED_NAME exists at all, and not that another ' +
  'measurement spelling (statSync().size, readFileSync().length, split(/\\r?\\n/).length, or ' +
  'a wrapper around them) is absent'

function hasOptOut(lines, lineNumber) {
  for (const offset of [1, 2]) {
    const candidate = lines[lineNumber - offset]
    if (typeof candidate !== 'string') continue
    const match = OPT_OUT.exec(candidate)
    if (!match || match[1].trim() === '') continue
    // The marker must be a real comment, not text inside a string literal. Shared with
    // check-vacuous-regions rather than forked: see its OPT_OUT_SCOPE_LIMITS block for the
    // two constructed inputs this scan still does not close.
    if (insideStringLiteral(candidate, match.index)) continue
    return true
  }
  return false
}

/**
 * Find every un-opted-out hand-rolled size measurement in `contents`.
 * @param {string} contents Whole file text.
 * @returns {{line: number, rule: string, remedy: string, text: string}[]} Offenders.
 */
export function findRawSizeRatchets(contents) {
  const lines = contents.split(String.fromCharCode(10))
  const offenders = []
  for (const rule of RULES) {
    const hits = rule.scan ? rule.scan(contents) : regexHits(rule.pattern, contents)
    for (const hit of hits) {
      const line = contents.slice(0, hit.index).split(String.fromCharCode(10)).length
      if (hasOptOut(lines, line)) continue
      offenders.push({ line, remedy: rule.remedy, rule: rule.name, text: hit.text })
    }
  }
  return offenders.sort((a, b) => a.line - b.line)
}

export function scannedFiles(repoRoot, deps = {}) {
  const fsImpl = deps.fs || fs
  const dir = path.join(repoRoot, SCANNED_DIR)
  const named = fsImpl.existsSync(dir)
    ? fsImpl
        .readdirSync(dir)
        .filter((name) => SCANNED_NAME.test(name))
        .map((name) => path.join(dir, name))
    : []
  const extra = SCANNED_EXTRA.map((rel) => path.join(repoRoot, ...rel.split('/'))).filter((file) =>
    fsImpl.existsSync(file),
  )
  const excluded = new Set(SCAN_EXCLUSIONS.map((rel) => path.join(repoRoot, ...rel.split('/'))))
  return [...new Set([...named, ...extra])].filter((file) => !excluded.has(file)).sort()
}

export function findRawSizeRatchetsInRepo(repoRoot, deps = {}) {
  const fsImpl = deps.fs || fs
  return scannedFiles(repoRoot, deps).flatMap((file) =>
    findRawSizeRatchets(fsImpl.readFileSync(file, 'utf8')).map((offender) => ({
      ...offender,
      file,
    })),
  )
}

function main() {
  const repoRoot = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')
  const files = scannedFiles(repoRoot)
  const offenders = findRawSizeRatchetsInRepo(repoRoot)
  if (offenders.length > 0) {
    console.error(
      'Hand-rolled size measurement found in a size-ratchet test. A gate that measures for ' +
        'itself cannot fail closed on a missing or empty artifact, and the comparison that ' +
        'follows is invariably one-sided. Route it through scripts/size-ratchet-lib.mjs, or ' +
        'add `// size-ratchet-ok: <reason>`:',
    )
    for (const offender of offenders) {
      console.error(
        `  - ${path.relative(repoRoot, offender.file)}:${offender.line} [${offender.rule}] ` +
          `${offender.remedy}`,
      )
    }
    process.exit(1)
  }
  // Qualified with the scope on purpose: an unqualified "none found" reads as a whole-tree
  // verdict, and this gate looked at a named subset. See SCANNED_DIR / SCANNED_NAME / RESIDUAL.
  console.log(
    `No hand-rolled size measurements in ${files.length} scanned file(s) ` +
      `(${SCANNED_DIR}/ matching ${SCANNED_NAME.source}, plus ${SCANNED_EXTRA.join(', ')}). ` +
      `Not covered: ${RESIDUAL}.`,
  )
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)

if (invokedDirectly) main()
