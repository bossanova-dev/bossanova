import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { requiredPlanSections, DEFAULT_CONFIG } from '../skill-config.mjs'
import { adoptReturnedMetadata } from './plan-run-guards.mjs'

process.env.BOSS_GATE_OUTCOME_FILE = path.join(tempDir('gate-outcome-suite-'), 'outcomes.tsv')

const guard = fileURLToPath(new URL('./plan-run-guards.mjs', import.meta.url))
const siblingCli = fileURLToPath(new URL('./plan-sibling-split.mjs', import.meta.url))

test('returned metadata decodes question text before comparing with the on-disk object', () => {
  const returned = { openQuestions: ['Use &lt;id&gt; or &lt;key&gt;?'] }
  const onDisk = { openQuestions: ['Use <id> or <key>?'] }
  const result = adoptReturnedMetadata({ returned, onDisk })
  assert.deepEqual(result.adopted, onDisk)
  assert.equal(result.state, 'identical')
  assert.equal(result.warnings.length, 1)
  assert.equal(result.warnings[0].code, 'returned-entities-decoded')
  assert.match(result.warnings[0].message, /openQuestions\[0\]/)
  assert.deepEqual(returned.openQuestions, ['Use &lt;id&gt; or &lt;key&gt;?'])
})

test('plain returned metadata needs no decode warning', () => {
  const returned = { openQuestions: ['Use <id>?'] }
  const result = adoptReturnedMetadata({ returned })
  assert.deepEqual(result.adopted, returned)
  assert.deepEqual(result.warnings ?? [], [])
})

test('transport decoding handles the five escaped characters in each encoding', async () => {
  const { decodeReturnTransport } = await import('./plan-adopt-metadata.mjs')
  for (const text of [
    '&lt; &gt; &amp; &quot; &apos;',
    '&#60; &#62; &#38; &#34; &#39;',
    '&#x3c; &#x3e; &#x26; &#x22; &#x27;',
    '&#X3C; &#X3E; &#X26; &#X22; &#X27;',
  ]) {
    assert.equal(decodeReturnTransport(text).value, '< > & " \'')
  }
})

test('transport decoding walks values once, preserves keys and unrelated entities', async () => {
  const { decodeReturnTransport } = await import('./plan-adopt-metadata.mjs')
  assert.deepEqual(
    decodeReturnTransport({
      '&lt;key&gt;': ['&amp;lt;id&amp;gt;', { summary: '&nbsp; &copy; bare & &lt;id&gt;' }],
    }),
    {
      value: { '&lt;key&gt;': ['&lt;id&gt;', { summary: '&nbsp; &copy; bare & <id>' }] },
      decoded: ['&lt;key&gt;[0]', '&lt;key&gt;[1].summary'],
    },
  )
  const plain = { labels: ['bug'], nested: { active: true, count: 2, empty: null } }
  assert.deepEqual(decodeReturnTransport(plain), { value: plain, decoded: [] })
})

function metadata(siblings = undefined) {
  const descriptionSummary = [
    ...new Set([...requiredPlanSections(DEFAULT_CONFIG), '## Key changes', '## Planning']),
  ]
    .map(
      (heading) =>
        `${heading}\n\n${heading === '## Planning' ? '- Contract: v1' : 'Bounded summary.'}`,
    )
    .join('\n\n')
    .replace(
      '## Key changes\n\nBounded summary.',
      '## Key changes\n\n- `skills-toolbox/plan/plan-run-guards.mjs`: adopt metadata.',
    )
  return {
    planPath: '.linear-plans/SRC-1.md',
    labels: ['bug'],
    agentBuild: true,
    estimate: 2,
    priority: 3,
    openQuestions: ['Use &lt;id&gt;?'],
    descriptionSummary: siblings
      ? descriptionSummary.replace(
          '- Contract: v1',
          '- Contract: v1\n- Sibling-split: keep first part; split rendering.',
        )
      : descriptionSummary,
    ...(siblings ? { siblings } : {}),
  }
}

test('adopt-metadata delivers decoded sibling titles and bodies through the sibling CLI', (t) => {
  const dir = tempDir('plan-adopt-metadata-', t)
  const metadataPath = path.join(dir, 'draft-metadata.json')
  const returned = metadata([
    {
      key: 'render',
      title: 'Render &lt;placeholder&gt; ids',
      estimate: 2,
      summary: 'Wrap `<id>` in a code span &amp; test it'.replace('<id>', '&lt;id&gt;'),
      keyChanges: ['src/render.mjs — render &lt;id&gt;'],
      blockedBy: ['source'],
    },
  ])
  const adopted = spawnSync(
    process.execPath,
    [guard, 'adopt-metadata', metadataPath, JSON.stringify(returned)],
    { encoding: 'utf8' },
  )
  assert.equal(adopted.status, 0, adopted.stderr)
  assert.match(adopted.stderr, /warning:.*siblings\[0\]\.summary/)
  const stored = JSON.parse(readFileSync(metadataPath, 'utf8'))
  assert.deepEqual(stored.openQuestions, ['Use <id>?'])
  assert.equal(stored.siblings[0].title, 'Render <placeholder> ids')
  assert.equal(stored.siblings[0].summary, 'Wrap `<id>` in a code span & test it')
  const inputPath = path.join(dir, 'siblings-in.json')
  writeFileSync(
    inputPath,
    JSON.stringify({
      sourceId: 'SRC-1',
      sourceTitle: 'Render ids',
      parentId: 'PAR-1',
      siblings: stored.siblings,
      existingChildren: [],
    }),
  )
  const split = spawnSync(
    process.execPath,
    [siblingCli, 'plan', '--input', inputPath, '--out-dir', dir],
    { encoding: 'utf8' },
  )
  assert.equal(split.status, 0, split.stderr)
  const created = JSON.parse(split.stdout).creates[0]
  assert.equal(created.title, 'Render <placeholder> ids')
  const body = readFileSync(created.bodyFile, 'utf8')
  assert.match(body, /`<id>` in a code span & test it/)
  assert.doesNotMatch(body, /&lt;|&amp;/)
})

test('metadata files that did not cross the transport retain literal entity text', (t) => {
  const dir = tempDir('plan-metadata-literal-', t)
  const metadataPath = path.join(dir, 'draft-metadata.json')
  writeFileSync(metadataPath, JSON.stringify(metadata()))
  const result = spawnSync(process.execPath, [guard, 'metadata', metadataPath], {
    encoding: 'utf8',
  })
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(readFileSync(metadataPath, 'utf8')).openQuestions, [
    'Use &lt;id&gt;?',
  ])
})
