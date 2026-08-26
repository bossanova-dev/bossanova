#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// A raw region extraction — `X.slice(i)` or `X.slice(i, j)` with the bounds spelled inline
// as `X.indexOf(…)` in the argument list — goes vacuous the moment
// a marker moves: `indexOf` returns -1, `String.prototype.slice` reads -1 as "one character
// from the end", and every negative assertion over the resulting sliver passes while the
// forbidden content sits untouched in the file. `region()` in scripts/gate-region-lib.mjs
// is the fail-closed replacement; this gate stops the raw shape coming back.
//
// NOT line-anchored, on purpose: Prettier wraps the longer call sites across four lines,
// and a per-line regex would ship this gate already blind to exactly the population it
// exists to find. `\s` spans newlines, so a whole-file scan catches both forms.
//
// Same-identifier rule: the receiver captured from `X.slice(` must be the same identifier
// inside the first `.indexOf(`. Telling a string receiver from an array receiver without a
// parser is not reliable, and a detector that guesses is a detector that quietly stops
// detecting — so legitimate array slices are flagged and opted out by hand instead.
//
// Parser-free also means prose is scanned: the comments in this file and in
// gate-region-lib.mjs therefore describe the forbidden shape without spelling it verbatim,
// so the gate passes on its own sources without an escape hatch. The gate's TEST is the one
// file that must carry the shape literally, and it is excluded by path below.
//
// The leading boundary is `(?<![\w$])`, not `\b`: `$` is a non-word character, so `\b` never
// matches before a `$`-prefixed identifier, so a receiver named `$src` walked straight
// through the gate. Found by feeding the gate that exact receiver — see the falsification
// suite's underscore/dollar case. A lookbehind excluding `$` as well as `\w` still refuses a
// suffix match (`bodyText.slice(…)` must not match as receiver `Text`).
const RAW_REGION_SLICE = /(?<![\w$])([A-Za-z_$][\w$]*)\s*\.\s*slice\s*\(\s*\1\s*\.\s*indexOf\s*\(/g
const RAW_SECTION_DISTANCE_WINDOW =
  /(?<![\w$])([A-Za-z_$][\w$]*)\s*\.\s*slice\s*\(\s*([A-Za-z_$][\w$]*|0)\s*,\s*(?:\2\s*\+\s*([0-9]+)|([0-9]+))\s*\)/g
const RAW_SECTION_HEADING_TERMINATOR =
  /\.(?:search|indexOf)\s*\(\s*(?:\/\\n#(?:\{[0-9,]+\}|#+)?(?:\\s| )\/|'\\n##(?: |\\s)'|"\\n##(?: |\\s)"|`\\n##(?: |\\s)`)/g
const WHOLE_DOC_FLATTENED_ASSIGN =
  /\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*([A-Za-z_$][\w$]*)\s*\.\s*replace\s*\(\s*\/\\s\+\/g\s*,\s*['"] ['"]\s*\)/g
const DOES_NOT_MATCH_CALL = /assert\s*\.\s*doesNotMatch\s*\(\s*([A-Za-z_$][\w$]*)\b/g
const WHOLE_DOC_NAMES = new Set(['source', 'skill', 'raw'])

// The escape hatch, on the match's own line or the line immediately before it. The reason
// is REQUIRED and must be non-empty — an unexplained opt-out is the next vacuous gate. This
// regex finds a CANDIDATE marker; `hasOptOut` below then requires it to be a real comment
// rather than text inside a string literal, because a marker the reviewer never sees is an
// unexplained opt-out wearing a reason.
const OPT_OUT = /\/\/\s*gate-region-ok:(.*)$/

// The two roots this gate ratchets, and deliberately NOT the whole tree. Naming the scope in
// code matters because offences of the fully-covered shape live outside it today —
// `services/marketing/tests/theme.test.mjs:55` and `:123` are both raw region slices, and the
// second has no fail-closed pin at all. They are a follow-up, not a silent pass: widening the
// scan to `services/**` would make every marketing/web task depend on this gate's inputs, which
// is a build-graph change rather than a ratchet. `main()` below prints the scanned roots with
// its success line so the verdict can never read wider than what was actually looked at.
export const SCANNED_ROOTS = ['scripts', 'skills-toolbox']

// The gate's own test must carry the forbidden shape verbatim as fixture text — that text
// is what proves the detector fires — so it is the one file exempt from the scan, by exact
// repo-relative path. Nothing else may be added here: opt out a real call site instead.
export const SCAN_EXCLUSIONS = ['scripts/check-vacuous-regions.test.mjs']
export const UNFALSIFIABLE_PROSE_PIN_FILES = [
  'scripts/boss-build-skill.test.mjs',
  'scripts/boss-review-skill.test.mjs',
]

// ─── OPT_OUT_SCOPE_LIMITS — the authoritative statement of both, stated once ─────────────
//
// Both limits used to be spelled out in full three times: here, in `insideStringLiteral`'s
// JSDoc below, and in the falsification suite's documented-scope-limit case. Those two now
// POINT here instead of restating, and must keep doing so: this prose already drifted into a
// false invariant once (an earlier revision claimed the check was "fail-closed in every
// direction", which it is not), and three copies of a claim is how a fourth revision drifts
// again unnoticed. Grep `OPT_OUT_SCOPE_LIMITS` to find every reference. The test PINS these
// limits executably; this block is the only place that EXPLAINS them.
//
// `insideStringLiteral` is a single-line quote scan, not a tokenizer. It closes the plausible
// bypass — a marker sitting in an ordinary string literal on its own line, which is what an
// author writes by accident and what an abuser writes on purpose. Two CONSTRUCTED inputs still
// suppress an offence:
//
//   1. A marker inside a MULTI-LINE template literal. On its own line the marker's quotes are
//      balanced, so it is indistinguishable from a real comment. Closing it needs quote state
//      carried ACROSS lines, and that was implemented and then REVERTED, because it does not
//      work here without a real parser. Only a backtick survives a newline, so one unbalanced
//      backtick anywhere — in prose a scanner has no reason to treat differently — makes every
//      following line read as template contents. The live example is
//      `scripts/sweep-pr-gate.test.mjs:652`:
//
//          /** Join `\`-continuations so a multi-line invocation is judged as one command. */
//
//      Two backticks, but the SECOND is escaped and consumed by the escape branch, so the span
//      survives the newline. (Ordinary balanced JSDoc backticks — `:712`, `:728` — are
//      harmless; it is specifically the escaped one that unbalances the line, which is why this
//      cites `:652` and not them.) Measured, not assumed: with cross-line state, 150 of the 232
//      lines after `:652` read as inside a template, which un-suppressed the reviewed opt-out
//      at `:755` and turned this gate RED on correct code.
//
//   2. A regex literal earlier on the line can UN-POISON the state: in `/'/ ; const s = '//
//      gate-region-ok: x'` the regex's quote opens the span and the string's opening quote
//      closes it, leaving the marker apparently unquoted.
//
// Closing either needs a tokenizer, the same trade the same-identifier rule above already
// declined. A detector that guesses is a detector that quietly stops detecting, so the limits
// are recorded and pinned by a test instead of papered over with a heuristic. Where the scan is
// wrong in the OTHER direction — a stray quote leaving the span open — the opt-out is IGNORED
// and the offence REPORTED, which costs an author one moved comment.

/**
 * True when the `//` at `at` sits inside a quoted span — so it opens a string's contents
 * rather than a comment.
 *
 * Why not "the marker must be the first non-whitespace on its line", the obvious tightening:
 * it breaks a LEGITIMATE opt-out this repo already relies on. sweep-releases-gate.mjs:42
 * spells its marker after a ternary `?`, which is the natural shape when the offence sits in
 * a conditional expression, so that rule would un-suppress a reviewed opt-out and turn the
 * gate red on correct code — the mirror-image overshoot a hasty fail-closed fix produces.
 *
 * WHAT THIS DOES AND DOES NOT BUY. It is NOT a JS parser and it is NOT fail-closed in every
 * direction; an earlier revision of this comment claimed it was, and that claim was false. The
 * two inputs that still suppress an offence, and why neither is closed, are stated once in the
 * `OPT_OUT_SCOPE_LIMITS` block above — do not restate them here; that duplication is what let
 * the false claim survive.
 *
 * The scan is deliberately WITHIN-LINE ONLY: it starts fresh at every line and carries nothing
 * out of one, which is what the reverted attempt described there tried to change. It stops at
 * an unquoted `//` so that a stray apostrophe in an earlier same-line comment ("don't") cannot
 * open a span that swallows a later genuine marker on that same line.
 *
 * Exported for `scripts/check-raw-size-ratchets.mjs`, which needs the same opt-out
 * discipline and must not fork a second copy of this scanner: the limits recorded in
 * `OPT_OUT_SCOPE_LIMITS` are hard-won, and a duplicate would drift away from them.
 *
 * @param {string} line Raw source line.
 * @param {number} at Index of the `//` on that line.
 * @returns {boolean}
 */
export function insideStringLiteral(line, at) {
  let quote = null
  for (let i = 0; i < at; i++) {
    const ch = line[i]
    if (ch === '\\') {
      i++
      continue
    }
    if (quote) {
      if (ch === quote) quote = null
    } else if (ch === '"' || ch === "'" || ch === '`') {
      quote = ch
    } else if (ch === '/' && line[i + 1] === '/') {
      break
    }
  }
  return quote !== null
}

function hasOptOut(lines, lineNumber) {
  for (const offset of [1, 2]) {
    const index = lineNumber - offset
    const candidate = lines[index]
    if (typeof candidate !== 'string') continue
    const match = OPT_OUT.exec(candidate)
    if (!match || match[1].trim() === '') continue
    // The marker must be a real comment. Without this, `const s = '// gate-region-ok: x'`
    // above an offence suppresses it with nothing a reviewer would ever read as an opt-out.
    // What this check does NOT close: see `OPT_OUT_SCOPE_LIMITS` above.
    if (insideStringLiteral(candidate, match.index)) continue
    return true
  }
  return false
}

/**
 * Find every un-opted-out raw region slice in `contents`.
 * @param {string} contents Whole file text.
 * @returns {{line: number, receiver: string, text: string, rule: string}[]} Offenders, in source order.
 */
export function findVacuousRegions(contents) {
  const lines = contents.split('\n')
  const offenders = []
  const flattenedWholeDocAliases = new Set()
  RAW_REGION_SLICE.lastIndex = 0
  for (const match of contents.matchAll(RAW_REGION_SLICE)) {
    // The reported line is where the receiver sits, which is the first line of a
    // Prettier-wrapped call and therefore the line a reader jumps to.
    const line = contents.slice(0, match.index).split('\n').length
    if (hasOptOut(lines, line)) continue
    offenders.push({ line, receiver: match[1], text: match[0] })
  }
  RAW_SECTION_DISTANCE_WINDOW.lastIndex = 0
  for (const match of contents.matchAll(RAW_SECTION_DISTANCE_WINDOW)) {
    const literal = Number(match[3] ?? match[4])
    const isDisplayHeadSlice = match[2] === '0' && match[4] !== undefined
    if (!Number.isFinite(literal) || (isDisplayHeadSlice && literal < 100)) continue
    const line = contents.slice(0, match.index).split('\n').length
    if (hasOptOut(lines, line)) continue
    offenders.push({
      line,
      receiver: match[1],
      text: match[0],
      rule: 'raw-section-window',
    })
  }
  RAW_SECTION_HEADING_TERMINATOR.lastIndex = 0
  for (const match of contents.matchAll(RAW_SECTION_HEADING_TERMINATOR)) {
    const line = contents.slice(0, match.index).split('\n').length
    if (hasOptOut(lines, line)) continue
    offenders.push({
      line,
      receiver: '',
      text: match[0],
      rule: 'raw-section-window',
    })
  }
  WHOLE_DOC_FLATTENED_ASSIGN.lastIndex = 0
  for (const match of contents.matchAll(WHOLE_DOC_FLATTENED_ASSIGN)) {
    if (WHOLE_DOC_NAMES.has(match[2])) flattenedWholeDocAliases.add(match[1])
  }
  DOES_NOT_MATCH_CALL.lastIndex = 0
  for (const match of contents.matchAll(DOES_NOT_MATCH_CALL)) {
    const subject = match[1]
    if (!WHOLE_DOC_NAMES.has(subject) && !flattenedWholeDocAliases.has(subject)) continue
    const line = contents.slice(0, match.index).split('\n').length
    if (hasOptOut(lines, line)) continue
    offenders.push({
      line,
      receiver: subject,
      text: match[0],
      rule: 'unfalsifiable-prose-pin',
    })
  }
  return offenders.sort((a, b) => a.line - b.line || a.text.localeCompare(b.text))
}

export function findMjsFiles(root, deps = {}) {
  const fsImpl = deps.fs || fs
  const results = []
  const walk = (dir) => {
    for (const entry of fsImpl.readdirSync(dir, { withFileTypes: true })) {
      if (entry.name === 'node_modules') continue
      const full = path.join(dir, entry.name)
      if (entry.isDirectory()) walk(full)
      else if (entry.name.endsWith('.mjs')) results.push(full)
    }
  }
  walk(root)
  return results.sort()
}

export function findVacuousRegionsInRepo(repoRoot, deps = {}) {
  const fsImpl = deps.fs || fs
  const excluded = new Set(SCAN_EXCLUSIONS.map((rel) => path.join(repoRoot, ...rel.split('/'))))
  const prosePinFiles = new Set(
    UNFALSIFIABLE_PROSE_PIN_FILES.map((rel) => path.join(repoRoot, ...rel.split('/'))),
  )
  return SCANNED_ROOTS.map((dir) => path.join(repoRoot, dir))
    .filter((dir) => fsImpl.existsSync(dir))
    .flatMap((root) => findMjsFiles(root, deps))
    .filter((file) => !excluded.has(file))
    .flatMap((file) => {
      const offenders = findVacuousRegions(fsImpl.readFileSync(file, 'utf8')).filter(
        (offender) => offender.rule !== 'unfalsifiable-prose-pin' || prosePinFiles.has(file),
      )
      return offenders.map((offender) => ({
        ...offender,
        file,
      }))
    })
}

function main() {
  const repoRoot = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')
  const offenders = findVacuousRegionsInRepo(repoRoot)
  if (offenders.length > 0) {
    console.error(
      'Raw slice/indexOf, raw section-window, or unfalsifiable prose-pin extraction found — use ' +
        'region()/sectionRegion() and scripts/prose-pin-lib.mjs, or add `// gate-region-ok: <reason>`:',
    )
    for (const offender of offenders) {
      console.error(
        `  - ${path.relative(repoRoot, offender.file)}:${offender.line} (${offender.rule ?? 'raw-region-slice'})`,
      )
    }
    process.exit(1)
  }
  // Qualified with the scanned roots on purpose: an unqualified "none found" reads as a
  // whole-tree verdict, and this gate looks at two directories. See SCANNED_ROOTS above.
  console.log(
    `No raw slice(indexOf(…)) region extractions or raw section windows in ${SCANNED_ROOTS.join(', ')} ` +
      `(fail-closed region()/sectionRegion() OK; unfalsifiable-prose-pin OK). Other roots are not scanned; parser-free raw-section-window does not trace hoisted bounds, and unfalsifiable-prose-pin cannot prove mutation relevance.`,
  )
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)

if (invokedDirectly) main()
