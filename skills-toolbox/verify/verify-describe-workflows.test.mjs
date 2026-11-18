import assert from 'node:assert/strict'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { discoverExtensions } from '../skill-extensions.mjs'
import { describeWorkflows, workflowItems, workflowTriggers } from './verify-describe-workflows.mjs'
import { envelopeFor, gitFixture, runDescriberScript } from './verify-git-fixture.mjs'
import { MAX_TEXT_CHARS } from './verify-proof.mjs'

const PAYLOAD = new URL(
  '../../lib/bossalib/contract/plugin/skillpayload/skills/boss-verify/extensions/',
  import.meta.url,
)

function changed(t, before, after) {
  const repo = gitFixture(t)
  repo.write({ README: 'base', ...before })
  const base = repo.commit('base')
  repo.write(after)
  const head = repo.commit('head')
  const envelope = envelopeFor(repo.dir, base, head, 'infra', 'boss-verify-workflows')
  return { repo, envelope, result: describeWorkflows(envelope, { cwd: repo.dir }) }
}

test('reads inline, list, block and quoted on forms', () => {
  for (const source of [
    'on: [push, pull_request]\n',
    'on:\n  - push\n  - pull_request\n',
    'on:\n  push:\n  pull_request:\n',
    '"on":\n  push:\n  pull_request:\n',
  ])
    assert.deepEqual([...workflowTriggers(source).keys()], ['push', 'pull_request'])
  assert.deepEqual([...workflowTriggers('on: push\n').keys()], ['push'])
  assert.equal(workflowTriggers('name: ci\njobs:\n  build: {}\n').size, 0)
})

test('quoted block event keys retain triggers and filter changes', () => {
  const plain = 'on:\n  push:\n'
  for (const key of ['"push"', "'push'"]) {
    const quoted = `on:\n  ${key}:\n`
    assert.deepEqual([...workflowTriggers(quoted).keys()], ['push'])
    assert.deepEqual(workflowItems('ci.yml', plain, quoted, 'M'), [])
    const before = `on:\n  ${key}: {branches: [PRIVATE_BEFORE]}\n`
    const after = before.replace('PRIVATE_BEFORE', 'PRIVATE_AFTER')
    const facts = workflowItems('ci.yml', before, after, 'M')
    assert.deepEqual(
      facts.map((fact) => fact.text),
      ['Workflow ci.yml triggers: changed push filters'],
    )
    assert.doesNotMatch(JSON.stringify(facts), /PRIVATE/)
  }
})

test('trigger syntax conversions never invent additions or removals', () => {
  const block = 'on:\n  push:\n'
  const sequence = 'on:\n- push\njobs: {}\n'
  assert.deepEqual(workflowItems('ci.yml', block, sequence, 'M'), [])
  const flow = 'on: {push: {}}\n'
  for (const [before, after] of [
    [block, flow],
    [flow, block],
  ]) {
    const facts = workflowItems('ci.yml', before, after, 'M')
    assert.doesNotMatch(JSON.stringify(facts), /added push|removed push/)
    assert.match(facts[0].text, /could not be fully read/)
  }
})

test('reads per-trigger filters and list-valued schedules without inventing triggers from jobs', () => {
  const source =
    "on:\n  pull_request:\n    paths:\n      - 'src/**'\n    branches: [main]\n  schedule:\n    - cron: '0 1 * * *'\njobs:\n  build: {}\n"
  const triggers = workflowTriggers(source)
  assert.deepEqual([...triggers.keys()], ['pull_request', 'schedule'])
  assert.deepEqual([...triggers.get('pull_request').keys()], ['paths', 'branches'])
  assert.match(triggers.get('schedule').get(''), /cron:/)
  assert.deepEqual(
    workflowTriggers(source),
    workflowTriggers('# heading\n' + source.replace('paths:', 'paths: # comment')),
  )
})

test('inline event filter changes and removals produce facts without exposing values', (t) => {
  const before = 'on:\n  push: {branches: [PRIVATE_BEFORE]}\n'
  for (const after of ['on:\n  push: {branches: [PRIVATE_AFTER]}\n', 'on:\n  push:\n']) {
    const { result } = changed(
      t,
      { '.github/workflows/ci.yml': before },
      { '.github/workflows/ci.yml': after },
    )
    assert.deepEqual(result.items, ['Workflow ci.yml triggers: changed push filters'])
    assert.doesNotMatch(JSON.stringify(result), /PRIVATE/)
  }
  assert.deepEqual(workflowItems('ci.yml', before, before, 'M'), [])
})

test('new and removed workflows report names and trigger keys only', (t) => {
  const { result } = changed(
    t,
    { '.github/workflows/old.yml': 'on: push\n' },
    {
      '.github/workflows/old.yml': null,
      '.github/workflows/new.yaml':
        'on: [push, pull_request]\nenv:\n  TOKEN: DO_NOT_PRINT_SECRET\n',
      '.github/workflows/manual.yml': 'name: No triggers\n',
    },
  )
  assert.equal(result.breaking, false)
  assert.ok(result.items.includes('Workflow old.yml removed'))
  assert.ok(result.items.includes('New workflow new.yaml (triggers: push, pull_request)'))
  assert.ok(result.items.includes('New workflow manual.yml (triggers: none read)'))
  assert.doesNotMatch(JSON.stringify(result), /DO_NOT_PRINT_SECRET/)
  assert.equal(result.blocks, undefined)
})

test('changed paths and cron name changed filters while hiding their values', (t) => {
  const source =
    "on:\n  pull_request:\n    paths:\n      - 'old/**'\n  schedule:\n    - cron: '0 1 * * *'\n"
  const { result } = changed(
    t,
    { '.github/workflows/ci.yml': source },
    {
      '.github/workflows/ci.yml': source
        .replace('old/**', 'private/**')
        .replace('0 1 * * *', '0 2 * * *'),
    },
  )
  assert.deepEqual(result.items, [
    'Workflow ci.yml triggers: changed pull_request paths, schedule filters',
  ])
  assert.doesNotMatch(JSON.stringify(result), /private|0 2/)
  assert.deepEqual(
    workflowItems('ci.yml', 'on: push\n', 'on: pull_request\n', 'M').map((x) => x.text),
    ['Workflow ci.yml triggers: added pull_request; removed push'],
  )
})

test('renamed workflow reads its previous path at base', (t) => {
  const repo = gitFixture(t)
  const body = 'on: push\n' + Array.from({ length: 20 }, (_, n) => `# line ${n}\n`).join('')
  repo.write({ '.github/workflows/before.yml': body })
  const base = repo.commit('base')
  repo.move('.github/workflows/before.yml', '.github/workflows/after.yml')
  repo.write({ '.github/workflows/after.yml': body.replace('on: push', 'on: pull_request') })
  const head = repo.commit('head')
  const result = describeWorkflows(envelopeFor(repo.dir, base, head, 'infra'), { cwd: repo.dir })
  assert.deepEqual(result.items, ['Workflow after.yml triggers: added pull_request; removed push'])
})

test('moving YAML into or out of workflow directory reports its added or removed workflow', (t) => {
  const repo = gitFixture(t)
  repo.write({ 'ci.yml': 'on: push\n', '.github/workflows/gone.yml': 'on: pull_request\n' })
  const base = repo.commit('base')
  repo.move('ci.yml', '.github/workflows/ci.yml')
  repo.move('.github/workflows/gone.yml', 'gone.yml')
  const head = repo.commit('head')
  const result = describeWorkflows(envelopeFor(repo.dir, base, head, 'infra'), { cwd: repo.dir })
  assert.ok(result.items.includes('New workflow ci.yml (triggers: push)'))
  assert.ok(result.items.includes('Workflow gone.yml removed'))
})

test('ignores comments, nested workflows, non-YAML files and absent workflow changes', (t) => {
  const comments = changed(
    t,
    { '.github/workflows/ci.yml': 'on: push\n' },
    { '.github/workflows/ci.yml': '# note\non: push # note\n' },
  )
  assert.deepEqual(comments.result, { items: [], breaking: false })
  const ignored = changed(
    t,
    {},
    {
      '.github/workflows/sub/ci.yml': 'on: push\n',
      '.github/workflows/ci.txt': 'on: push\n',
      README: 'changed',
    },
  )
  assert.deepEqual(ignored.result, { items: [], breaking: false })
  const empty = changed(t, {}, { README: 'changed' })
  assert.deepEqual(empty.result, { items: [], breaking: false })
})

test('long workflow names and trigger lists fit text bounds', () => {
  const facts = workflowItems(
    'a'.repeat(240) + '.yml',
    '',
    'on: [' + Array.from({ length: 12 }, (_, n) => `event_${n}`).join(', ') + ']\n',
    'A',
  )
  assert.ok(facts[0].text.length <= MAX_TEXT_CHARS)
  assert.ok(facts[0].text.endsWith('…'))
})

test('payload discovery and vendored entry point match canonical workflow facts', (t) => {
  const { repo, envelope, result } = changed(t, {}, { '.github/workflows/ci.yml': 'on: push\n' })
  const { extensions } = discoverExtensions({
    core: 'boss-verify',
    role: 'proof-describe',
    builtinDir: fileURLToPath(PAYLOAD),
    root: repo.dir,
  })
  const extension = extensions.find((x) => x.name === 'boss-verify-workflows')
  assert.equal(extension.builtin, true)
  assert.deepEqual(extension.describeAreas, ['infra', 'no-sign-off'])
  const actual = runDescriberScript(
    new URL('boss-verify-workflows/describe.mjs', PAYLOAD),
    envelope,
    repo.dir,
  )
  assert.deepEqual(actual.items, result.items)
  assert.equal(actual.breaking, false)
})

test('unreadable flow mappings do not print their filter values as trigger names', () => {
  const result = workflowItems(
    '.github/workflows/test.yml',
    '',
    'on: {push: {branches: [DO_NOT_PRINT_SECRET]}}\n',
    'A',
  )
  assert.match(result[0].text, /none read/)
  assert.doesNotMatch(JSON.stringify(result), /DO_NOT_PRINT_SECRET/)
})

test('nested flow mapping lists never become trigger names or trigger changes', () => {
  const before = 'on: {push: {branches: [main, PRIVATE_BEFORE, dev]}}\n'
  const after = 'on: {push: {branches: [main, PRIVATE_AFTER, dev]}}\n'
  assert.equal(workflowTriggers(after).size, 0)
  const added = workflowItems('.github/workflows/test.yml', '', after, 'A')
  assert.match(added[0].text, /none read/)
  assert.doesNotMatch(JSON.stringify(added), /PRIVATE_AFTER|main|dev/)
  const modified = workflowItems('.github/workflows/test.yml', before, after, 'M')
  assert.match(modified[0].text, /trigger configuration changed/)
  assert.doesNotMatch(JSON.stringify(modified), /PRIVATE|main|dev/)
  assert.deepEqual(workflowItems('test.yml', before, before + '# comment\n', 'M'), [])
})

test('quoted hash filters keep their values while real comments are ignored', (t) => {
  for (const value of [
    "'docs #old/**'",
    '"docs #old/**"',
    "'docs'' #old/**'",
    '"docs\\" #old/**"',
  ]) {
    const before = `on:\n  push:\n    paths:\n      - ${value} # trailing comment\n`
    const { result } = changed(
      t,
      { '.github/workflows/ci.yml': before },
      { '.github/workflows/ci.yml': before.replace('#old', '#new') },
    )
    assert.deepEqual(result.items, ['Workflow ci.yml triggers: changed push paths filters'])
    assert.doesNotMatch(JSON.stringify(result), /#old|#new/)
    assert.deepEqual(
      workflowItems('ci.yml', before, before.replace('trailing comment', 'new comment'), 'M'),
      [],
    )
  }
})

test('filling an existing empty workflow describes added triggers', () => {
  const facts = workflowItems('ci.yml', '', 'on: push\n', 'M')
  assert.deepEqual(
    facts.map((fact) => fact.text),
    ['Workflow ci.yml triggers: added push'],
  )
})

test('flow trigger changes are reported without exposing values or unrelated job edits', () => {
  const before = 'on: {push: {}}\njobs: {}\n'
  const after = 'on: {pull_request_target: {}}\njobs: {}\n'
  assert.match(workflowItems('ci.yml', before, after, 'M')[0].text, /configuration changed/)
  assert.deepEqual(
    workflowItems('ci.yml', before, before.replace('jobs: {}', 'jobs: {build: {}}'), 'M'),
    [],
  )
})

test('multiline flow sequences report unknown trigger changes without partial names', () => {
  const before = 'on: [push,\n  pull_request]\njobs: {}\n'
  const after = before.replace('pull_request', 'workflow_dispatch')
  const parsed = workflowTriggers(before)
  assert.equal(parsed.unreadable, true)
  assert.equal(parsed.size, 0)
  const facts = workflowItems('ci.yml', before, after, 'M')
  assert.match(facts[0].text, /trigger configuration changed/)
  assert.doesNotMatch(JSON.stringify(facts), /added push|removed push|workflow_dispatch/)
  assert.deepEqual(
    workflowItems('ci.yml', before, before.replace('jobs: {}', 'jobs: {build: {}}'), 'M'),
    [],
  )
  assert.deepEqual(
    workflowItems(
      'ci.yml',
      before,
      before.replace('pull_request]', 'pull_request] # comment'),
      'M',
    ),
    [],
  )
  const unindented = 'on: [push,\n  pull_request\n]\njobs: {}\n'
  assert.deepEqual(workflowItems('ci.yml', unindented, unindented + '# comment\n', 'M'), [])
})
