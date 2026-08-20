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
// TWO RULES, BOTH STRUCTURAL. This gate flags the MEASUREMENT, not the comparison. That is a
// deliberate narrowing: a detector for the comparison itself — an assertion whose operand is
// compared against a SCREAMING_SNAKE constant — was prototyped over this exact scope and
// produced eight false positives, none of them size gates. They were string fixtures
// containing `<TICKET`, byte-OFFSET ordering assertions (`gateAt > PHASE_4_SECTION`), and a
// batch-size bound (`b.length <= GO_BATCH`). A detector that guesses is a detector that
// quietly stops detecting, so the comparison rule was dropped rather than shipped with a
// standing false-positive tax and the opt-outs that would follow. What that costs is stated
// in RESIDUAL below — this gate is not the whole invariant, and does not claim to be.
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
]

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
//   2. It does not catch a one-sided comparison written over a `measureFile` result, for the
//      false-positive reason recorded above. Review catches that shape, not this gate.
//   3. It looks at one directory and one filename pattern. A size ratchet written in a file
//      that does not match SCANNED_NAME is not scanned at all.
//   4. The two rules match TWO SPELLINGS, not the concept of measuring. Hand-rolled
//      measurement written any other way — `fs.statSync(p).size`, `readFileSync(p).length`,
//      `[...text].length`, `split(/\r?\n/).length`, or a helper that wraps any of them — is
//      invisible here, with no opt-out marker to make the omission visible either. This is the
//      same failure mode the ticket exists to remove, one level up: a structural detector's
//      verdict is bounded by the shapes it enumerates, so read the rule list, not the headline.
export const RESIDUAL =
  'a green run means no gate in scope measures bytes or lines by hand IN THE TWO SPELLINGS ' +
  'these rules match — not that any pin is correct, not that a comparison over a ' +
  'measureFile() result is two-sided, not that a size ratchet outside SCANNED_NAME exists at ' +
  'all, and not that another measurement spelling (statSync().size, readFileSync().length, ' +
  'split(/\\r?\\n/).length, or a wrapper around them) is absent'

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
    rule.pattern.lastIndex = 0
    for (const match of contents.matchAll(rule.pattern)) {
      const line = contents.slice(0, match.index).split(String.fromCharCode(10)).length
      if (hasOptOut(lines, line)) continue
      offenders.push({ line, remedy: rule.remedy, rule: rule.name, text: match[0] })
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
