import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { loadSkillConfig } from '../skill-config.mjs'
import { emittedContractHeadings, isContractOrdered } from './plan-contract-guard.mjs'
import { originalNotesBodies } from './plan-image-guard.mjs'
import { foldSection } from './plan-fold-section.mjs'

process.env.BOSS_GATE_OUTCOME_FILE = join(tempDir('gate-outcome-suite-'), 'outcomes.tsv')

const config = loadSkillConfig()
const heading = '## Feature map changes'
const body = '- add feature.AC1: observable behavior'

test('free-form plan falls back to the original-notes wrapper', () => {
  const result = foldSection(
    config,
    '# Plan\n\n## Changes\n\nChange\n\n## Original notes\nOriginal',
    heading,
    body,
  )
  assert.equal(result.action, 'inserted')
  assert.equal(result.before, '## Original notes')
  assert.ok(result.text.includes(`${heading}\n\n${body}\n\n## Original notes`))
})

for (const middle of [
  '## Key changes\n\nChange',
  '## Proof harness analysis\n\nProof\n\n## Why this needs a human\n\nReason',
]) {
  test(`description uses contract order after ${middle.split('\n')[0]}`, () => {
    const source = `## Summary\n\nSummary\n\n${middle}\n\n## Planning\n\nPlanning\n\n## Original notes\nOriginal`
    const result = foldSection(config, source, heading, body)
    assert.equal(
      result.before,
      middle.includes('human') ? '## Why this needs a human' : '## Planning',
    )
    assert.ok(isContractOrdered(config, emittedContractHeadings(config, result.text)))
    const replacement = foldSection(config, result.text, '## FEATURE MAP CHANGES:', '- replacement')
    assert.equal(replacement.action, 'replaced')
    assert.equal(replacement.text.split(heading).length, 2)
    assert.ok(replacement.text.includes('- replacement'))
    assert.ok(!replacement.text.includes(body))
    assert.deepEqual(foldSection(config, replacement.text, heading, '- replacement'), replacement)
  })
}

test('fenced headings are ignored and the complete original snapshot stays byte-identical', () => {
  const snapshot =
    '## Original notes\r\n\r\nSnapshot\r\n## Original notes\r\n## Planning\r\n```\r\n## Planning\r\n```\r\n'
  const source = '# Plan\n\n```md\n## Original notes\n## Planning\n```\n\n' + snapshot
  const result = foldSection(config, source, heading, body)
  assert.equal(result.before, '## Original notes')
  assert.ok(result.text.endsWith(snapshot))
  assert.deepEqual(originalNotesBodies(result.text), originalNotesBodies(source))
})

test('replacement preserves a variant heading and the next free-form section', () => {
  const source = `# Plan\n\n## FEATURE MAP CHANGES:\n\nOld\n\n## Risks\n\nRisk\n\n## Original notes\nOriginal`
  const result = foldSection(config, source, heading, body)
  assert.equal(result.action, 'replaced')
  assert.equal(result.before, '## Risks')
  assert.ok(result.text.includes(`${heading}\n\n${body}\n\n## Risks\n\nRisk`))
})

test('CLI folds both files and refuses invalid inputs without writing either', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'plan-fold-section-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const plan = join(dir, 'plan.md')
  const description = join(dir, 'description.md')
  const bodyFile = join(dir, 'body.md')
  const original = '## Summary\n\nSummary\n\n## Original notes\nOriginal\n'
  const run = (selectedHeading = heading) =>
    spawnSync(
      process.execPath,
      [
        'skills-toolbox/plan/plan-fold-section.mjs',
        'fold',
        '--heading',
        selectedHeading,
        '--body-file',
        bodyFile,
        '--plan',
        plan,
        '--description',
        description,
        '--json',
      ],
      { encoding: 'utf8' },
    )
  writeFileSync(plan, original)
  writeFileSync(description, original)
  writeFileSync(bodyFile, body)
  const result = run()
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(result.stdout), {
    plan: { action: 'inserted', before: '## Original notes' },
    description: { action: 'inserted', before: '## Original notes' },
  })
  for (const scenario of [
    'unknown',
    'terminal',
    'plan-wrapper',
    'description-wrapper',
    'blank',
    'missing',
  ]) {
    writeFileSync(plan, scenario === 'plan-wrapper' ? '# Plan' : original)
    writeFileSync(description, scenario === 'description-wrapper' ? '# Description' : original)
    writeFileSync(bodyFile, scenario === 'blank' ? ' \n' : body)
    if (scenario === 'missing') rmSync(bodyFile)
    const before = [readFileSync(plan, 'utf8'), readFileSync(description, 'utf8')]
    const failed = run(
      scenario === 'unknown'
        ? '## Unknown'
        : scenario === 'terminal'
          ? '## Original notes'
          : heading,
    )
    assert.notEqual(failed.status, 0, scenario)
    assert.deepEqual(
      [readFileSync(plan, 'utf8'), readFileSync(description, 'utf8')],
      before,
      scenario,
    )
  }
})
