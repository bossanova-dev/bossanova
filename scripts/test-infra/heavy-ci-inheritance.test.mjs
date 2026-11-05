import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { classifyLines, keyValue } from '../dev/workflow-yaml-lib.mjs'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const action = fs.readFileSync(
  path.join(root, '.github/actions/skip-if-sha-already-green/action.yml'),
  'utf8',
)
const lines = classifyLines(action)
const runIndex = lines.findIndex(
  (line) => line.kind === 'structural' && keyValue(line.text)?.key === 'run',
)
assert.ok(runIndex >= 0, 'dedup action must contain an executable probe')
const runLine = lines[runIndex]
const script = lines
  .slice(runIndex + 1)
  .filter((line) => line.kind === 'scalar')
  .map((line) => line.raw.slice(runLine.indent + 2))
  .join('\n')
const repo = 'example/project'
const url = (id) => `https://github.com/${repo}/actions/runs/${id}`
const run = (id, event = 'workflow_dispatch', overrides = {}) => ({
  id,
  name: 'heavy suite',
  event,
  conclusion: 'success',
  html_url: url(id),
  ...overrides,
})
const heavy = (conclusion = 'success', merge = 'success') => ({
  name: 'go-test',
  conclusion,
  steps: [{ name: 'Run ./.github/actions/checkout-pr-merge', conclusion: merge }],
})
const dedup = { name: 'dedup', conclusion: 'success' }

function probe(
  t,
  {
    runs = [run(10)],
    jobs = { 10: [heavy()] },
    events = 'workflow_dispatch,pull_request',
    failRuns = false,
    failJobs = false,
    malformedRuns = false,
    malformedJobs = false,
  } = {},
) {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'heavy-ci-inheritance-'))
  t.after(() => fs.rmSync(temp, { recursive: true, force: true, maxRetries: 3 }))
  fs.writeFileSync(
    path.join(temp, 'fixture.json'),
    JSON.stringify({ runs, jobs, failRuns, failJobs, malformedRuns, malformedJobs }),
  )
  fs.writeFileSync(
    path.join(temp, 'gh'),
    `#!/usr/bin/env node
const fs = require('node:fs')
const path = require('node:path')
const fixture = JSON.parse(fs.readFileSync(path.join(__dirname, 'fixture.json'), 'utf8'))
const endpoint = process.argv[3]
fs.appendFileSync(path.join(__dirname, 'requests'), endpoint + '\\n')
const jobRun = endpoint.match(/\\/runs\\/(\\d+)\\/jobs/)
if (jobRun ? fixture.failJobs : fixture.failRuns) process.exit(1)
if (jobRun ? fixture.malformedJobs : fixture.malformedRuns) process.stdout.write('invalid JSON')
else process.stdout.write(JSON.stringify(jobRun ? {jobs: fixture.jobs[jobRun[1]] ?? []} : {workflow_runs: fixture.runs}))
`,
    { mode: 0o755 },
  )
  const output = path.join(temp, 'output')
  const result = spawnSync('/bin/bash', ['-c', script], {
    encoding: 'utf8',
    env: {
      ...process.env,
      PATH: `${temp}:${process.env.PATH}`,
      HEAD_SHA: 'abc123',
      WORKFLOW_NAME: 'heavy suite',
      RUN_ID: '999',
      EVENTS: events,
      REPO: repo,
      GITHUB_OUTPUT: output,
    },
  })
  assert.equal(result.status, 0, result.stderr)
  const outputs = Object.fromEntries(
    fs
      .readFileSync(output, 'utf8')
      .trim()
      .split('\n')
      .map((line) => line.split('=')),
  )
  const requests = fs.readFileSync(path.join(temp, 'requests'), 'utf8').trim().split('\n')
  assert.equal(requests[0], `repos/${repo}/actions/runs?head_sha=abc123&per_page=100`)
  return { outputs, requests, stdout: result.stdout }
}

function assertNotInherited(result) {
  assert.equal(result.outputs.skip, 'false')
  assert.equal(result.outputs['run-url'], undefined)
  assert.doesNotMatch(result.stdout, /::notice::/)
}

test('expanded inheritance accepts a dispatch with successful heavy work and names its run', (t) => {
  const result = probe(t)
  assert.equal(result.outputs.skip, 'true')
  assert.equal(result.outputs['run-url'], url(10))
  assert.match(result.stdout, /::notice::/)
  assert.ok(result.stdout.includes(url(10)))
  assert.equal(result.requests[1], `repos/${repo}/actions/runs/10/jobs?per_page=100`)
})

for (const [name, jobs] of [
  ['all heavy jobs skipped', [dedup, heavy('skipped')]],
  ['only dedup succeeded', [dedup]],
  ['heavy job queued', [dedup, heavy(null)]],
  ['heavy job failed', [dedup, heavy('failure')]],
  ['empty job response', []],
]) {
  test(`expanded inheritance rejects successful PR workflow with ${name}`, (t) => {
    assertNotInherited(probe(t, { runs: [run(10, 'pull_request')], jobs: { 10: jobs } }))
  })
}

test('expanded inheritance accepts a successful PR run with completed heavy work', (t) => {
  assert.equal(
    probe(t, { runs: [run(10, 'pull_request')], jobs: { 10: [heavy('success', 'skipped')] } })
      .outputs.skip,
    'true',
  )
})

for (const name of ['roadmap-only', 'future-light-probe']) {
  test(`successful ${name} cannot certify skipped draft tests`, (t) => {
    assertNotInherited(
      probe(t, {
        runs: [run(10, 'pull_request')],
        jobs: { 10: [dedup, { name, conclusion: 'success' }, heavy('skipped')] },
      }),
    )
  })
}

for (const merge of ['skipped', 'failure', null]) {
  test(`dispatch without successful merge checkout (${merge}) cannot certify a merge`, (t) => {
    assertNotInherited(probe(t, { jobs: { 10: [heavy('success', merge)] } }))
  })
}
test('dispatch with no checkout provenance cannot certify a merge', (t) => {
  assertNotInherited(probe(t, { jobs: { 10: [{ name: 'go-test', conclusion: 'success' }] } }))
})

test('default release inheritance retains workflow-success behavior without querying jobs', (t) => {
  const result = probe(t, {
    events: 'pull_request',
    runs: [run(10, 'pull_request')],
    jobs: { 10: [dedup, heavy('skipped')] },
  })
  assert.equal(result.outputs.skip, 'true')
  assert.equal(result.outputs['run-url'], url(10))
  assert.equal(result.requests.length, 1)
})

test('default release inheritance excludes feature dispatches', (t) => {
  const result = probe(t, { events: 'pull_request' })
  assertNotInherited(result)
  assert.equal(result.requests.length, 1)
})

for (const failure of ['failRuns', 'failJobs', 'malformedRuns', 'malformedJobs']) {
  test(`${failure} falls back to running heavy work`, (t) =>
    assertNotInherited(probe(t, { [failure]: true })))
}

test('event, workflow, conclusion and current-run filters exclude ineligible successes', (t) => {
  const result = probe(t, {
    runs: [
      run(999),
      run(10, 'push'),
      run(11, 'workflow_dispatch', { name: 'another suite' }),
      run(12, 'pull_request', { conclusion: 'failure' }),
    ],
  })
  assertNotInherited(result)
  assert.equal(result.requests.length, 1)
})

test('a skipped-only draft success cannot hide an older dispatch that did heavy work', (t) => {
  const result = probe(t, {
    runs: [run(11, 'pull_request'), run(10)],
    jobs: { 11: [dedup, heavy('skipped')], 10: [heavy()] },
  })
  assert.equal(result.outputs.skip, 'true')
  assert.equal(result.outputs['run-url'], url(10))
  assert.equal(result.requests.length, 3)
})

test('an untrusted run URL cannot produce inherited success', (t) => {
  assertNotInherited(
    probe(t, {
      runs: [run(10, 'workflow_dispatch', { html_url: 'https://elsewhere.invalid/run' })],
    }),
  )
})
