import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { DEFAULT_CONFIG, loadSkillConfig, withTrackerDefaults } from '../skill-config.mjs'
import { mergePlannedLabels } from './plan-label-merge.mjs'

const CLI = fileURLToPath(new URL('./plan-label-merge.mjs', import.meta.url))

function configWithLabels(labels = {}) {
  return withTrackerDefaults({
    ...DEFAULT_CONFIG,
    trackerConfig: {
      linear: {
        labels,
      },
    },
  })
}

test('mergePlannedLabels preserves existing tracker spelling while stripping agent-plan and adding agent-build', () => {
  const config = configWithLabels({ feature: 'Feature' })
  const result = mergePlannedLabels({
    config,
    current: ['feature', 'agent-plan'],
    metadata: { labels: ['feature'], agentBuild: true, openQuestions: [] },
  })
  assert.deepEqual(result, {
    labels: ['feature', 'agent-build'],
    added: ['agent-build'],
    removed: ['agent-plan'],
  })
})

test('mergePlannedLabels applies configured spelling when the issue does not carry the label', () => {
  const config = configWithLabels({ improvement: 'Improvement' })
  const result = mergePlannedLabels({
    config,
    current: [],
    metadata: { labels: ['improvement'], agentBuild: true, openQuestions: [] },
  })
  assert.deepEqual(result, {
    labels: ['Improvement', 'agent-build'],
    added: ['Improvement', 'agent-build'],
    removed: [],
  })
})

test('mergePlannedLabels handles agentBuild false, true, and omitted with existing Needs-Human', () => {
  const config = withTrackerDefaults(DEFAULT_CONFIG)

  const needsHumanKept = mergePlannedLabels({
    config,
    current: ['Needs-Human', 'agent-plan'],
    metadata: { labels: [], agentBuild: false, openQuestions: [] },
  })
  assert.deepEqual(needsHumanKept, {
    labels: ['Needs-Human'],
    added: [],
    removed: ['agent-plan'],
  })

  const flippedToBuild = mergePlannedLabels({
    config,
    current: ['Needs-Human'],
    metadata: { labels: [], agentBuild: true, openQuestions: [] },
  })
  assert.deepEqual(flippedToBuild, {
    labels: ['agent-build'],
    added: ['agent-build'],
    removed: ['Needs-Human'],
  })

  const omittedDefaultsToBuild = mergePlannedLabels({
    config,
    current: ['agent-plan'],
    metadata: { labels: [], openQuestions: [] },
  })
  assert.deepEqual(omittedDefaultsToBuild, {
    labels: ['agent-build'],
    added: ['agent-build'],
    removed: ['agent-plan'],
  })
})

test('mergePlannedLabels reuses existing Agent-Question verbatim when openQuestions are present', () => {
  const config = withTrackerDefaults(DEFAULT_CONFIG)
  const reused = mergePlannedLabels({
    config,
    current: ['Agent-Question', 'agent-plan'],
    metadata: { labels: [], agentBuild: true, openQuestions: ['Should this retry?'] },
  })
  assert.deepEqual(reused, {
    labels: ['Agent-Question', 'agent-build'],
    added: ['agent-build'],
    removed: ['agent-plan'],
  })

  const added = mergePlannedLabels({
    config,
    current: [],
    metadata: { labels: [], agentBuild: true, openQuestions: ['Should this retry?'] },
  })
  assert.deepEqual(added, {
    labels: ['agent-build', 'agent-question'],
    added: ['agent-build', 'agent-question'],
    removed: [],
  })
})

test('mergePlannedLabels interactive mode strips agent-question regardless of casing and never adds it', () => {
  const config = withTrackerDefaults(DEFAULT_CONFIG)
  for (const existingQuestion of ['agent-question', 'Agent-Question']) {
    const result = mergePlannedLabels({
      config,
      current: ['bug', existingQuestion, 'agent-plan'],
      metadata: { labels: ['bug'], agentBuild: true, openQuestions: ['Leftover question'] },
      interactive: true,
      addAgentQuestion: true,
    })
    assert.deepEqual(result, {
      labels: ['bug', 'agent-build'],
      added: ['agent-build'],
      removed: [existingQuestion, 'agent-plan'],
    })
  }
})

test('mergePlannedLabels headless mode preserves leftover agent-question and honors addAgentQuestion', () => {
  const config = withTrackerDefaults(DEFAULT_CONFIG)

  const leftoverKept = mergePlannedLabels({
    config,
    current: ['agent-question'],
    metadata: { labels: [], agentBuild: true, openQuestions: [] },
  })
  assert.deepEqual(leftoverKept, {
    labels: ['agent-question', 'agent-build'],
    added: ['agent-build'],
    removed: [],
  })

  const addedByFlag = mergePlannedLabels({
    config,
    current: ['agent-build'],
    metadata: { labels: [], agentBuild: true, openQuestions: [] },
    addAgentQuestion: true,
  })
  assert.deepEqual(addedByFlag, {
    labels: ['agent-build', 'agent-question'],
    added: ['agent-question'],
    removed: [],
  })

  const reusedByFlag = mergePlannedLabels({
    config,
    current: ['Agent-Question', 'agent-build'],
    metadata: { labels: [], agentBuild: true, openQuestions: [] },
    addAgentQuestion: true,
  })
  assert.deepEqual(reusedByFlag, {
    labels: ['Agent-Question', 'agent-build'],
    added: [],
    removed: [],
  })
})

test('mergePlannedLabels holds needs-human for non-empty siblings until siblingsVerified flips to agent-build', () => {
  const config = withTrackerDefaults(DEFAULT_CONFIG)
  const siblings = [{ key: 'part-b', title: 'Second half' }]

  const held = mergePlannedLabels({
    config,
    current: ['agent-build', 'agent-plan'],
    metadata: { labels: [], agentBuild: true, siblings, openQuestions: [] },
  })
  assert.deepEqual(held, {
    labels: ['needs-human'],
    added: ['needs-human'],
    removed: ['agent-build', 'agent-plan'],
  })

  const verified = mergePlannedLabels({
    config,
    current: ['Needs-Human', 'feature'],
    metadata: { labels: ['feature'], agentBuild: true, siblings, openQuestions: [] },
    siblingsVerified: true,
  })
  assert.deepEqual(verified, {
    labels: ['feature', 'agent-build'],
    added: ['agent-build'],
    removed: ['Needs-Human'],
  })

  const verifiedButNeedsHuman = mergePlannedLabels({
    config,
    current: ['needs-human'],
    metadata: { labels: [], agentBuild: false, siblings, openQuestions: [] },
    siblingsVerified: true,
  })
  assert.deepEqual(verifiedButNeedsHuman, {
    labels: ['needs-human'],
    added: [],
    removed: [],
  })
})

test('mergePlannedLabels keeps unrelated labels in order, drops non-taxonomy labels, and dedupes additions', () => {
  const config = configWithLabels({ feature: 'Feature', bug: 'Bug' })
  const result = mergePlannedLabels({
    config,
    current: ['team-alpha', 'urgent', 'agent-plan', 'customer-reported'],
    metadata: {
      labels: ['feature', 'FEATURE', 'not-in-taxonomy', 'agent-build', 'bug', 'Bug'],
      agentBuild: true,
      openQuestions: [],
    },
  })
  assert.deepEqual(result, {
    labels: ['team-alpha', 'urgent', 'customer-reported', 'Feature', 'Bug', 'agent-build'],
    added: ['Feature', 'Bug', 'agent-build'],
    removed: ['agent-plan'],
  })
})

test('mergePlannedLabels accepts all four current label shapes with identical output', () => {
  const config = configWithLabels({ feature: 'Feature' })
  const metadata = { labels: ['feature'], agentBuild: true, openQuestions: [] }
  const expected = {
    labels: ['feature', 'agent-build'],
    added: ['agent-build'],
    removed: ['agent-plan'],
  }

  const shapes = [
    ['feature', 'agent-plan'],
    [{ name: 'feature' }, { name: 'agent-plan' }],
    { id: 'ABC-1', labels: { nodes: [{ name: 'feature' }, { name: 'agent-plan' }] } },
    { id: 'ABC-1', labels: ['feature', 'agent-plan'] },
    { id: 'ABC-1', labels: [{ name: 'feature' }, { name: 'agent-plan' }] },
  ]
  for (const current of shapes) {
    assert.deepEqual(mergePlannedLabels({ config, current, metadata }), expected)
  }
})

test('mergePlannedLabels leaves a pre-existing duplicate spelling pair untouched and adds no third', () => {
  const config = configWithLabels({ feature: 'Feature' })
  const result = mergePlannedLabels({
    config,
    current: ['feature', 'Feature'],
    metadata: { labels: ['feature'], agentBuild: true, openQuestions: [] },
  })
  assert.deepEqual(result, {
    labels: ['feature', 'Feature', 'agent-build'],
    added: ['agent-build'],
    removed: [],
  })
})

test('mergePlannedLabels works with default config from withTrackerDefaults and loadSkillConfig', (t) => {
  const emptyDir = tempDir('plan-label-merge-default-', t)
  const metadata = { labels: ['feature', 'docs'], agentBuild: true, openQuestions: [] }
  for (const config of [withTrackerDefaults(DEFAULT_CONFIG), loadSkillConfig({ cwd: emptyDir })]) {
    const result = mergePlannedLabels({
      config,
      current: ['Feature', 'agent-plan'],
      metadata,
    })
    assert.deepEqual(result, {
      labels: ['Feature', 'docs', 'agent-build'],
      added: ['docs', 'agent-build'],
      removed: ['agent-plan'],
    })
  }
})

test('CLI preserves existing spelling from repo .boss-skills.json and supports optional flags', (t) => {
  const dir = tempDir('plan-label-merge-cli-', t)
  writeFileSync(
    path.join(dir, '.boss-skills.json'),
    JSON.stringify({
      trackerConfig: {
        linear: {
          labels: {
            feature: 'Feature',
            improvement: 'Improvement',
          },
        },
      },
    }),
  )
  const metadataPath = path.join(dir, 'ABC-1.draft-metadata.json')
  const currentPath = path.join(dir, 'ABC-1.current-labels.json')
  writeFileSync(
    metadataPath,
    JSON.stringify({
      labels: ['feature'],
      agentBuild: true,
      openQuestions: [],
    }),
  )
  writeFileSync(
    currentPath,
    JSON.stringify({
      id: 'ABC-1',
      labels: { nodes: [{ name: 'feature' }, { name: 'agent-plan' }] },
    }),
  )

  const run = spawnSync(
    process.execPath,
    [CLI, '--metadata', metadataPath, '--current', currentPath],
    { cwd: dir, encoding: 'utf8' },
  )
  assert.equal(run.status, 0, run.stderr)
  assert.equal(run.stderr, '')
  const lines = run.stdout.trimEnd().split('\n')
  assert.equal(lines.length, 1)
  assert.deepEqual(JSON.parse(lines[0]), {
    labels: ['feature', 'agent-build'],
    added: ['agent-build'],
    removed: ['agent-plan'],
  })

  const withQuestion = spawnSync(
    process.execPath,
    [CLI, '--metadata', metadataPath, '--current', currentPath, '--add-agent-question'],
    { cwd: dir, encoding: 'utf8' },
  )
  assert.equal(withQuestion.status, 0, withQuestion.stderr)
  assert.deepEqual(JSON.parse(withQuestion.stdout), {
    labels: ['feature', 'agent-build', 'agent-question'],
    added: ['agent-build', 'agent-question'],
    removed: ['agent-plan'],
  })

  writeFileSync(
    metadataPath,
    JSON.stringify({
      labels: ['feature'],
      agentBuild: true,
      siblings: [{ key: 'b', title: 'Part B' }],
      openQuestions: ['Pending?'],
    }),
  )
  writeFileSync(currentPath, JSON.stringify(['feature', 'Needs-Human', 'Agent-Question']))
  const interactiveVerified = spawnSync(
    process.execPath,
    [
      CLI,
      '--metadata',
      metadataPath,
      '--current',
      currentPath,
      '--interactive',
      '--siblings-verified',
    ],
    { cwd: dir, encoding: 'utf8' },
  )
  assert.equal(interactiveVerified.status, 0, interactiveVerified.stderr)
  assert.deepEqual(JSON.parse(interactiveVerified.stdout), {
    labels: ['feature', 'agent-build'],
    added: ['agent-build'],
    removed: ['Needs-Human', 'Agent-Question'],
  })
})

test('CLI exits 1 with input-unreadable on stderr and empty stdout for missing, malformed, or bad-shape inputs', (t) => {
  const dir = tempDir('plan-label-merge-errors-', t)
  const validMeta = path.join(dir, 'meta.json')
  const validCurrent = path.join(dir, 'current.json')
  writeFileSync(validMeta, JSON.stringify({ labels: [], agentBuild: true, openQuestions: [] }))
  writeFileSync(validCurrent, JSON.stringify(['feature']))

  const missingCurrent = spawnSync(
    process.execPath,
    [CLI, '--metadata', validMeta, '--current', path.join(dir, 'nope.json')],
    { cwd: dir, encoding: 'utf8' },
  )
  assert.equal(missingCurrent.status, 1)
  assert.equal(missingCurrent.stdout, '')
  assert.match(missingCurrent.stderr, /^plan-label-merge: input-unreadable: .+\n$/)

  const missingMeta = spawnSync(
    process.execPath,
    [CLI, '--metadata', path.join(dir, 'nope-meta.json'), '--current', validCurrent],
    { cwd: dir, encoding: 'utf8' },
  )
  assert.equal(missingMeta.status, 1)
  assert.equal(missingMeta.stdout, '')
  assert.match(missingMeta.stderr, /^plan-label-merge: input-unreadable: .+\n$/)

  const malformedCurrent = path.join(dir, 'malformed.json')
  writeFileSync(malformedCurrent, '{not valid json')
  const badJson = spawnSync(
    process.execPath,
    [CLI, '--metadata', validMeta, '--current', malformedCurrent],
    { cwd: dir, encoding: 'utf8' },
  )
  assert.equal(badJson.status, 1)
  assert.equal(badJson.stdout, '')
  assert.match(badJson.stderr, /^plan-label-merge: input-unreadable: .+\n$/)

  const badShapeCurrent = path.join(dir, 'bad-shape.json')
  for (const payload of [
    'null',
    '"feature"',
    '42',
    '{}',
    '{"id":"ABC-1"}',
    '{"labels":null}',
    '{"labels":"nope"}',
    '{"transport":"command","result":["feature","agent-plan"],"outcome":"operation-executed"}',
  ]) {
    writeFileSync(badShapeCurrent, payload)
    const run = spawnSync(
      process.execPath,
      [CLI, '--metadata', validMeta, '--current', badShapeCurrent],
      { cwd: dir, encoding: 'utf8' },
    )
    assert.equal(run.status, 1, payload)
    assert.equal(run.stdout, '', payload)
    assert.match(run.stderr, /^plan-label-merge: input-unreadable: .+\n$/, payload)
  }

  const emptyLabelsCurrent = path.join(dir, 'empty-labels.json')
  for (const payload of ['[]', '{"labels":[]}', '{"labels":{"nodes":[]}}']) {
    writeFileSync(emptyLabelsCurrent, payload)
    const run = spawnSync(
      process.execPath,
      [CLI, '--metadata', validMeta, '--current', emptyLabelsCurrent],
      { cwd: dir, encoding: 'utf8' },
    )
    assert.equal(run.status, 0, `${payload}: ${run.stderr}`)
    assert.deepEqual(JSON.parse(run.stdout), {
      labels: ['agent-build'],
      added: ['agent-build'],
      removed: [],
    })
  }

  const badShapeMeta = path.join(dir, 'bad-meta.json')
  writeFileSync(badShapeMeta, '[]')
  const metaShapeRun = spawnSync(
    process.execPath,
    [CLI, '--metadata', badShapeMeta, '--current', validCurrent],
    { cwd: dir, encoding: 'utf8' },
  )
  assert.equal(metaShapeRun.status, 1)
  assert.equal(metaShapeRun.stdout, '')
  assert.match(metaShapeRun.stderr, /^plan-label-merge: input-unreadable: .+\n$/)

  const badConfigDir = tempDir('plan-label-merge-bad-cfg-', t)
  writeFileSync(path.join(badConfigDir, '.boss-skills.json'), '{broken')
  const cfgRun = spawnSync(
    process.execPath,
    [CLI, '--metadata', validMeta, '--current', validCurrent],
    { cwd: badConfigDir, encoding: 'utf8' },
  )
  assert.equal(cfgRun.status, 1)
  assert.equal(cfgRun.stdout, '')
  assert.match(cfgRun.stderr, /^plan-label-merge: input-unreadable: .+\n$/)
})

test('CLI exits 2 with usage on stderr and empty stdout for bad argv', (t) => {
  const dir = tempDir('plan-label-merge-argv-', t)
  for (const args of [
    [],
    ['--metadata', 'a.json'],
    ['--current', 'b.json'],
    ['--metadata', 'a.json', '--current', 'b.json', '--unknown-flag'],
    ['--metadata', 'a.json', '--current', 'b.json', 'unexpected-positional'],
  ]) {
    const run = spawnSync(process.execPath, [CLI, ...args], { cwd: dir, encoding: 'utf8' })
    assert.equal(run.status, 2, JSON.stringify(args))
    assert.equal(run.stdout, '', JSON.stringify(args))
    assert.match(run.stderr, /usage:/i, JSON.stringify(args))
  }
})
