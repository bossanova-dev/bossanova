#!/usr/bin/env node

// Falsification suite for the vacuous-job-gate detector (BOS-1242).
//
// Discipline: docs/solutions/best-practices/feed-a-new-gate-the-values-it-exists-to-reject.md.
// This ticket is about gates that pass while never exercising their subject, so a new gate that
// asserted nothing would be the same defect in a new file. Every forbidden shape below is the
// PRE-CHANGE text of a real workflow, copied verbatim, and asserted to be reported — falsification
// by ADDING the defect, not by hoping its absence means detection.
//
// Both directions are pinned. A detector that fired on every job output would be as useless as one
// that fired on none, so this file carries one negative fixture per `not a defect` row of the
// ticket's repo-wide enumeration — the three `steps.probe.outputs.skip` publishers, the
// `fromJSON` matrix publisher, the two release-metadata publishers, and the correct status
// spelling that gates on a job `result`.
//
// SAFETY: every probe runs against an in-memory fixture string or an injected stub filesystem.
// The two real-tree cases only READ. Nothing here writes or mutates a source file.
//
// This file is the one entry in SCAN_EXCLUSIONS, because it carries the forbidden shapes verbatim
// as fixture text. That exclusion, and the fact that it holds exactly one entry, are pinned below.

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  SCANNED_EXTENSIONS,
  SCANNED_ROOT,
  SCAN_EXCLUSIONS,
  STATUS_VOCABULARY,
  STEP_STATUS_FIELDS,
  commentStart,
  findVacuousJobGates,
  findVacuousJobGatesInRepo,
  findWorkflowFiles,
  renderVerdict,
} from './check-vacuous-job-gates.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// --- The shapes the gate exists to find -------------------------------------------------

test('reports a job output published from a step completion status', () => {
  // Verbatim from .github/workflows/test-scripts.yml:112-115 as it stood at 512a32996, the
  // commit before this ticket deleted the job.
  const source = [
    'jobs:',
    '  check:',
    '    runs-on: ubicloud',
    '    outputs:',
    '      status: ${{ steps.changes.conclusion }}',
    '    steps:',
    '      - uses: actions/checkout@v6',
  ].join('\n')

  const offenders = findVacuousJobGates(source)
  assert.equal(
    offenders.length,
    1,
    'the vacuous job gate detector must report the step-status output shape exactly once',
  )
  assert.equal(offenders[0].rule, 'job-output-step-status')
  assert.equal(offenders[0].line, 5)
  assert.equal(offenders[0].text, 'status: ${{ steps.changes.conclusion }}')
})

test('reports the other step status field the same way', () => {
  // No call site exists in the tree; the rule forbids it anyway, because it is the same status
  // vocabulary and is the spelling the next author reaches for once the first one is gone.
  const source = ['    outputs:', '      status: ${{ steps.changes.outcome }}'].join('\n')

  const offenders = findVacuousJobGates(source)
  assert.equal(
    offenders.length,
    1,
    'the vacuous job gate detector must report both step status fields, not only the first',
  )
  assert.equal(offenders[0].rule, 'job-output-step-status')
})

test('reports a job condition comparing a job output against status vocabulary', () => {
  // Verbatim from .github/workflows/test-scripts.yml:180-183 at 512a32996.
  const source = [
    '  scripts:',
    '    runs-on: ubicloud',
    '    needs: check',
    "    if:    needs.check.outputs.status == 'success'",
  ].join('\n')

  const offenders = findVacuousJobGates(source)
  assert.equal(
    offenders.length,
    1,
    'the vacuous job gate detector must report the status-compare-on-job-output shape',
  )
  assert.equal(offenders[0].rule, 'status-compare-on-job-output')
  assert.equal(offenders[0].line, 4)
  assert.equal(offenders[0].text, "needs.check.outputs.status == 'success'")
})

test('reports the status comparison with the operands reversed', () => {
  const source = ["    if: ${{ 'failure' != needs.check.outputs.status }}"].join('\n')

  const offenders = findVacuousJobGates(source)
  assert.equal(
    offenders.length,
    1,
    'the vacuous job gate detector must not be defeated by operand order',
  )
  assert.equal(offenders[0].rule, 'status-compare-on-job-output')
})

test('reports every status word in the vocabulary, not just the two that shipped', () => {
  for (const word of STATUS_VOCABULARY) {
    const offenders = findVacuousJobGates(`    if: needs.check.outputs.status == '${word}'`)
    assert.equal(
      offenders.length,
      1,
      `the vacuous job gate detector must report the '${word}' comparison`,
    )
  }
})

test('reports both rules independently when one workflow carries both', () => {
  const source = [
    '    outputs:',
    '      status: ${{ steps.changes.conclusion }}',
    '  scripts:',
    "    if:    needs.check.outputs.status == 'success'",
  ].join('\n')

  assert.deepEqual(
    findVacuousJobGates(source).map((offender) => offender.rule),
    ['job-output-step-status', 'status-compare-on-job-output'],
    'the vacuous job gate detector must report the publisher and the consumer separately',
  )
})

// --- The shapes it must NOT find --------------------------------------------------------
//
// One per `not a defect` row of the ticket's enumeration. A detector that flagged every job
// output would fail here rather than pass.

test('a job output publishing a value a step computed is not a finding', () => {
  // bazel.yml:35-36, bazel-linux-smoke.yml:63-64, codeql.yml:36-37 — a real data channel, and
  // its consumer compares it against that value's own vocabulary rather than against a status.
  const source = [
    '    outputs:',
    '      skip: ${{ steps.probe.outputs.skip }}',
    '  build:',
    "    if: ${{ !cancelled() && needs.dedup.outputs.skip != 'true' }}",
  ].join('\n')

  assert.deepEqual(findVacuousJobGates(source), [], 'a real data channel is not a vacuous job gate')
})

test('a JSON matrix output consumed through fromJSON is not a finding', () => {
  // security.yml:36-37 and :53 — a value no status string could satisfy.
  const source = [
    '    outputs:',
    '      modules: ${{ steps.gen.outputs.modules }}',
    '      matrix:',
    '        module: ${{ fromJSON(needs.matrix.outputs.modules) }}',
  ].join('\n')

  assert.deepEqual(findVacuousJobGates(source), [], 'a fromJSON matrix fan-out is not a finding')
})

test('release-metadata outputs and their consumer are not a finding', () => {
  // perform-production-release.yml:20-22 and :52, mirrored in perform-staging-release.yml.
  const source = [
    '    outputs:',
    '      version: ${{ steps.semantic.outputs.new_release_version }}',
    '      released: ${{ steps.semantic.outputs.new_release_published }}',
    '  images:',
    "    if: needs.version.outputs.released == 'true'",
  ].join('\n')

  assert.deepEqual(findVacuousJobGates(source), [], 'release metadata is not a vacuous job gate')
})

test('gating on a job result — the correct status spelling — is not a finding', () => {
  // perform-production-release.yml:542. `result` is a status channel, so status vocabulary
  // belongs there; this is the contrast the rules are built on and must never be flagged.
  const source = [
    "    if: always() && needs.version.outputs.released == 'true' && needs.build.result == 'success'",
  ].join('\n')

  assert.deepEqual(
    findVacuousJobGates(source),
    [],
    'gating on a job result is not a vacuous job gate',
  )
})

test('a step status read inside a comparison or a larger string is not a finding', () => {
  // Documented scope limit, asserted rather than commented: rule 1 is a WHOLE-VALUE rule, which
  // is what keeps it parser-free without flagging the legitimate ways to read a step's status.
  const source = [
    "      - if: steps.changes.outcome == 'failure'",
    '        run: echo "filter finished ${{ steps.changes.conclusion }}"',
  ].join('\n')

  assert.deepEqual(findVacuousJobGates(source), [], 'legitimate step status reads are not findings')
})

// --- The opt-out ------------------------------------------------------------------------

test('an opt-out with a reason suppresses the finding, on the line above and inline', () => {
  const above = [
    '    outputs:',
    '      # vacuous-job-gate-ok: the probe deliberately publishes its own completion status',
    '      status: ${{ steps.changes.conclusion }}',
  ].join('\n')
  assert.deepEqual(
    findVacuousJobGates(above),
    [],
    'a reasoned opt-out on the line above suppresses',
  )

  const inline = [
    '    outputs:',
    '      status: ${{ steps.changes.conclusion }} # vacuous-job-gate-ok: reviewed, see BOS-1242',
  ].join('\n')
  assert.deepEqual(findVacuousJobGates(inline), [], 'a reasoned inline opt-out suppresses')
})

test('an opt-out without a non-empty reason is rejected', () => {
  for (const marker of ['# vacuous-job-gate-ok:', '# vacuous-job-gate-ok:    ']) {
    const source = [
      '    outputs:',
      `      ${marker}`,
      '      status: ${{ steps.changes.conclusion }}',
    ].join('\n')
    const offenders = findVacuousJobGates(source)
    assert.equal(
      offenders.length,
      1,
      `an unexplained opt-out (${JSON.stringify(marker)}) must not suppress a vacuous job gate`,
    )
  }
})

test('an opt-out marker inside a quoted scalar is not an opt-out', () => {
  const source = [
    '    outputs:',
    '      note: "# vacuous-job-gate-ok: text a reviewer would never read as an opt-out"',
    '      status: ${{ steps.changes.conclusion }}',
  ].join('\n')

  assert.equal(
    findVacuousJobGates(source).length,
    1,
    'a marker inside a string must not suppress a vacuous job gate',
  )
})

test('commentStart ignores a hash inside a quoted scalar', () => {
  assert.equal(commentStart('      key: "a # b"'), -1)
  assert.equal(commentStart("      key: 'a # b' # real"), 19)
  assert.equal(commentStart('      key: value'), -1)
  assert.equal(commentStart('# whole-line comment'), 0)
})

// --- The corpus -------------------------------------------------------------------------

test('the real workflow tree is clean, and the corpus it was judged on is non-empty', () => {
  const { files, offenders } = findVacuousJobGatesInRepo(repoRoot)

  assert.deepEqual(offenders, [], 'the real workflow tree must carry no vacuous job gate')
  assert.ok(files.length > 0, 'zero workflow files read is not a clean tree')
  assert.deepEqual(
    files,
    fs
      .readdirSync(path.join(repoRoot, SCANNED_ROOT))
      .filter((name) => SCANNED_EXTENSIONS.some((extension) => name.endsWith(extension)))
      .map((name) => `${SCANNED_ROOT}/${name}`)
      .sort(),
    'the scan must read every workflow file on disk',
  )
})

test('the success line names the scanned directory and the count read from disk', () => {
  const { files, offenders } = findVacuousJobGatesInRepo(repoRoot)
  const verdict = renderVerdict({ files, offenders })

  assert.equal(verdict.ok, true)
  assert.match(verdict.text, new RegExp(SCANNED_ROOT.replace(/[.]/g, '\\.')))
  assert.match(verdict.text, new RegExp(`\\b${files.length} workflow files read\\b`))
  // Derived from disk, not written into the source: the count moves with the corpus.
  assert.notEqual(files.length, 0)
})

test('an empty corpus fails closed instead of reading as a clean tree', () => {
  const verdict = renderVerdict({ files: [], offenders: [] })

  assert.equal(verdict.ok, false, 'zero files read must not be a pass')
  assert.match(verdict.text, /EMPTY/)
})

test('a missing scan root yields an empty corpus rather than a green scan', () => {
  const stub = { existsSync: () => false, readdirSync: () => [], readFileSync: () => '' }
  const { files, offenders } = findVacuousJobGatesInRepo('/nowhere', { fs: stub })

  assert.deepEqual(files, [])
  assert.deepEqual(offenders, [])
  assert.equal(renderVerdict({ files, offenders }).ok, false)
})

// --- The exemption ----------------------------------------------------------------------

test('the exemption list holds exactly the detector own test file, by exact path', () => {
  assert.deepEqual(SCAN_EXCLUSIONS, ['scripts/check-vacuous-job-gates.test.mjs'])
  for (const entry of SCAN_EXCLUSIONS) {
    assert.doesNotMatch(entry, /[*?[\]]/, 'exemptions are exact paths, never patterns')
    assert.ok(
      fs.existsSync(path.join(repoRoot, entry)),
      'an exemption naming a path that does not exist is a stale exemption',
    )
  }
})

test('the exemption drops a file by exact repo-relative path, not by basename', () => {
  // The shipped exemption is a forward guard only against widening the scan root AND the extension
  // set together — see the SCAN_EXCLUSIONS comment in check-vacuous-job-gates.mjs for why neither
  // widening reaches it alone. Its one entry is an .mjs file that does not sit under the scan root,
  // so nothing on the real tree exercises the mechanism. Injecting the list is what keeps this from
  // being a check that never runs against its own subject.
  const stub = {
    existsSync: () => true,
    readdirSync: () => ['real.yml', 'fixtures.yml'],
    readFileSync: () => '      status: ${{ steps.changes.conclusion }}',
  }

  assert.deepEqual(
    findWorkflowFiles('/repo', { fs: stub, exclusions: [`${SCANNED_ROOT}/fixtures.yml`] }),
    [`${SCANNED_ROOT}/real.yml`],
    'an exact repo-relative exemption must drop exactly that file',
  )

  assert.deepEqual(
    findWorkflowFiles('/repo', { fs: stub, exclusions: ['fixtures.yml'] }),
    [`${SCANNED_ROOT}/fixtures.yml`, `${SCANNED_ROOT}/real.yml`],
    'a bare basename is not an exemption; the key is the whole repo-relative path',
  )

  assert.deepEqual(
    findWorkflowFiles('/repo', { fs: stub, exclusions: [`${SCANNED_ROOT}/*.yml`] }),
    [`${SCANNED_ROOT}/fixtures.yml`, `${SCANNED_ROOT}/real.yml`],
    'exemptions are exact paths, so a pattern exempts nothing rather than exempting everything',
  )

  // And the shipped list cannot reach the corpus through the extension filter either, so the
  // fixture text in this file can never redden the real-tree scan.
  assert.deepEqual(
    SCAN_EXCLUSIONS.filter((entry) =>
      SCANNED_EXTENSIONS.some((extension) => entry.endsWith(extension)),
    ),
    [],
  )
})

// --- CI reachability --------------------------------------------------------------------

test('the scripts workflow push-path filter covers every file the detector scans', () => {
  const workflow = fs.readFileSync(
    path.join(repoRoot, '.github/workflows/test-scripts.yml'),
    'utf8',
  )
  const patterns = extractPushPaths(workflow)
  assert.ok(patterns.length > 0, 'test-scripts.yml must declare push paths')

  const { files } = findVacuousJobGatesInRepo(repoRoot)
  const uncovered = files.filter((file) => !patterns.some((pattern) => globMatches(pattern, file)))

  assert.deepEqual(
    uncovered,
    [],
    'the gate would be blind in CI to workflow edits it scans; widen test-scripts.yml on.push.paths',
  )
})

test('globMatches distinguishes a directory glob from the single-file entry it replaced', () => {
  assert.equal(globMatches('.github/workflows/**', '.github/workflows/bazel.yml'), true)
  assert.equal(
    globMatches('.github/workflows/test-scripts.yml', '.github/workflows/bazel.yml'),
    false,
  )
  assert.equal(globMatches('scripts/**', '.github/workflows/bazel.yml'), false)
  assert.equal(globMatches('**/*_test.go', 'services/bosso/a_test.go'), true)
})

// --- helpers ----------------------------------------------------------------------------

function extractPushPaths(text) {
  const lines = text.split('\n')
  const pushLine = lines.findIndex((line) => /^\s{2}push:\s*$/.test(line))
  assert.notEqual(pushLine, -1, 'workflow push block not found')
  const pathsLine = lines.findIndex(
    (line, index) => index > pushLine && /^\s{4}paths:\s*$/.test(line),
  )
  assert.notEqual(pathsLine, -1, 'workflow push.paths block not found')

  const items = []
  for (let i = pathsLine + 1; i < lines.length; i++) {
    const line = lines[i]
    if (line.trim() === '' || line.trimStart().startsWith('#')) continue
    const item = line.match(/^\s{6}-\s+(.+?)\s*$/)
    if (!item) break
    items.push(item[1].replace(/^["'](.*)["']$/, '$1'))
  }
  return items
}

function globMatches(pattern, file) {
  const source = pattern
    .split('**')
    .map((chunk) => chunk.split('*').map(escapeRegExp).join('[^/]*'))
    .join('.*')
  return new RegExp(`^${source}$`).test(file)
}

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

// Vocabulary pins: a widening of either list is a deliberate edit that shows up here.
test('the rule vocabularies stay exactly what the enumeration adjudicated', () => {
  assert.deepEqual(STEP_STATUS_FIELDS, ['conclusion', 'outcome'])
  assert.deepEqual(STATUS_VOCABULARY, ['success', 'failure', 'cancelled', 'skipped'])
})
