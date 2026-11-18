import assert from 'node:assert/strict'
import test from 'node:test'
import { spawnSync } from 'node:child_process'
import { writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  DEFAULT_CONFIG,
  DESCRIPTION_NORMALIZATION_TRANSFORMS,
  mergeConfig,
} from '../skill-config.mjs'
import { verifyWriteback } from '../plan/plan-writeback-verify.mjs'
import {
  canonicalizeBareReferences,
  canonicalizePullRequestReferences,
  normalizeReferenceForms,
  REFERENCE_TRANSFORM_IDS,
} from './reference-forms.mjs'

const ISSUE_URL = 'https://linear.app/demo/issue/ABC-1199/title'
const PR_URL = 'https://linear.app/demo/review/branch'
const issue = `[ABC-1199](${ISSUE_URL})`
const pr = `[owner/repo#4135](${PR_URL})`
const issueElement = `<issue id="x" label="ABC-1199" href="${ISSUE_URL}" />`
const prElement = `<pull-request href="${PR_URL}" label="owner/repo#4135" id="y" />`
const ALL = new Set(DESCRIPTION_NORMALIZATION_TRANSFORMS)
const WITHOUT = new Set([...ALL].filter((id) => !REFERENCE_TRANSFORM_IDS.includes(id)))
const guard = fileURLToPath(new URL('../plan/plan-image-guard.mjs', import.meta.url))
const dir = tempDir('reference-forms-')
process.env.BOSS_GATE_OUTCOME_FILE = path.join(dir, 'outcomes.tsv')

const configWith = (tolerated) =>
  mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        descriptionNormalization: { tolerated: [...tolerated] },
      },
    },
  })
const description = (notes) =>
  [
    '## Summary\n\nPreserve reporter references.',
    '## Approach\n\nCompare both forms.',
    '## Key changes\n\n- shared reference normalizer',
    '## Testing\n\n- regression coverage',
    '## Risks / unknowns\n\n- malformed mentions stay distinct',
    '## Acceptance criteria\n\n- [ ] references survive',
    '## Required proof\n\n- [ ] (backend-only) unit tests',
    '## Planning\n\n- Contract: v1',
    `## Original notes\n\n${notes}`,
  ].join('\n\n')

const fixtures = [
  { name: 'plain control', original: '#4135 ABC-1199', stored: '#4135 ABC-1199', expectPass: true },
  { name: 'issue link', original: 'ABC-1199', stored: issue, expectPass: true },
  {
    name: 'issue angle destination',
    original: 'ABC-1199',
    stored: `[ABC-1199](<${ISSUE_URL}>)`,
    expectPass: true,
  },
  { name: 'issue element', original: 'ABC-1199', stored: issueElement, expectPass: true },
  { name: 'PR link', original: '#4135', stored: pr, expectPass: true },
  {
    name: 'PR angle destination',
    original: '#4135',
    stored: `[owner/repo#4135](<${PR_URL}>)`,
    expectPass: true,
  },
  { name: 'PR element', original: '#4135', stored: prElement, expectPass: true },
  {
    name: 'escaped repository underscore',
    original: '#4135',
    stored: `[owner/repo\\_name#4135](${PR_URL})`,
    expectPass: true,
  },
  {
    name: 'combined references',
    original: 'See ABC-1199 and #4135.',
    stored: `See ${issue} and ${pr}.`,
    expectPass: true,
  },
  {
    name: 'nested heading',
    original: '## Original notes\n\nABC-1199 #4135',
    stored: `## Original notes\n\n${issueElement} ${prElement}`,
    expectPass: true,
  },
  {
    name: 'changed PR number',
    original: '#4135',
    stored: pr.replace('#4135', '#4136'),
    expectPass: false,
  },
  {
    name: 'changed issue label',
    original: 'ABC-1199',
    stored: issueElement.replace('label="ABC-1199"', 'label="ABC-1200"'),
    expectPass: false,
  },
  {
    name: 'mismatched issue destination',
    original: 'ABC-1199',
    stored: issue.replace('/issue/ABC-1199/', '/issue/ABC-1200/'),
    expectPass: false,
  },
  {
    name: 'invalid PR label',
    original: '#4135',
    stored: `[repo#4135](${PR_URL})`,
    expectPass: false,
  },
  { name: 'dropped reference', original: 'See #4135.', stored: 'See .', expectPass: false },
  {
    name: 'bare owner repository stays distinct',
    original: 'owner/repo#4135',
    stored: pr,
    expectPass: false,
  },
  { name: 'inline code', original: '`#4135`', stored: '`' + pr + '`', expectPass: false },
  {
    name: 'fenced code',
    original: '````\n#4135\n~~~\n````',
    stored: '````\n' + pr + '\n~~~\n````',
    expectPass: false,
  },
  {
    name: 'quoted fenced code',
    original: '> ```markdown\n> #4135\n> ```',
    stored: '> ```markdown\n> ' + pr + '\n> ```',
    expectPass: false,
  },
  {
    name: 'nested quoted fenced code',
    original: '> > ~~~\n> > #4135\n> > ~~~',
    stored: '> > ~~~\n> > ' + pr + '\n> > ~~~',
    expectPass: false,
  },
  {
    name: 'quote ends an unclosed quoted fence',
    original: '> ```\n> sample\n\n#4135',
    stored: '> ```\n> sample\n\n' + pr,
    expectPass: true,
  },
  {
    name: 'quoted prose reference',
    original: '> #4135',
    stored: '> ' + pr,
    expectPass: true,
  },
  { name: 'indented code', original: '    #4135', stored: '    ' + pr, expectPass: false },
  { name: 'image', original: '#4135', stored: '!' + pr, expectPass: false },
  { name: 'escaped link', original: '#4135', stored: '\\' + pr, expectPass: false },
  {
    name: 'titled link',
    original: '#4135',
    stored: `[owner/repo#4135](${PR_URL} "title")`,
    expectPass: false,
  },
  {
    name: 'relative destination',
    original: '#4135',
    stored: '[owner/repo#4135](/review/branch)',
    expectPass: false,
  },
  {
    name: 'zero PR number',
    original: '#0',
    stored: `[owner/repo#0](${PR_URL})`,
    expectPass: false,
  },
]

test('reference normalizers preserve near misses, literal contexts and idempotence', () => {
  for (const fixture of fixtures) {
    const original = normalizeReferenceForms(fixture.original, ALL)
    const stored = normalizeReferenceForms(fixture.stored, ALL)
    assert.equal(original === stored, fixture.expectPass, fixture.name)
    assert.equal(normalizeReferenceForms(stored, ALL), stored, fixture.name)
    assert.equal(normalizeReferenceForms(fixture.stored, new Set()), fixture.stored)
  }
  assert.equal(canonicalizeBareReferences(issueElement), 'ABC-1199')
  assert.equal(canonicalizePullRequestReferences(prElement), '#4135')
  assert.equal(canonicalizePullRequestReferences(pr), '#4135')
  for (const literal of [
    '!' + pr,
    '\\' + pr,
    '`' + pr + '`',
    '````\n' + pr + '\n````',
    '    ' + pr,
    '> ```markdown\n> ' + pr + '\n> ```',
    '> > ~~~\n> > ' + pr + '\n> > ~~~',
    '> ```\n> > ' + pr + '\n> ```',
  ]) {
    assert.equal(canonicalizePullRequestReferences(literal), literal)
  }
})

test('both description gates agree under default and explicitly restricted reference tolerances', () => {
  for (const [label, tolerated] of [
    ['default', ALL],
    ['without reference transforms', WITHOUT],
  ]) {
    const config = configWith(tolerated)
    // Exercise the absent-block defaults as well as an explicit narrow declaration.
    writeFileSync(
      path.join(dir, '.boss-skills.json'),
      JSON.stringify(label === 'default' ? {} : config),
    )
    for (const fixture of fixtures) {
      const intendedText = description(fixture.original)
      const storedText = description(fixture.stored)
      const result = verifyWriteback({ config, intendedText, storedText })
      writeFileSync(path.join(dir, 'original.md'), fixture.original)
      writeFileSync(path.join(dir, 'stored.md'), storedText)
      const cli = spawnSync(
        process.execPath,
        [
          guard,
          '--original',
          path.join(dir, 'original.md'),
          '--rewritten',
          path.join(dir, 'stored.md'),
          '--require-verbatim',
        ],
        { cwd: dir, encoding: 'utf8', env: { ...process.env, BOSS_PLAN_TOOLBOX: '' } },
      )
      const pass = ['byte-exact', 'normalized-equivalent'].includes(result.verdict)
      const expected =
        label === 'default' ? fixture.expectPass : fixture.original === fixture.stored
      assert.equal(pass, expected, `${label}: ${fixture.name}: ${result.reason}`)
      assert.equal(cli.status === 0, pass, `${label}: ${fixture.name}: ${cli.stderr}`)
    }
  }
})

test('combined issue and PR notes verify end to end for link and element readbacks', () => {
  for (const stored of [`See ${issue} and ${pr}.`, `See ${issueElement} and ${prElement}.`]) {
    const result = verifyWriteback({
      config: configWith(ALL),
      intendedText: description('See ABC-1199 and #4135.'),
      storedText: description(stored),
    })
    assert.equal(result.verdict, 'normalized-equivalent')
  }
  const tolerated = new Set([...ALL].filter((id) => id !== 'pull-request-reference-autolinking'))
  const result = verifyWriteback({
    config: configWith(tolerated),
    intendedText: description('#4135'),
    storedText: description(pr),
  })
  assert.equal(result.verdict, 'drift')
  assert.equal(result.cause, 'notes')
})
