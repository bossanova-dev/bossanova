import test from 'node:test'
import { region } from '../../scripts/test-infra/gate-region-lib.mjs'
import { DEFAULT_CONFIG } from '../skill-config.mjs'
import assert from 'node:assert/strict'
import { annotatePrediction, keyChangePaths, predictVerifyPark } from './plan-park-prediction.mjs'

const config = {
  ...DEFAULT_CONFIG,
  verifyDefaults: {
    alwaysHumanPaths: [
      { glob: 'toolbox/verify/**', label: 'the verify judge (cannot grade itself)' },
    ],
  },
}
const trackedFiles = ['toolbox/verify/SKILL.md', 'toolbox/verify/gate.mjs', 'plain/file.txt']
const deps = { trackedFiles }
const body = (path) =>
  `## Key changes\n\n- \`${path}\`\n\n## Planning\n\n- Agent-friendly: yes\n\n## Original notes\n\nImages ![](https://example.com/image.png)\n- Predicted verify park: original text\n`
const RETIRED_WHY =
  '## Why this needs a human\n\nAn agent can build this change, but verify will hold the merge for a person.\nPredicted reasons: always-human-path · the verify judge (cannot grade itself) (from toolbox/verify/gate.mjs).\n\n'
const RETIRED_AGENT =
  '- Agent-friendly: needs-human (see "Why this needs a human")\n<!-- predicted-verify-park-agent-friendly -->'
// What the retired needs-human mode wrote into an annotated plan.
const retiredAnnotation = (annotated, agentLine = RETIRED_AGENT) =>
  annotated
    .replace('## Planning', `${RETIRED_WHY}## Planning`)
    .replace('- Agent-friendly: yes', agentLine)

test('file, collapsed glob and tracked case resolve to the same protected judge', () => {
  for (const path of ['toolbox/verify/gate.mjs', 'toolbox/verify/**', 'toolbox/verify/skill.md']) {
    const prediction = predictVerifyPark(config, body(path), deps)
    assert.equal(prediction.predicted, true)
    assert.deepEqual(prediction.codes, ['always-human-path'])
    assert.deepEqual(prediction.labels, ['the verify judge (cannot grade itself)'])
    assert.ok(
      annotatePrediction(body(path), prediction).includes(
        '- Predicted verify park: always-human-path',
      ),
    )
  }
  assert.deepEqual(keyChangePaths(config, body('toolbox/verify/skill.md'), deps).paths, [
    'toolbox/verify/SKILL.md',
  ])
  assert.deepEqual(keyChangePaths(config, body('toolbox/verify/NEW.mjs'), deps).paths, [
    'toolbox/verify/NEW.mjs',
  ])
})
test('unrelated paths remain byte-identical', () => {
  const source = body('plain/file.txt')
  const prediction = predictVerifyPark(config, source, deps)
  assert.equal(prediction.predicted, false)
  assert.equal(annotatePrediction(source, prediction), source)
})
test('a predicted park keeps the plan agent-friendly and preserves Original notes verbatim', () => {
  const source = body('toolbox/verify/gate.mjs')
  const tail = region(source, '## Original notes')
  const prediction = predictVerifyPark(config, source, deps)
  const annotated = annotatePrediction(source, prediction)
  assert.ok(!annotated.includes('## Why this needs a human'))
  assert.ok(!annotated.includes('Agent-friendly: needs-human'))
  assert.ok(annotated.includes('- Agent-friendly: yes'))
  assert.equal(region(annotated, '## Original notes'), tail)
  assert.equal(annotatePrediction(annotated, prediction), annotated)
  assert.ok(
    !annotatePrediction(annotated.replace('always-human-path ·', 'stale ·'), prediction).includes(
      'stale ·',
    ),
  )
  assert.equal(annotatePrediction(annotated, { ...prediction, predicted: false }), source)
})

test('re-annotating a plan from the retired needs-human mode restores it to agent-friendly', () => {
  const source = body('toolbox/verify/gate.mjs')
  const prediction = predictVerifyPark(config, source, deps)
  const annotated = annotatePrediction(source, prediction)
  const retired = retiredAnnotation(annotated)
  assert.ok(retired.includes('Agent-friendly: needs-human'))
  assert.equal(annotatePrediction(retired, prediction), annotated)
  assert.equal(annotatePrediction(retired, { ...prediction, predicted: false }), source)
})
test('missing Planning is created before original notes', () => {
  const source = body('toolbox/verify/gate.mjs').replace(
    '## Planning\n\n- Agent-friendly: yes\n\n',
    '',
  )
  const prediction = predictVerifyPark(config, source, deps)
  const annotated = annotatePrediction(source, prediction)
  assert.ok(annotated.indexOf('## Planning') < annotated.indexOf('## Original notes'))
  assert.equal(annotatePrediction(annotated, prediction), annotated)
})

test('annotate CLI updates both artifacts and preserves unrelated artifacts', async (t) => {
  const { mkdtempSync, writeFileSync, readFileSync, rmSync } = await import('node:fs')
  const { tmpdir } = await import('node:os')
  const { join } = await import('node:path')
  const { execFileSync } = await import('node:child_process')
  const { fileURLToPath } = await import('node:url')
  const dir = mkdtempSync(join(tmpdir(), 'plan-park-cli-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  execFileSync('git', ['init', '--quiet', dir])
  writeFileSync(
    join(dir, '.boss-skills.json'),
    JSON.stringify({
      verifyDefaults: config.verifyDefaults,
    }),
  )
  const cli = fileURLToPath(new URL('./plan-park-prediction.mjs', import.meta.url))
  const description = join(dir, 'description.md')
  const plan = join(dir, 'plan.md')
  for (const [path, predicted] of [
    ['toolbox/verify/gate.mjs', true],
    ['plain/file.txt', false],
  ]) {
    const source = body(path)
    writeFileSync(description, source)
    writeFileSync(plan, source)
    const run = () =>
      JSON.parse(
        execFileSync(
          process.execPath,
          [cli, 'annotate', '--description', description, '--plan', plan, '--json'],
          { cwd: dir, encoding: 'utf8' },
        ),
      )
    assert.deepEqual(Object.keys(run()), ['predicted', 'codes', 'labels'])
    const bytes = readFileSync(description, 'utf8')
    assert.equal(bytes.includes('Agent-friendly: needs-human'), false)
    assert.equal(readFileSync(plan, 'utf8'), bytes)
    if (predicted) assert.ok(bytes.includes('the verify judge (cannot grade itself)'))
    else assert.equal(bytes, source)
    run()
    assert.equal(readFileSync(description, 'utf8'), bytes)
    assert.equal(readFileSync(plan, 'utf8'), bytes)
  }
})

test('plan predictions match judge path reasons across policy categories and mixed paths', async () => {
  const { decideVerdict } = await import('../verify/verify-gate.mjs')
  const { pathParkFacts } = await import('../verify/verify-path-parks.mjs')
  const { reviewPolicyFromBlock } = await import('../verify/verify-review-policy.mjs')
  const policyConfig = {
    ...DEFAULT_CONFIG,
    verifyDefaults: {
      ...config.verifyDefaults,
      humanReview: { security: ['wire/**'], ui: ['screens/**'] },
    },
  }
  const policy = { review: reviewPolicyFromBlock(policyConfig.verifyDefaults) }
  for (const paths of [
    ['toolbox/verify/gate.mjs'],
    ['wire/types.proto'],
    ['screens/view.tsx'],
    ['.boss-skills.json'],
    ['plain/file.txt'],
    ['toolbox/verify/gate.mjs', 'wire/types.proto', '.boss-skills.json'],
  ]) {
    const description = `## Key changes\n\n${paths.map((path) => `- \`${path}\``).join('\n')}\n`
    const prediction = predictVerifyPark(policyConfig, description, {
      trackedFiles: paths,
      rootFiles: ['.boss-skills.json'],
    })
    const verdict = decideVerdict({
      ...pathParkFacts(paths, policy),
      review: policy.review,
      changedPaths: paths,
      judgeAreas: [],
      changedLines: 0,
      sizeBaseline: { ok: true, thresholds: null },
      prState: 'OPEN',
      isDraft: false,
      ci: { settled: true, green: true },
      receipt: 'success',
      extensions: [],
      // A prediction names path parks only; a high head merges under a rule this repo has.
      mergePolicy: { auto: [{ level: 'high' }] },
    })
    assert.deepEqual(prediction.codes, verdict.reasons ?? [])
    assert.deepEqual(prediction.labels, verdict.labels ?? [])
  }
})

test('predicted park markers retain legal labels containing replacement tokens', () => {
  const prediction = {
    predicted: true,
    codes: ['always-human-path'],
    labels: ['$& policy'],
    areas: ['judge/main.mjs'],
  }
  for (const prefix of ['', '## Why this needs a human\n\nIndependent reason.\n\n']) {
    const annotated = annotatePrediction(prefix + body('judge/main.mjs'), prediction)
    assert.ok(
      annotated.includes(
        '- Predicted verify park: always-human-path · $& policy (from judge/main.mjs)',
      ),
    )
    assert.equal(annotatePrediction(annotated, prediction), annotated)
  }
})

test('new file spelling comes from Key changes rather than an earlier summary mention', () => {
  const source =
    '## Summary\n\nReplace `toolbox/verify/new.mjs`.\n\n' + body('toolbox/verify/NEW.mjs')
  assert.deepEqual(keyChangePaths(config, source, deps).paths, ['toolbox/verify/NEW.mjs'])
})

test('restricted globs include only matching files, including empty matches and mixed entries', () => {
  const scoped = {
    ...DEFAULT_CONFIG,
    verifyDefaults: {
      humanReview: { ui: ['services/web/src/**/*.tsx', 'services/web/src/**/*.css'] },
    },
  }
  const files = [
    'services/web/src/example.test.ts',
    'services/web/src/View.tsx',
    'services/web/src/style.css',
    'services/web/src/nested/Other.test.ts',
  ]
  for (const spec of [
    'services/web/src/*.test.ts',
    'services/web/src/*.missing.ts',
    'services/web/src/**/*.test.ts',
  ]) {
    const prediction = predictVerifyPark(scoped, body(spec), { trackedFiles: files })
    assert.equal(prediction.predicted, false)
  }
  assert.deepEqual(
    keyChangePaths(scoped, body('services/web/src/*.test.ts'), { trackedFiles: files }).paths,
    [files[0]],
  )
  const mixed = body('services/web/src/*.test.ts').replace(
    '## Planning',
    '- `services/web/src/View.tsx`\n\n## Planning',
  )
  assert.equal(predictVerifyPark(scoped, mixed, { trackedFiles: files }).predicted, true)
  assert.equal(
    predictVerifyPark(scoped, body('services/web/src/**'), { trackedFiles: files }).predicted,
    true,
  )
})

test('annotation preserves independently authored human decisions across prediction changes', () => {
  const source = body('toolbox/verify/gate.mjs')
    .replace(
      '- Agent-friendly: yes',
      '- Agent-friendly: needs-human (see "Why this needs a human")',
    )
    .replace(
      '## Planning',
      '## Why this needs a human\n\nA person must supply credentials.\n\n## Planning',
    )
  const prediction = predictVerifyPark(config, source, deps)
  assert.equal(annotatePrediction(source, { ...prediction, predicted: false }), source)
  const annotated = annotatePrediction(source, prediction)
  assert.ok(annotated.includes('A person must supply credentials.'))
  assert.ok(annotated.includes('- Agent-friendly: needs-human'))
  assert.equal(annotatePrediction(annotated, prediction), annotated)
  assert.equal(annotatePrediction(annotated, { ...prediction, predicted: false }), source)
})

test('unmatched globs retain broad path policy predictions for planned new files', () => {
  const scoped = {
    ...DEFAULT_CONFIG,
    verifyDefaults: { alwaysHumanPaths: [{ glob: 'infra/**', label: 'infrastructure' }] },
  }
  const source = body('infra/new-stack/*.tf')
  assert.deepEqual(keyChangePaths(scoped, source, { trackedFiles: [] }).paths, [
    'infra/new-stack/*.tf',
  ])
  assert.equal(predictVerifyPark(scoped, source, { trackedFiles: [] }).predicted, true)
})

test('legacy generated human decisions reconcile without overriding independent reasons', () => {
  const source = body('toolbox/verify/gate.mjs')
  const prediction = predictVerifyPark(config, source, deps)
  const legacy = retiredAnnotation(
    annotatePrediction(source, prediction),
    '- Agent-friendly: needs-human (see "Why this needs a human")',
  )
  assert.equal(annotatePrediction(legacy, { ...prediction, predicted: false }), source)
  const authored = legacy.replace(
    '## Why this needs a human\n',
    '## Why this needs a human\nA human must provide credentials.\n',
  )
  const cleared = annotatePrediction(authored, { ...prediction, predicted: false })
  assert.ok(cleared.includes('A human must provide credentials.'))
  assert.ok(cleared.includes('- Agent-friendly: needs-human'))
})
