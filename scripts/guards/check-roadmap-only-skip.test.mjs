// bazel.yml's `roadmap-only` probe lets a PR that changes only the ls-roadmap output skip
// go-test, go-ledger, sqlc and go-lint. That is safe only while nothing in those jobs reads the
// roadmap files: a check that did would never run on the one PR that could turn it red, and main
// would go red under the next unrelated PR. This pins that premise for the affected selectors
// and for every registry guard.

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import { trackedFiles } from '../dev/tree-scan-lib.mjs'
import {
  needsBuildDriftCheck,
  selectBazelAffected,
  selectLedgerModules,
} from '../test-infra/select-affected-tests.mjs'
import { loadRegistry, scannedFiles } from './registry.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const WORKFLOW = 'bazel.yml'
const workflowText = fs.readFileSync(path.join(repoRoot, '.github/workflows', WORKFLOW), 'utf8')

function roadmapPrefix(text) {
  return text.match(/^ {10}ROADMAP_PREFIX: (\S+)$/m)?.[1]
}

function roadmapGatedJobs(text) {
  const jobs = []
  let current = null
  for (const line of (text.split(/^jobs:\n/m)[1] ?? '').split('\n')) {
    const job = line.match(/^ {2}([\w-]+):\s*$/)
    if (job) current = job[1]
    else if (current && /^ {4}if:.*needs\.roadmap-only\.outputs\.skip != 'true'/.test(line)) {
      jobs.push(current)
    }
  }
  return jobs
}

function roadmapReadsInSkippedJobs(records, text, tracked) {
  const prefix = roadmapPrefix(text)
  const gated = roadmapGatedJobs(text)
  return records
    .filter((record) => record.wiring.ci.workflow === WORKFLOW)
    .filter((record) => gated.includes(record.wiring.ci.job))
    .flatMap((record) =>
      scannedFiles(repoRoot, record.scans, { tracked })
        .filter((file) => file.startsWith(prefix))
        .map((file) => `${record.id} (${record.wiring.ci.job}) scans ${file}`),
    )
}

const tracked = trackedFiles(repoRoot)
const { records, errors } = await loadRegistry(repoRoot)

test('the probe skips exactly the feature-tier Go jobs, never a release-tier job', () => {
  assert.equal(roadmapPrefix(workflowText), 'docs/roadmap/')
  assert.deepEqual(
    roadmapGatedJobs(workflowText).sort(),
    ['go-ledger', 'go-lint', 'go-test', 'sqlc'],
    'a job that starts or stops reading roadmap-only must be reviewed against the guard below',
  )
})

test('the affected selectors give go-test and go-ledger nothing to run for roadmap files', () => {
  const prefix = roadmapPrefix(workflowText)
  const files = [...tracked.filter((file) => file.startsWith(prefix)), `${prefix}new-output.html`]
  assert.ok(files.length > 1, `no tracked file under ${prefix}`)
  assert.deepEqual(selectBazelAffected(files), {
    full: false,
    patterns: [],
    reason: 'no go-relevant changes',
  })
  assert.deepEqual(selectLedgerModules(files), { all: false, modules: [] })
  assert.equal(needsBuildDriftCheck(files), false)
})

test('no registry guard in a roadmap-skippable job scans the roadmap output', () => {
  assert.deepEqual(errors, [])
  assert.ok(
    records.some((record) => record.wiring.ci.workflow === WORKFLOW),
    'no bazel.yml-wired guard loaded, so the scan below would pass vacuously',
  )
  assert.deepEqual(roadmapReadsInSkippedJobs(records, workflowText, tracked), [])
})

test('a guard wired to a skippable job that scans docs/ is reported', () => {
  const seeded = {
    id: 'seeded-docs-scan',
    scans: ['docs/**'],
    wiring: { ci: { job: 'go-lint', workflow: WORKFLOW }, make: 'lint' },
  }
  const found = roadmapReadsInSkippedJobs([...records, seeded], workflowText, tracked)
  assert.ok(found.length > 0, 'the seeded docs/** scan was not reported')
  for (const line of found)
    assert.match(line, /^seeded-docs-scan \(go-lint\) scans docs\/roadmap\//)
})
