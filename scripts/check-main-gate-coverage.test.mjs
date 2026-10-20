import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const workflowDir = path.join(repoRoot, '.github/workflows')
const scriptsWorkflowPath = path.join(workflowDir, 'test-scripts.yml')
const scriptsWorkflow = fs.readFileSync(scriptsWorkflowPath, 'utf8')

const intentionallyBranchIgnored = new Map([
  [
    'bazel.yml',
    'pinned because the whole-graph Bazel gate is the cost tier docs/build-and-ci.md deliberately keeps off main',
  ],
  [
    'test-docs.yml',
    'pinned because services/docs inputs are PR-scoped; docs MCP drift is covered by the scripts workflow fixed here',
  ],
  [
    'test-go.yml',
    'pinned because the native Go race/coverage tier is the main-push cost center documented as release-tier only',
  ],
  [
    'test-hermes-plugin.yml',
    'pinned because the Hermes plugin suite is PR-scoped; the Go renderer that embeds it runs in bazel.yml',
  ],
  [
    'test-marketing.yml',
    'pinned because marketing build inputs are PR-scoped and do not carry generated repo-wide hygiene pins',
  ],
  [
    'test-plugin-distribution.yml',
    'pinned because plugin distribution inputs are PR-scoped and are rerun at production release time',
  ],
  [
    'test-proto.yml',
    'pinned because proto generated artifacts are owned end-to-end by one PR rather than by repo-wide glob inputs',
  ],
  [
    'test-warehouse.yml',
    'pinned because warehouse dbt inputs are PR-scoped; the models run hourly from production, not main',
  ],
  [
    'test-scripts.yml',
    'pinned because release PRs run it path-filter-free and a nightly schedule covers main',
  ],
  ['test-web-real-stack.yml', 'pinned because the real-stack suite reruns on every release PR'],
  [
    'test-web.yml',
    'pinned because web inputs are PR-scoped and the full web tier is rerun on release PRs',
  ],
])

function readWorkflows() {
  return fs
    .readdirSync(workflowDir)
    .filter((name) => name.endsWith('.yml'))
    .map((name) => [name, fs.readFileSync(path.join(workflowDir, name), 'utf8')])
}

function onBlockOf(contents) {
  return contents.match(/^on:\n((?:[ \t].*\n|\n)*)/m)?.[1] ?? ''
}

// The scripts workflow no longer runs on main merge pushes (2026-10 CI pass). Its replacement
// contract is what keeps a main-only breakage visible: every release PR runs it path-filter-free,
// and a nightly schedule runs it on main under its own conclusion.
// The gate: the scripts workflow text keeps its release-PR and nightly triggers.
function assertRunsOnReleasePRsAndNightly(workflowText) {
  const onBlock = onBlockOf(workflowText)
  assert.notEqual(onBlock, '', 'could not locate test-scripts.yml `on:` block')

  const pullRequest = onBlock.match(/^ {2}pull_request:\n((?: {4,}.*\n)*)/m)?.[1] ?? ''
  assert.match(pullRequest, /- staging$/m, 'release PRs into staging must run the scripts suite')
  assert.match(
    pullRequest,
    /- production$/m,
    'release PRs into production must run the scripts suite',
  )
  assert.doesNotMatch(
    pullRequest,
    /paths:/,
    'release PRs must run the scripts suite path-filter-free',
  )

  assert.match(
    onBlock,
    /^ {2}schedule:\n {4}(?:#.*\n {4})*- cron: '\d+ \d+ \* \* \*'$/m,
    'the scripts suite must keep a daily scheduled run on main',
  )
}

test('scripts workflow runs on every release PR and nightly on main', () => {
  assertRunsOnReleasePRsAndNightly(scriptsWorkflow)
})

test('a scripts workflow that drops the production release-PR trigger fails the gate', () => {
  const trigger = '      - production\n  schedule:\n'
  assert.ok(scriptsWorkflow.includes(trigger), 'seed anchor moved: update this fixture')
  const seeded = scriptsWorkflow.replace(trigger, '  schedule:\n')
  assert.throws(
    () => assertRunsOnReleasePRsAndNightly(seeded),
    /release PRs into production must run the scripts suite/,
  )
})

test('scripts scheduled runs are never cancelled', () => {
  assert.match(
    scriptsWorkflow,
    /^  cancel-in-progress: \$\{\{ github\.event_name != 'schedule' \}\}$/m,
    'the nightly main run must record a terminal conclusion instead of being cancelled',
  )
})

test('deliberate main-exclusion workflow allowlist stays explicit', () => {
  const actual = readWorkflows()
    .filter(([, contents]) => /^ {4}branches-ignore:/m.test(contents))
    .map(([name]) => name)
    .sort()
  const expected = [...intentionallyBranchIgnored.keys()].sort()

  assert.deepEqual(
    actual,
    expected,
    'update this allowlist and its reason comments whenever a workflow starts or stops excluding main',
  )

  for (const [name, reason] of intentionallyBranchIgnored) {
    assert.match(reason, /\bpinned because\b/, `${name} needs an explicit reason comment`)
  }
})
