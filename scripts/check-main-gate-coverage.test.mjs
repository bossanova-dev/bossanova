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

test('scripts workflow push trigger includes main', () => {
  const onBlock = scriptsWorkflow.match(/^on:\n((?:[ \t].*\n|\n)*)/m)?.[1] ?? ''

  assert.notEqual(onBlock, '', 'could not locate test-scripts.yml `on:` block')
  assert.match(onBlock, /^ {2}push:$/m)
  assert.doesNotMatch(
    onBlock,
    /^ {4}branches-ignore:/m,
    'test-scripts.yml must not exclude main from its push trigger',
  )
  assert.doesNotMatch(
    onBlock,
    /^ {4}branches:/m,
    'test-scripts.yml must not restrict its push trigger to non-main branches',
  )
})

test('scripts workflow keeps main runs from being cancelled', () => {
  assert.match(
    scriptsWorkflow,
    /^  cancel-in-progress: \$\{\{ github\.ref != 'refs\/heads\/main' \}\}$/m,
    'scripts main runs must record a terminal conclusion instead of being cancelled by the next merge',
  )
  assert.doesNotMatch(
    scriptsWorkflow,
    /^  cancel-in-progress: true$/m,
    'scripts workflow must not use bare cancel-in-progress: true',
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
