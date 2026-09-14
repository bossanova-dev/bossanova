#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

// A CI job's `outputs:` block is a DATA channel. Its right-hand side must be a value some step
// produced — `steps.<id>.outputs.<name>` — never one of the two status fields GitHub Actions
// exposes for a step, which report how the step finished rather than what it computed.
//
// Why that distinction is the whole gate (BOS-1242). Five workflows in this repository published
// a dorny/paths-filter job's output from the filter step's completion status. For that step the
// value is `success` on every run that does not fail the job, and a run that DOES fail the job
// skips its dependents anyway — so every consumer spelled `needs.check.outputs.status ==
// 'success'` was true whenever it was evaluated at all. The filter was computed, read once to
// decide whether to echo a line, and thrown away. Seven job conditions read that dead output.
// The jobs looked like gates in the check set and gated nothing.
//
// The correct spelling for gating on a status is a job's own `result`, as
// `.github/workflows/perform-production-release.yml` does with `needs.build.result == 'success'`.
// `result` is status vocabulary on a status channel; an `outputs.<name>` read is data vocabulary,
// and comparing it against status words is either always true or always false.
//
// PARSER-FREE, on purpose. `yaml` is declared only under `overrides` in package.json and is not
// installed, so there is no parser to reach for — the same constraint scripts/check-vacuous-
// regions.mjs works under, and this gate is modelled directly on it: a named scan root printed
// with the verdict, a required-reason opt-out, and one companion test file carrying the forbidden
// shapes verbatim as fixture text.
//
// Both rules anchor on EXPRESSION TEXT rather than on YAML block structure, so indentation style
// cannot blind them. RESIDUAL, stated rather than papered over: a folded or block scalar that
// splits an expression across lines is invisible to rule 1, and rule 1 does not itself verify
// that the entry it flagged sits under an `outputs:` key — it flags the whole-value shape
// wherever it appears, which is why the opt-out exists. Both limits are pinned by the suite.

// The two status fields a step exposes. Rule 1 is generated from this list, so a status field
// added to the vocabulary is one edit, not two. `outcome` has no call site in the tree today; it
// is forbidden anyway, because it is the same vocabulary and is what the next author reaches for
// once the first spelling is gone.
export const STEP_STATUS_FIELDS = ['conclusion', 'outcome']

// Status words a job output must never be compared against. A job output carries whatever string
// a step wrote to $GITHUB_OUTPUT, so a comparison against this vocabulary is a category error:
// either the output genuinely holds one of these words (in which case the opt-out documents it)
// or the comparison is a constant.
export const STATUS_VOCABULARY = ['success', 'failure', 'cancelled', 'skipped']

const stepStatusAlternation = STEP_STATUS_FIELDS.join('|')
const statusAlternation = STATUS_VOCABULARY.join('|')

// Rule 1 — `job-output-step-status`. A mapping entry whose ENTIRE value is a single
// `${{ steps.<id>.<status field> }}` expression. Whole-value is what makes this precise without a
// parser: the legitimate ways to read a step's status are comparisons (`if: steps.x.outcome ==
// 'failure'`) or interpolations inside a larger string (`run: echo "…${{ … }}…"`), and neither is
// a bare value. The optional matched quote pair allows the YAML-quoted spelling.
const JOB_OUTPUT_STEP_STATUS = new RegExp(
  String.raw`^\s*([A-Za-z_][\w.\-]*)\s*:\s*(['"]?)\$\{\{\s*steps\.([A-Za-z_][\w\-]*)\.(` +
    stepStatusAlternation +
    String.raw`)\s*\}\}\2\s*$`,
)

// Rule 2 — `status-compare-on-job-output`. A comparison between a `needs.<job>.outputs.<name>`
// read and a status literal, in either operand order.
const STATUS_COMPARE_AFTER = new RegExp(
  String.raw`needs\.([A-Za-z_][\w\-]*)\.outputs\.([A-Za-z_][\w\-]*)\s*(?:==|!=)\s*(['"])(?:` +
    statusAlternation +
    String.raw`)\3`,
)
const STATUS_COMPARE_BEFORE = new RegExp(
  String.raw`(['"])(?:` +
    statusAlternation +
    String.raw`)\1\s*(?:==|!=)\s*needs\.([A-Za-z_][\w\-]*)\.outputs\.([A-Za-z_][\w\-]*)`,
)

// The escape hatch, on the offending line or the line immediately above it. The reason is
// REQUIRED and must be non-empty — an unexplained opt-out is the next vacuous gate.
const OPT_OUT = /vacuous-job-gate-ok:(.*)$/

// The scan root, named in code and printed with the verdict so a pass can never read wider than
// what was actually looked at.
export const SCANNED_ROOT = '.github/workflows'
export const SCANNED_EXTENSIONS = ['.yml', '.yaml']

// The gate's own test must carry the forbidden shapes verbatim as fixture text, so it is exempt
// from the tree scan by EXACT repo-relative path.
//
// Be honest about what this entry does today: NOTHING. Measured, the shipped list and an empty
// list return byte-identical file sets, so the exemption currently filters nothing and its
// behaviour is exercised only through the suite's injected file list — which is also what proves
// the keying is by exact path rather than by basename or pattern.
//
// It is a forward guard, but only against BOTH widenings at once — not against either alone, and
// the two are blocked by different mechanisms:
//   - Widening SCANNED_EXTENSIONS alone cannot reach it: `findWorkflowFiles` builds each key as
//     `${SCANNED_ROOT}/${name}`, so the keys still start with `.github/workflows/` and none can
//     equal a `scripts/…` path.
//   - Widening SCANNED_ROOT alone cannot reach it either, but NOT because the key stops matching:
//     with the root at `scripts` the key would be byte-identical to the entry below. The extension
//     filter is what blocks it, and it runs BEFORE the key is built, so an `.mjs` name is dropped
//     while SCANNED_EXTENSIONS is still `.yml`/`.yaml`.
//   - Widen both — root to `scripts` and the extension set to admit `.mjs` — and this entry goes
//     live and does its job.
//
// It stays because the exemption is a policy statement — this file, and only this file, may carry
// the forbidden bytes — and pinning the list to exactly one entry is what stops that policy
// widening quietly. Nothing else may be added here: opt out a real call site instead.
export const SCAN_EXCLUSIONS = ['scripts/check-vacuous-job-gates.test.mjs']

/**
 * Index of the character that starts a YAML comment on `line`, or -1 when the line has none.
 *
 * A `#` inside a quoted scalar is content, not a comment, so an opt-out marker written inside a
 * string is NOT an opt-out. The scan is single-line and starts fresh on every line, which is all
 * a line-oriented gate needs; YAML's block scalars can still carry a `#` that this reads as a
 * comment, and that direction is harmless — it can only make a marker count, and a marker still
 * needs a non-empty reason to suppress anything.
 *
 * @param {string} line Raw source line.
 * @returns {number} Index of the comment `#`, or -1.
 */
export function commentStart(line) {
  let quote = null
  for (let i = 0; i < line.length; i++) {
    const ch = line[i]
    if (quote) {
      if (ch === quote) quote = null
      continue
    }
    if (ch === '"' || ch === "'") {
      quote = ch
      continue
    }
    // YAML needs whitespace (or line start) before an inline comment marker.
    if (ch === '#' && (i === 0 || /\s/.test(line[i - 1]))) return i
  }
  return -1
}

function hasOptOut(lines, lineNumber) {
  for (const offset of [1, 2]) {
    const candidate = lines[lineNumber - offset]
    if (typeof candidate !== 'string') continue
    const commentAt = commentStart(candidate)
    if (commentAt === -1) continue
    const match = OPT_OUT.exec(candidate.slice(commentAt))
    if (!match || match[1].trim() === '') continue
    return true
  }
  return false
}

/**
 * Find every un-opted-out vacuous job gate in one workflow's source text.
 *
 * @param {string} contents Whole workflow file text.
 * @returns {{line: number, rule: string, text: string}[]} Offenders, in source order.
 */
export function findVacuousJobGates(contents) {
  const lines = contents.split('\n')
  const offenders = []

  lines.forEach((rawLine, index) => {
    const lineNumber = index + 1
    const commentAt = commentStart(rawLine)
    const line = commentAt === -1 ? rawLine : rawLine.slice(0, commentAt)

    const publish = JOB_OUTPUT_STEP_STATUS.exec(line)
    if (publish && !hasOptOut(lines, lineNumber)) {
      offenders.push({
        line: lineNumber,
        rule: 'job-output-step-status',
        text: publish[0].trim(),
      })
    }

    const compare = STATUS_COMPARE_AFTER.exec(line) || STATUS_COMPARE_BEFORE.exec(line)
    if (compare && !hasOptOut(lines, lineNumber)) {
      offenders.push({
        line: lineNumber,
        rule: 'status-compare-on-job-output',
        text: compare[0].trim(),
      })
    }
  })

  return offenders
}

/**
 * List the workflow files the tree scan reads, as repo-relative POSIX paths.
 *
 * Exclusions are matched against the WHOLE repo-relative path, never against a basename and never
 * as a pattern, so an exemption cannot widen by accident. `deps.exclusions` exists so the suite
 * can exercise that keying directly: the shipped list names a file that does not sit under the
 * scan root, so without an injected list the mechanism would never run and would be the very
 * thing this gate exists to refuse — a check that passes without exercising its subject.
 *
 * @param {string} repoRoot Absolute repository root.
 * @param {{fs?: typeof fs, exclusions?: string[]}} [deps] Injected filesystem / exemption list.
 * @returns {string[]} Sorted repo-relative paths.
 */
export function findWorkflowFiles(repoRoot, deps = {}) {
  const fsImpl = deps.fs || fs
  const root = path.join(repoRoot, ...SCANNED_ROOT.split('/'))
  if (!fsImpl.existsSync(root)) return []
  const excluded = new Set(deps.exclusions || SCAN_EXCLUSIONS)
  return fsImpl
    .readdirSync(root)
    .filter((name) => SCANNED_EXTENSIONS.some((extension) => name.endsWith(extension)))
    .map((name) => `${SCANNED_ROOT}/${name}`)
    .filter((relative) => !excluded.has(relative))
    .sort()
}

/**
 * Scan the real workflow tree.
 *
 * @param {string} repoRoot Absolute repository root.
 * @param {{fs?: typeof fs, exclusions?: string[]}} [deps] Injected filesystem / exemption list.
 * @returns {{files: string[], offenders: {file: string, line: number, rule: string, text: string}[]}}
 */
export function findVacuousJobGatesInRepo(repoRoot, deps = {}) {
  const fsImpl = deps.fs || fs
  const files = findWorkflowFiles(repoRoot, deps)
  const offenders = files.flatMap((relative) => {
    const contents = fsImpl.readFileSync(path.join(repoRoot, ...relative.split('/')), 'utf8')
    return findVacuousJobGates(contents).map((offender) => ({ ...offender, file: relative }))
  })
  return { files, offenders }
}

export const REMEDY = [
  'A job `outputs:` value must be a value a step PRODUCED (steps.<id>.outputs.<name>), and a',
  "job output must never be compared against status words. To gate on a status use the job's",
  "own `result` (needs.<job>.result == 'success'). If a site is genuinely correct, annotate it",
  'with `# vacuous-job-gate-ok: <reason>` on its own line or the line above.',
].join('\n')

export function renderVerdict({ files, offenders }) {
  if (files.length === 0) {
    return {
      ok: false,
      text:
        `Scanned corpus is EMPTY: no ${SCANNED_EXTENSIONS.join('/')} files under ${SCANNED_ROOT}. ` +
        'Zero files read is not a clean tree — fix the scan root before trusting this gate.',
    }
  }
  if (offenders.length > 0) {
    return {
      ok: false,
      text: [
        `Vacuous job gate(s) found in ${SCANNED_ROOT} (${files.length} workflow files read):`,
        ...offenders.map(
          (offender) => `  - ${offender.file}:${offender.line} (${offender.rule}) ${offender.text}`,
        ),
        '',
        REMEDY,
      ].join('\n'),
    }
  }
  return {
    ok: true,
    text:
      `No vacuous job gates in ${SCANNED_ROOT} (${files.length} workflow files read). ` +
      'Only that directory is scanned, and the two rules are parser-free: a status expression ' +
      'split across lines by a folded or block scalar is not seen, and rule 1 flags the ' +
      'whole-value shape wherever it appears rather than proving the entry sits under `outputs:`.',
  }
}

function main() {
  const repoRoot = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')
  const verdict = renderVerdict(findVacuousJobGatesInRepo(repoRoot))
  if (!verdict.ok) {
    console.error(verdict.text)
    process.exit(1)
  }
  console.log(verdict.text)
}

if (isMainModule(import.meta.url)) main()
