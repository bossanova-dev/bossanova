// Coverage for the producer-side plan-contract guard (BOS-741). Every violation code must fire on
// a minimal fixture, and every documented non-violation must pass — in particular the two scoping
// guarantees (`## Original notes` is not scanned for placeholders; a plan that merely DOCUMENTS the
// scaffolding element names is not residue), which are the regressions most likely to be
// "simplified" away later. Node builtins only.
import { describe, test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  checkPlanCitations,
  checkPlanContract,
  DYNAMIC_VIOLATION_CODE_PREFIXES,
  VIOLATION_CODES,
  checkPlanFileStructure,
  checkPrBodyOnlyEvidence,
  checkVerifyOnlyCommandVacuity,
  checkSelfFalsifiedLiteralSearch,
  emittedContractHeadings,
  hasUnterminatedFence,
  isContractOrdered,
  lineSpanningEmphasis,
  linesOutsideFences,
  parseContractGuardArgs,
  placeholderResidue,
  planFileResidue,
} from './plan-contract-guard.mjs'
import { DEFAULT_CONFIG, planSections, requiredPlanSections } from './skill-config.mjs'

const GUARD = fileURLToPath(new URL('./plan-contract-guard.mjs', import.meta.url))

const codes = (result) => result.violations.map((v) => v.code)

// A conformant description: every required heading, in contract order, with a v1 stamp and enough
// body to clear the byte floor.
const conformant = (planningLine = '- Contract: v1') =>
  `${requiredPlanSections(DEFAULT_CONFIG)
    .map((h) => `${h}\n\nSubstantive body prose for this section, long enough to be a real plan.`)
    .join('\n\n')
    .replace(
      '## Planning\n\nSubstantive body prose for this section, long enough to be a real plan.',
      `## Planning\n\n${planningLine}`,
    )}\n`

const epicParentConformant = () =>
  [
    '## Summary\n\nSubstantive epic overview prose, long enough to clear the byte floor.',
    '## Child tickets\n\n- [ ] BOS-1 — child plan covering the first bounded slice.\n- [ ] BOS-2 — child plan covering the dependent slice.',
    '## Planning\n\n- Contract: v1\n- Epic parent overview; children carry implementation-plan contracts.',
    '## Original notes\n\nOriginal reporter notes are preserved here with enough content to keep the overview realistic.',
  ].join('\n\n')

// A rich plan file that carries a non-contract heading and merely documents the residue guard: it
// names the scaffolding elements in prose, quotes them inside a fenced block, and ends on ordinary
// prose. The description contract must not reject this attachment structure.
const documentingPlan = `# Plan

## Summary

The plan keeps the description contract sections while preserving richer structure.

## Approach

Use the existing guard.

## Key changes

Touch the config, guard, skill prose, and tests.

## Testing

Run the plan-contract tests.

## Risks / unknowns

The floor is structural only.

## Acceptance criteria

- [ ] The floor blocks flattened copies.

## Required proof

- [ ] Tests are the proof.

## Planning

- Contract: v1

## Problem Frame

The plan attachment has structure beyond the description projection.

## Requirements

- R1: Preserve richer plan structure.

## Implementation Units

The drafting layer keeps its native work-unit structure in the plan attachment.

## Original notes

Original reporter notes.

The guard rejects a plan whose last line is a bare closing invoke or function_calls tag.

\`\`\`xml
</invoke>
</function_calls>
\`\`\`

That is why the check is anchored to the last non-blank line outside fenced code.
`

describe('checkPlanContract — conformant input', () => {
  test('a conformant description passes with a rich non-contract plan heading', () => {
    const result = checkPlanContract({ description: conformant(), plan: documentingPlan })
    assert.deepEqual(result.violations, [])
    assert.equal(result.ok, true)
  })

  test('the exact byte sequence rendered from planSections(DEFAULT_CONFIG) passes', () => {
    // The contract's own rendering must survive its own gate, otherwise the producer can never
    // emit a publishable plan.
    const rendered = `${planSections(DEFAULT_CONFIG)
      .map((s) => `${s.heading}\n\nbody prose that is long enough to matter for the byte floor`)
      .join('\n\n')
      .replace(
        '## Planning\n\nbody prose that is long enough to matter for the byte floor',
        '## Planning\n\n- Contract: v1',
      )}\n`
    const result = checkPlanContract({ description: rendered })
    assert.deepEqual(result.violations, [])
  })

  test('an asterisk-bullet `* Contract: v1` stamp is accepted (tracker renormalises the marker)', () => {
    const result = checkPlanContract({ description: conformant('* Contract: v1') })
    assert.deepEqual(result.violations, [])
  })

  test('a registered `optional` section is recognised, not an unknown heading', () => {
    // `## Proof harness analysis` is the heading the drafting brief's own template emits. It is
    // registered `optional` precisely so strict unknown-heading detection does not reject it.
    const withOptional = conformant().replace(
      '## Planning',
      '## Proof harness analysis\n\nnot applicable\n\n## Planning',
    )
    assert.deepEqual(checkPlanContract({ description: withOptional }).violations, [])
  })

  test('epic-parent mode accepts the epic overview contract', () => {
    const result = checkPlanContract({ description: epicParentConformant(), mode: 'epic-parent' })
    assert.deepEqual(result.violations, [])
    assert.equal(result.ok, true)
  })

  test('epic-parent mode accepts the terse verify-only overview fixture', () => {
    const description =
      '## Summary\n\nx\n\n## Child tickets\n\n- a\n\n## Planning\n\n- Contract: v1\n\n## Original notes\n\nn\n'
    const result = checkPlanContract({ description, mode: 'epic-parent' })
    assert.deepEqual(result.violations, [])
    assert.equal(result.ok, true)
  })

  test('description modes discriminate in both directions', () => {
    const epicAsChild = checkPlanContract({ description: epicParentConformant() })
    assert.equal(epicAsChild.ok, false)
    assert.ok(codes(epicAsChild).includes('missing-sections'))
    assert.ok(codes(epicAsChild).includes('unknown-section'))

    const childAsEpic = checkPlanContract({ description: conformant(), mode: 'epic-parent' })
    assert.equal(childAsEpic.ok, false)
    assert.ok(codes(childAsEpic).includes('missing-sections'))
    assert.ok(codes(childAsEpic).includes('unknown-section'))
  })
})

describe('checkPlanContract — each violation code fires', () => {
  test('missing-sections fires when a required heading is absent', () => {
    const description = conformant().replace(
      '## Testing\n\nSubstantive body prose for this section, long enough to be a real plan.\n\n',
      '',
    )
    const result = checkPlanContract({ description })
    assert.equal(result.ok, false)
    assert.ok(codes(result).includes('missing-sections'))
    assert.match(result.violations.find((v) => v.code === 'missing-sections').message, /## Testing/)
  })

  test('missing-sections fires on a stamped version newer than the contract', () => {
    const result = checkPlanContract({ description: conformant('- Contract: v99') })
    assert.equal(result.ok, false)
    assert.ok(
      result.violations.some((v) => v.code === 'missing-sections' && /v99/.test(v.message)),
      'the unsupported version must be reported separately from the missing headings',
    )
  })

  test('unknown-section fires on an off-contract heading and names the config remedy', () => {
    const description = conformant().replace('## Planning', '## Notes\n\nx\n\n## Planning')
    const result = checkPlanContract({ description })
    assert.equal(result.ok, false)
    const found = result.violations.find((v) => v.code === 'unknown-section')
    assert.ok(found)
    assert.match(found.message, /## Notes/)
    assert.match(found.message, /planContract\.sections/)
  })

  test('section-order fires when contract sections are emitted out of order', () => {
    const body = 'Substantive body prose for this section, long enough to be a real plan.'
    const description = `## Summary\n\n${body}\n\n## Key changes\n\n${body}\n\n## Approach\n\n${body}\n\n${requiredPlanSections(
      DEFAULT_CONFIG,
    )
      .filter((h) => !['## Summary', '## Approach', '## Key changes'].includes(h))
      .map((h) => `${h}\n\n${body}`)
      .join('\n\n')
      .replace(`## Planning\n\n${body}`, '## Planning\n\n- Contract: v1')}\n`
    const result = checkPlanContract({ description })
    assert.equal(result.ok, false)
    assert.ok(codes(result).includes('section-order'))
    // Ordering must be the ONLY complaint: nothing is missing and nothing is off-contract.
    assert.ok(!codes(result).includes('missing-sections'))
    assert.ok(!codes(result).includes('unknown-section'))
  })

  test('section-order names the epic-parent order when mode is epic-parent', () => {
    const description = epicParentConformant().replace(
      '## Child tickets',
      '## Planning\n\n- Contract: v1\n\n## Child tickets',
    )
    const result = checkPlanContract({ description, mode: 'epic-parent' })
    assert.equal(result.ok, false)
    const found = result.violations.find((v) => v.code === 'section-order')
    assert.ok(found)
    assert.match(
      found.message,
      /expected the relative order of ## Summary → ## Child tickets → ## Planning → ## Original notes/,
    )
    assert.doesNotMatch(found.message, /## Approach/)
  })

  test('placeholder-residue fires on an unsubstituted token and quotes it', () => {
    const description = conformant().replace(
      '## Planning\n\n- Contract: v1',
      '## Planning\n\n- Contract: v1\n- Attachment: <ATTACHMENT-ID>',
    )
    const result = checkPlanContract({ description })
    assert.equal(result.ok, false)
    const found = result.violations.find((v) => v.code === 'placeholder-residue')
    assert.ok(found)
    assert.match(found.message, /<ATTACHMENT-ID>/)
  })

  test('not-a-description fires on a whole-field self-describing placeholder', () => {
    const result = checkPlanContract({
      description: 'the full markdown plan description as specified in Step 7 of the brief',
    })
    assert.equal(result.ok, false)
    assert.ok(codes(result).includes('not-a-description'))
  })

  test('not-a-description fires on a long body that does not start with the first heading', () => {
    const description = `Here is the plan I drafted.\n\n${conformant()}`
    const result = checkPlanContract({ description })
    assert.equal(result.ok, false)
    assert.ok(
      result.violations.some((v) => v.code === 'not-a-description' && /## Summary/.test(v.message)),
    )
  })

  test('plan-file-residue fires when the plan ends on a bare closing scaffolding tag', () => {
    const result = checkPlanContract({
      description: conformant(),
      plan: `# Plan\n\nSome real plan body.\n\n</invoke>\n`,
    })
    assert.equal(result.ok, false)
    const found = result.violations.find((v) => v.code === 'plan-file-residue')
    assert.ok(found)
    assert.match(found.message, /<\/invoke>/)
  })

  test('plan-file-residue fires on an empty plan file', () => {
    const result = checkPlanContract({ description: conformant(), plan: '   \n\n' })
    assert.equal(result.ok, false)
    assert.ok(codes(result).includes('plan-file-residue'))
  })

  test('plan-file-structure blocks a flattened plan with exactly the description headings', () => {
    const flattened = conformant()
    const result = checkPlanContract({ description: conformant(), plan: flattened })
    assert.equal(result.ok, false)
    const messages = result.violations.map((v) => v.message).join('\n')
    assert.match(messages, /inspected \d+ heading/)
    assert.match(messages, /outside planContract\.sections/)
  })

  test('plan-file-structure passes a plan carrying contract headings plus the configured floor', () => {
    const result = checkPlanFileStructure(DEFAULT_CONFIG, documentingPlan)
    assert.equal(result.ok, true)
    assert.equal(result.headingsInspected > 0, true)
    assert.deepEqual(result.violations, [])
  })

  test('plan-file-structure reports zero headings as a violation', () => {
    const result = checkPlanFileStructure(DEFAULT_CONFIG, 'plain prose only')
    assert.equal(result.ok, false)
    assert.equal(result.headingsInspected, 0)
    assert.match(result.violations.map((v) => v.message).join('\n'), /inspected 0 heading/)
  })

  test('plan-file-structure blocks bypass shapes from the input grammar', () => {
    const withReplacement = (replacement) =>
      documentingPlan.replace(
        '## Problem Frame\n\nThe plan attachment',
        `${replacement}\n\nThe plan attachment`,
      )

    for (const [label, plan] of [
      ['bold pseudo-heading', withReplacement('**Problem Frame**')],
      ['heading only inside a fenced block', withReplacement('```md\n## Problem Frame\n```')],
      [
        'heading only inside an inline code span',
        withReplacement('The heading is `## Problem Frame`.'),
      ],
      [
        'declared block present but empty',
        documentingPlan.replace(
          '## Requirements\n\n- R1: Preserve richer plan structure.',
          '## Requirements\n\n',
        ),
      ],
      ['four-space indented code heading', withReplacement('    ## Problem Frame')],
    ]) {
      const result = checkPlanFileStructure(DEFAULT_CONFIG, plan)
      assert.equal(result.ok, false, `${label} must be blocked`)
      assert.ok(codes(result).includes('plan-file-structure'), `${label} must use structure code`)
    }
  })

  test('plan-file-structure does not count headings inside terminal Original notes', () => {
    const plan = documentingPlan
      .replace(
        '## Problem Frame\n\nThe plan attachment has structure beyond the description projection.\n\n',
        '',
      )
      .replace('Original reporter notes.', 'Original reporter notes.\n\n## Problem Frame\n\nquoted')
    const result = checkPlanFileStructure(DEFAULT_CONFIG, plan)
    assert.equal(result.ok, false)
    assert.ok(codes(result).includes('plan-file-structure'))
    assert.match(
      result.violations.map((v) => v.message).join('\n'),
      /missing required plan-file heading "## Problem Frame"/,
    )
  })

  test('plan-file-structure rejects unterminated fences instead of counting hidden headings', () => {
    const plan = documentingPlan.replace(
      '## Requirements\n\n- R1: Preserve richer plan structure.',
      '~~~md\n## Requirements\n\n- R1: Preserve richer plan structure.',
    )
    assert.equal(hasUnterminatedFence(plan), true)
    const result = checkPlanFileStructure(DEFAULT_CONFIG, plan)
    assert.equal(result.ok, false)
    assert.ok(codes(result).includes('plan-file-structure'))
    assert.match(
      result.violations.map((v) => v.message).join('\n'),
      /unterminated fenced code block/,
    )
  })

  test('plan-file-structure exemptions are explicit and closed', () => {
    for (const exemption of ['epic-parent-overview', 'adopted-child-redraft', 'consumer']) {
      const result = checkPlanFileStructure(DEFAULT_CONFIG, 'plain prose only', { exemption })
      assert.equal(result.ok, true, `${exemption} must exempt the floor`)
      assert.deepEqual(result.violations, [])
    }

    const unknown = checkPlanFileStructure(DEFAULT_CONFIG, 'plain prose only', {
      exemption: 'future-shape',
    })
    assert.equal(unknown.ok, false)
    assert.deepEqual(codes(unknown), ['plan-file-structure-exemption'])
  })

  test('self-falsified-literal-search fires when a criterion forbids text the plan mandates', () => {
    const descriptionFor = (check) =>
      conformant()
        .replace(
          '## Key changes\n\nSubstantive body prose for this section, long enough to be a real plan.',
          '## Key changes\n\nAdd the exact phrase `must stay visible` to the skill body.',
        )
        .replace(
          '## Acceptance criteria\n\nSubstantive body prose for this section, long enough to be a real plan.',
          `## Acceptance criteria\n\n- [ ] No mandated text remains — check: \`${check}\``,
        )
    const description = descriptionFor('rg -F "must stay visible" services/boss')
    const result = checkPlanContract({ description })
    assert.ok(codes(result).includes('self-falsified-literal-search'))

    for (const check of [
      'rg -Fq "must stay visible" services/boss',
      'grep -qF "must stay visible" services/boss',
      'grep -Fq "must stay visible" services/boss',
    ]) {
      assert.ok(
        codes(checkPlanContract({ description: descriptionFor(check) })).includes(
          'self-falsified-literal-search',
        ),
        `${check} should be treated as a fixed-string search`,
      )
    }

    const control = description.replace(
      'must stay visible" services/boss',
      'not mandated" services/boss',
    )
    assert.deepEqual(checkSelfFalsifiedLiteralSearch(DEFAULT_CONFIG, control), [])
  })

  test('unresolvable-citation rejects paths that escape the working tree', () => {
    const parent = mkdtempSync(path.join(tmpdir(), 'plan-contract-citation-parent-'))
    const dir = path.join(parent, 'repo')
    mkdirSync(dir)
    writeFileSync(path.join(parent, 'outside.md'), 'one\n')
    const description = conformant().replace(
      '## Acceptance criteria\n\nSubstantive body prose for this section, long enough to be a real plan.',
      '## Acceptance criteria\n\n- [ ] ../outside.md:1 must not resolve',
    )
    const result = checkPlanCitations(DEFAULT_CONFIG, description, { cwd: dir })
    assert.deepEqual(
      result.violations.map((v) => v.code),
      ['unresolvable-citation'],
    )
    assert.match(result.violations[0].message, /escapes the working tree root/)
  })

  test('unresolvable-citation fires only in the scanned sections', () => {
    const dir = mkdtempSync(path.join(tmpdir(), 'plan-contract-citation-'))
    writeFileSync(path.join(dir, 'present.md'), 'one\ntwo\n')
    const description = conformant()
      .replace(
        '## Acceptance criteria\n\nSubstantive body prose for this section, long enough to be a real plan.',
        [
          '## Premises',
          '',
          '- [ ] `two` at present.md:2 resolves',
          '- [ ] `two` at present.md:5 does not',
          '',
          '## Acceptance criteria',
          '',
          '- [ ] missing.md:1 does not',
        ].join('\n'),
      )
      .replace(
        '## Original notes\n\nSubstantive body prose for this section, long enough to be a real plan.',
        '## Original notes\n\nstale.md:999 is quoted source material, not scanned',
      )
    const result = checkPlanCitations(DEFAULT_CONFIG, description, { cwd: dir })
    assert.deepEqual(
      result.violations.map((v) => v.code),
      ['unresolvable-citation', 'unresolvable-citation'],
    )
    assert.match(result.violations[0].message, /present\.md:5/)
    assert.match(result.violations[1].message, /missing\.md:1/)
    assert.doesNotMatch(result.violations.map((v) => v.message).join('\n'), /stale\.md/)
  })

  // BOS-1186: `## Key changes` pins call sites as often as a criterion does, and nothing read them.
  test('unresolvable-citation covers `## Key changes` in both directions', () => {
    const dir = mkdtempSync(path.join(tmpdir(), 'plan-contract-key-changes-'))
    writeFileSync(path.join(dir, 'present.md'), 'one\ntwo\n')
    const keyChanges = (bullet) =>
      conformant().replace(
        '## Key changes\n\nSubstantive body prose for this section, long enough to be a real plan.',
        `## Key changes\n\n${bullet}`,
      )

    const past = checkPlanCitations(DEFAULT_CONFIG, keyChanges('- `present.md:9` is rewritten'), {
      cwd: dir,
    })
    assert.deepEqual(
      past.violations.map((v) => v.code),
      ['unresolvable-citation'],
    )
    assert.match(past.violations[0].message, /##\s+Key\s+changes\s+cites\s+present\.md:9/)

    const resolves = checkPlanCitations(
      DEFAULT_CONFIG,
      keyChanges('- `present.md:2` is rewritten'),
      { cwd: dir },
    )
    assert.deepEqual(resolves.violations, [])

    // A `## Key changes` bullet naming a file this ticket will CREATE carries no `:<line>` suffix,
    // so it is not a citation at all — this is why the widening cannot false-positive.
    const creates = checkPlanCitations(
      DEFAULT_CONFIG,
      keyChanges('- `not-yet-created.md`: new module added by this ticket'),
      { cwd: dir },
    )
    assert.deepEqual(creates.violations, [])
  })

  describe('premise citation anchors (BOS-1186)', () => {
    const anchorFixture = () => {
      const dir = mkdtempSync(path.join(tmpdir(), 'plan-contract-anchor-'))
      writeFileSync(
        path.join(dir, 'target.mjs'),
        ['zero', 'one', 'const ANCHOR_TOKEN = 1', 'three', 'four'].join('\n') + '\n',
      )
      return dir
    }
    const premises = (bullet) =>
      conformant().replace(
        '## Acceptance criteria\n\nSubstantive body prose for this section, long enough to be a real plan.',
        [
          '## Premises',
          '',
          bullet,
          '',
          '## Acceptance criteria',
          '',
          '- [ ] the criterion is unrelated to the anchor rule',
        ].join('\n'),
      )

    test('a premise whose only span is its check command is unanchored', () => {
      const result = checkPlanCitations(
        DEFAULT_CONFIG,
        premises('- [ ] target.mjs:3 still pins the token — check: `sed -n 3p target.mjs`'),
        { cwd: anchorFixture() },
      )
      assert.deepEqual(
        result.violations.map((v) => v.code),
        ['unanchored-premise-citation'],
      )
      assert.match(
        result.violations[0].message,
        /add\s+a\s+backticked\s+token\s+copied\s+from\s+that\s+location/,
      )
    })

    test('a premise anchored on a token that has moved away is stale', () => {
      const result = checkPlanCitations(
        DEFAULT_CONFIG,
        premises('- [ ] the `GONE_TOKEN` at `target.mjs:3` — check: `sed -n 3p target.mjs`'),
        { cwd: anchorFixture() },
      )
      assert.deepEqual(
        result.violations.map((v) => v.code),
        ['stale-premise-citation'],
      )
      assert.match(result.violations[0].message, /within\s+5\s+of\s+line\s+3\s+in\s+target\.mjs/)
      assert.match(result.violations[0].message, /"GONE_TOKEN"/)
    })

    test('a premise whose anchor resolves in the window raises neither violation', () => {
      const result = checkPlanCitations(
        DEFAULT_CONFIG,
        premises('- [ ] the `ANCHOR_TOKEN` at `target.mjs:3` — check: `sed -n 3p target.mjs`'),
        { cwd: anchorFixture() },
      )
      assert.deepEqual(result.violations, [])
    })

    test('the escape hatch is citing the file without a line number', () => {
      const result = checkPlanCitations(
        DEFAULT_CONFIG,
        premises(
          '- [ ] `target.mjs` still pins the token — check: `rg -n ANCHOR_TOKEN target.mjs`',
        ),
        { cwd: anchorFixture() },
      )
      assert.deepEqual(result.violations, [])
    })

    test('the anchor rule is scoped to `## Premises`', () => {
      const dir = anchorFixture()
      const inCriteria = conformant().replace(
        '## Acceptance criteria\n\nSubstantive body prose for this section, long enough to be a real plan.',
        '## Acceptance criteria\n\n- [ ] target.mjs:3 still pins the token',
      )
      assert.deepEqual(checkPlanCitations(DEFAULT_CONFIG, inCriteria, { cwd: dir }).violations, [])

      const inKeyChanges = conformant().replace(
        '## Key changes\n\nSubstantive body prose for this section, long enough to be a real plan.',
        '## Key changes\n\n- target.mjs:3 gains the new branch',
      )
      assert.deepEqual(
        checkPlanCitations(DEFAULT_CONFIG, inKeyChanges, { cwd: dir }).violations,
        [],
      )
    })

    test('a premise citation whose file is missing reports only the existence defect', () => {
      const result = checkPlanCitations(
        DEFAULT_CONFIG,
        premises('- [ ] the `ANCHOR_TOKEN` at `absent.mjs:3` — check: `sed -n 3p absent.mjs`'),
        { cwd: anchorFixture() },
      )
      assert.deepEqual(
        result.violations.map((v) => v.code),
        ['unresolvable-citation'],
      )
    })
  })

  test('VIOLATION_CODES covers every static violation literal in the module', () => {
    const source = readFileSync(GUARD, 'utf8')
    const emitted = new Set(
      [...source.matchAll(/violation\(\s*'([a-z0-9-]+)'/g)].map((match) => match[1]),
    )
    assert.ok(emitted.size > 0, 'the extraction must find literals, or it proves nothing')
    for (const code of emitted) {
      assert.ok(
        VIOLATION_CODES.includes(code),
        `VIOLATION_CODES is missing the emitted code ${code}`,
      )
    }
    // ...and the REVERSE direction, or removal rots silently. A code deleted from the module would
    // otherwise linger in this export advertising something the guard can no longer emit — and,
    // because the prose lists in the two skill bodies are ratcheted against this export, stay
    // mandatory in four copies with nothing reading them. That is the copied-forward-claim rot the
    // export exists to retire, so the anti-rot chain has to close in both directions.
    for (const code of VIOLATION_CODES) {
      assert.ok(
        emitted.has(code),
        `VIOLATION_CODES declares ${code}, which no violation() literal in the module emits`,
      )
    }
    assert.deepEqual(
      VIOLATION_CODES,
      [...VIOLATION_CODES].sort(),
      'VIOLATION_CODES must stay sorted so a prose list can be diffed against it',
    )
    assert.deepEqual(
      VIOLATION_CODES,
      [...new Set(VIOLATION_CODES)],
      'VIOLATION_CODES must not repeat a code',
    )
    // The dynamic family is a documented residual, not an enumerated member.
    assert.deepEqual(DYNAMIC_VIOLATION_CODE_PREFIXES, ['vacuous-*'])
    for (const code of VIOLATION_CODES) {
      assert.doesNotMatch(code, /^vacuous-/)
    }
  })

  test('pr-body-only-evidence fires unless the criterion is verify-only or orchestrator-owned', () => {
    const description = conformant().replace(
      '## Acceptance criteria\n\nSubstantive body prose for this section, long enough to be a real plan.',
      [
        '## Acceptance criteria',
        '',
        '- [ ] The PR body lists the mutation result.',
        '- [ ] (verify-only) The PR body records the run — check: `gh pr view`',
        '- [ ] orchestrator-owned: The PR body contains the Linear link.',
      ].join('\n'),
    )
    const findings = checkPrBodyOnlyEvidence(DEFAULT_CONFIG, description)
    assert.equal(findings.length, 1)
    assert.equal(findings[0].code, 'pr-body-only-evidence')
  })

  test('verify-only command vacuity guard covers criteria and premises', () => {
    const description = conformant().replace(
      '## Acceptance criteria\n\nSubstantive body prose for this section, long enough to be a real plan.',
      [
        '## Premises',
        '',
        '- [ ] (central) premise has a vacuous check — check: `git diff -- skills-toolbox/skill-config.mjs`',
        '',
        '## Acceptance criteria',
        '',
        '- [ ] (verify-only) criterion has a missing binary — check: `this-command-does-not-exist-bos1015`',
      ].join('\n'),
    )
    const findings = checkVerifyOnlyCommandVacuity(DEFAULT_CONFIG, description)
    assert.deepEqual(
      findings.map((finding) => finding.code),
      ['vacuous-criterion-command-command-unresolvable'],
    )
  })

  // The vacuity check delegates to `classifyCheckCommand` verbatim and only re-labels the blocking
  // code, so it inherits the `!` negation fix with NO edit of its own. Pinning that here is what
  // stops a future change re-implementing head resolution locally and re-breaking the shape only in
  // this caller. Both directions again: negation accepted, unresolvable head still `vacuous-…`.
  test('verify-only command vacuity guard inherits the negation fix without its own change', () => {
    const withCheck = (command) =>
      conformant().replace(
        '## Acceptance criteria\n\nSubstantive body prose for this section, long enough to be a real plan.',
        [
          '## Acceptance criteria',
          '',
          `- [ ] (verify-only) criterion — check: \`${command}\``,
        ].join('\n'),
      )

    // The head must be PATH-guaranteed on a bare CI runner — see the sibling note in
    // `skill-config.test.mjs`; `rg` is not installed on GitHub's ubuntu image.
    assert.deepEqual(
      checkVerifyOnlyCommandVacuity(DEFAULT_CONFIG, withCheck('! grep -rq needle skills-toolbox')),
      [],
      'a negated absence assertion is a legitimate verify-only check',
    )
    assert.deepEqual(
      checkVerifyOnlyCommandVacuity(
        DEFAULT_CONFIG,
        withCheck('! this-command-does-not-exist-bos1189'),
      ).map((finding) => finding.code),
      ['vacuous-criterion-command-command-unresolvable'],
      'negating an unknown head must not make it resolve',
    )
  })

  test('verify-only command vacuity guard does not block advisory command risks', () => {
    const description = conformant().replace(
      '## Acceptance criteria\n\nSubstantive body prose for this section, long enough to be a real plan.',
      [
        '## Premises',
        '',
        '- [ ] (central) premise has an advisory check — check: `git diff -- skills-toolbox/skill-config.mjs`',
        '',
        '## Acceptance criteria',
        '',
        '- [ ] (verify-only) criterion has an advisory pipeline — check: `make test | tee out.log`',
      ].join('\n'),
    )
    assert.deepEqual(checkVerifyOnlyCommandVacuity(DEFAULT_CONFIG, description), [])
  })

  test('verify-only command vacuity guard stays silent for sound commands', () => {
    const description = conformant().replace(
      '## Acceptance criteria\n\nSubstantive body prose for this section, long enough to be a real plan.',
      [
        '## Premises',
        '',
        '- [ ] (central) premise — check: `make test-scripts`',
        '',
        '## Acceptance criteria',
        '',
        '- [ ] (verify-only) criterion — check: `node --test skills-toolbox/skill-config.test.mjs`',
      ].join('\n'),
    )
    assert.deepEqual(checkVerifyOnlyCommandVacuity(DEFAULT_CONFIG, description), [])
  })

  test('citation could-not-evaluate is separate from clean and violation', () => {
    const result = checkPlanCitations(DEFAULT_CONFIG, conformant(), { cwd: '' })
    assert.deepEqual(result.violations, [])
    assert.deepEqual(
      result.couldNotEvaluate.map((item) => item.code),
      ['citation-could-not-evaluate'],
    )

    const full = checkPlanContract({ description: conformant(), citationCwd: '' })
    assert.equal(full.ok, false)
    assert.deepEqual(full.violations, [])
    assert.deepEqual(
      full.couldNotEvaluate.map((item) => item.code),
      ['citation-could-not-evaluate'],
    )
  })

  test('a namespace-prefixed closing scaffolding tag is still residue', () => {
    assert.match(planFileResidue('# Plan\n\nbody\n\n</function_calls>\n'), /scaffolding/)
  })
})

describe('checkPlanContract — the scoping guarantees', () => {
  test('SCOPING: a placeholder token and extra `##` headings inside ## Original notes pass', () => {
    // The verbatim reporter block legitimately quotes placeholder tokens and `##`-shaped headings —
    // a ticket ABOUT unsubstituted placeholders does exactly this — and it must survive
    // byte-for-byte. An unscoped scan would make this very plan unpublishable.
    const description = conformant().replace(
      '## Original notes\n\nSubstantive body prose for this section, long enough to be a real plan.',
      [
        '## Original notes',
        '',
        '## Problem',
        '',
        'A literal <ATTACHMENT-ID> placeholder reaches the tracker.',
        '',
        '## Evidence',
        '',
        'The drafter emitted <ISSUE_ID> unsubstituted.',
      ].join('\n'),
    )
    const result = checkPlanContract({ description })
    assert.deepEqual(result.violations, [])
    assert.deepEqual(placeholderResidue(DEFAULT_CONFIG, description), [])
  })

  test('SELF-REFERENCE: a plan that documents the residue element names in prose and fences passes', () => {
    assert.equal(planFileResidue(documentingPlan), null)
    assert.deepEqual(
      checkPlanContract({ description: conformant(), plan: documentingPlan }).violations,
      [],
    )
  })

  test('a placeholder in an emitted heading is still caught, wherever it sits', () => {
    const description = conformant().replace('## Planning', '## <SECTION_NAME>\n\nx\n\n## Planning')
    const result = checkPlanContract({ description })
    assert.ok(codes(result).includes('placeholder-residue'))
  })

  test('SCOPING: a token inside a fenced block or an inline code span is documentation, not residue', () => {
    // A plan whose `## Key changes` shows the very shell block a skill runs is DOCUMENTING the
    // token, and `- Contract: v<N>` is this contract's own notation — written that way in the
    // skill body and in docs/skills/skill-config.md. Rejecting either makes a plan that describes
    // the plan contract unpublishable, and unlike `unknown-section` this rule has no config remedy.
    const fenced = conformant().replace(
      '## Key changes\n\nSubstantive body prose for this section, long enough to be a real plan.',
      '## Key changes\n\n```bash\nNEW=".linear-plans/<ISSUE-ID>.new.md"\n```',
    )
    assert.deepEqual(checkPlanContract({ description: fenced }).violations, [])

    const inline = conformant().replace(
      '## Approach\n\nSubstantive body prose for this section, long enough to be a real plan.',
      '## Approach\n\nEach description stamps `- Contract: v<N>` under `## Planning` on write.',
    )
    assert.deepEqual(checkPlanContract({ description: inline }).violations, [])
  })

  test('a BARE token in ordinary prose is still residue — the exclusions are scoping, not a hole', () => {
    const bare = conformant().replace(
      '## Approach\n\nSubstantive body prose for this section, long enough to be a real plan.',
      '## Approach\n\nWrite the plan for <ISSUE-ID> against the existing seam.',
    )
    assert.deepEqual(placeholderResidue(DEFAULT_CONFIG, bare), ['<ISSUE-ID>'])
    // An unbalanced backtick must not hide a token either: the span regex needs a closing run, so
    // the line is scanned rather than blanked. Fail closed.
    const unbalanced = conformant().replace(
      '## Approach\n\nSubstantive body prose for this section, long enough to be a real plan.',
      '## Approach\n\nA stray ` backtick then <ISSUE-ID> on the same line.',
    )
    assert.deepEqual(placeholderResidue(DEFAULT_CONFIG, unbalanced), ['<ISSUE-ID>'])
  })

  test('the Step 7 `## Planning` template slots are caught INSIDE their permanent backticks', () => {
    // These two lines are copied verbatim from the drafting brief's Step 7 template. Their
    // backticks are PERMANENT — they survive substitution (`Implementation plan (ABC-741)`) — so a
    // blanket inline-code exemption would cancel this check on the single line the ticket exists
    // to protect: an unsubstituted <ATTACHMENT-ID>/<ISSUE-ID> in the `## Planning` attachment line.
    // Assert on the template's own bytes, not a synthesised bare token, so the test tracks the
    // artifact the producer actually emits.
    const templated = conformant(
      [
        '- Contract: v1',
        '- Plan attachment: `Implementation plan (<ISSUE-ID>)`',
        '- On implementation: copy the plan to `docs/plans/<ISSUE-ID>-<slug>.md` and commit it.',
      ].join('\n'),
    )
    assert.deepEqual(placeholderResidue(DEFAULT_CONFIG, templated), ['<ISSUE-ID>'])
    assert.ok(codes(checkPlanContract({ description: templated })).includes('placeholder-residue'))
    // …and the substituted form publishes cleanly.
    assert.deepEqual(
      checkPlanContract({ description: templated.replaceAll('<ISSUE-ID>', 'ABC-741') }).violations,
      [],
    )
  })

  test('a one-segment metavariable in a code span stays exempt — that is what the exemption is for', () => {
    // `- Contract: v<N>` is this contract's own notation. `N` is not in TEMPLATE_SLOT_NAMES, so
    // inside a code span it stays exempt and must not be swept in alongside <ISSUE-ID> above.
    // The rule is the NAMED vocabulary, never token shape: `<PARENT>` is a slot with no separator
    // at all, so re-deriving this as a shape test would both miss it and re-reject documentation.
    const inline = conformant().replace(
      '## Approach\n\nSubstantive body prose for this section, long enough to be a real plan.',
      '## Approach\n\nEach description stamps `- Contract: v<N>` under `## Planning` on write.',
    )
    assert.deepEqual(placeholderResidue(DEFAULT_CONFIG, inline), [])
  })

  test('a `## ` line inside a fenced block is quoted output, not an off-contract section', () => {
    // Plans routinely fence command output and markdown examples whose lines begin with `##` — a
    // `make help` listing, a `git status` branch line. Reading one as emitted structure invented an
    // `unknown-section` (and could trip `section-order`), which is a deterministic hard abort whose
    // printed remedy — "register it in planContract.sections" — is wrong and unactionable here.
    for (const quoted of [
      '## test: Run tests with race detector and coverage',
      '## <branch>...origin/<branch>',
    ]) {
      const description = conformant().replace(
        '## Key changes\n\nSubstantive body prose for this section, long enough to be a real plan.',
        `## Key changes\n\n\`\`\`\n${quoted}\n\`\`\``,
      )
      assert.deepEqual(
        checkPlanContract({ description }).violations,
        [],
        `a fenced "${quoted}" must not be read as an emitted section`,
      )
    }
  })

  test('a multi-segment token quoted as documentation is not a template slot', () => {
    // Shape cannot separate a slot from documentation: `<PLAN_PATH>` and `<APPROVAL_POLICY>` are
    // identical in shape. Scanning by shape inside code spans rejected roughly one plan in twenty
    // of this repo's own corpus — permanently, since this rule has no config escape hatch. Only the
    // producer's own NAMED slot vocabulary is scanned inside a span.
    for (const quoted of [
      '`-a, --ask-for-approval <APPROVAL_POLICY>`',
      '`<CODEX_HOME>/config.toml`',
      '`<LENS_SKILL>`',
    ]) {
      const description = conformant().replace(
        '## Key changes\n\nSubstantive body prose for this section, long enough to be a real plan.',
        `## Key changes\n\nThe flag is spelled ${quoted} in its usage string.`,
      )
      assert.deepEqual(
        placeholderResidue(DEFAULT_CONFIG, description),
        [],
        `${quoted} is documentation, not an unsubstituted slot`,
      )
    }
    // …but the SAME token unquoted is still residue: outside a span, any placeholder shape counts.
    const bare = conformant().replace(
      '## Key changes\n\nSubstantive body prose for this section, long enough to be a real plan.',
      '## Key changes\n\nThe <APPROVAL_POLICY> flag, unquoted.',
    )
    assert.deepEqual(placeholderResidue(DEFAULT_CONFIG, bare), ['<APPROVAL_POLICY>'])
    // …and a named slot inside a span still fires wherever it sits.
    const slot = conformant('- Contract: v1\n- Dependencies: blocks `<BLOCKED-ID>`')
    assert.deepEqual(placeholderResidue(DEFAULT_CONFIG, slot), ['<BLOCKED-ID>'])
  })

  test('an UNTERMINATED fence cannot blind the placeholder scan either', () => {
    // The same hole `planFileResidue` closes one function below: an unclosed fence hides every line
    // after it, and a drafter forgetting a closing fence is at least as likely as a truncated
    // transcript. Scanning the raw lines there is the fail-closed direction.
    const unclosed = conformant().replace(
      '## Key changes\n\nSubstantive body prose for this section, long enough to be a real plan.',
      '## Key changes\n\n```bash\nX=1\n\nand then a bare <ISSUE-ID> token in prose',
    )
    assert.deepEqual(placeholderResidue(DEFAULT_CONFIG, unclosed), ['<ISSUE-ID>'])
  })

  test('a fence nested in a list item is still a fence', () => {
    // A fence under a bullet is indented by the marker width, past CommonMark's 3-space limit for a
    // TOP-LEVEL fence. Quoted output under a bullet is the shape plans use most, so the strict
    // limit let its sample lines read as document structure and hard-aborted the run.
    const description = conformant().replace(
      '## Key changes\n\nSubstantive body prose for this section, long enough to be a real plan.',
      '## Key changes\n\n- The help output is:\n\n    ```\n    ## test: Run tests with coverage\n    ```\n',
    )
    assert.deepEqual(checkPlanContract({ description }).violations, [])
  })

  test('an unclosed fence is reported as itself, not as the sections it hides', () => {
    // One missing backtick line made every later heading invisible to the splitter, which reported
    // it as six missing sections — sending an unattended drafter off to re-add sections it had
    // already written. The structural checks are skipped because they read the broken split.
    const description = conformant().replace(
      '## Key changes\n\nSubstantive body prose for this section, long enough to be a real plan.',
      '## Key changes\n\n```bash\nX=1',
    )
    const result = checkPlanContract({ description })
    assert.deepEqual(codes(result), ['not-a-description'])
    assert.match(result.violations[0].message, /never closed/)
    assert.ok(!codes(result).includes('missing-sections'), 'the misdiagnosis must not survive')
    // The placeholder scan still runs — it has its own raw-line fallback for this exact input.
    const withToken = description.replace('```bash\nX=1', '```bash\nX=1\n\nbare <ISSUE-ID> here')
    assert.ok(codes(checkPlanContract({ description: withToken })).includes('placeholder-residue'))
    // A genuinely missing section is still reported when the fences are balanced.
    const missing = conformant().replace(
      '## Testing\n\nSubstantive body prose for this section, long enough to be a real plan.\n\n',
      '',
    )
    assert.ok(codes(checkPlanContract({ description: missing })).includes('missing-sections'))
  })

  test('plan-file-residue survives an UNTERMINATED fence hiding the trailing tag', () => {
    // A plan truncated out of a transcript mid-code-block, with scaffolding appended, is precisely
    // the shape this check exists for — and it is the one shape a fence tracker makes invisible.
    const truncated = '# Plan\n\nReal body.\n\n```\nstill inside the block\n\n</invoke>\n'
    assert.equal(hasUnterminatedFence(truncated), true)
    assert.match(planFileResidue(truncated), /scaffolding/)
    assert.ok(
      codes(checkPlanContract({ description: conformant(), plan: truncated })).includes(
        'plan-file-residue',
      ),
    )
    // …while a plan that legitimately ENDS inside a closed fence still passes: it ends on the
    // fence's own closing run, never on a bare tag.
    assert.equal(hasUnterminatedFence(documentingPlan), false)
    assert.equal(planFileResidue('# Plan\n\nbody\n\n```xml\n</invoke>\n```\n'), null)
  })
})

describe('CLI', () => {
  // The CLI — not `checkPlanContract` — is what Phase 4 and the drafting brief actually invoke, so
  // its exit code and stderr shape are the contract agents see. Exercise the process itself.
  const runCli = (descriptionMd, planMd = null, override = null, options = {}) => {
    const { modeArgs = [], ...spawnOptions } = options
    const dir = mkdtempSync(path.join(tmpdir(), 'plan-contract-guard-'))
    const description = path.join(dir, 'description.md')
    writeFileSync(description, descriptionMd)
    const args = [GUARD, '--description', override ?? description]
    if (planMd !== null) {
      const plan = path.join(dir, 'plan.md')
      writeFileSync(plan, planMd)
      args.push('--plan', plan)
    }
    args.push(...modeArgs)
    return spawnSync(process.execPath, args, { encoding: 'utf8', ...spawnOptions })
  }

  test('a conformant description exits 0 and says nothing', () => {
    const res = runCli(conformant(), documentingPlan)
    assert.equal(res.status, 0, `expected a clean exit, got ${res.status}: ${res.stderr}`)
    assert.equal(res.stderr.trim(), '')
  })

  test('the CLI exemption flag suppresses only the plan-file structure floor', () => {
    const flattened = conformant()
    const blocked = runCli(conformant(), flattened)
    assert.notEqual(blocked.status, 0)
    assert.match(blocked.stderr, /\[plan-file-structure\]/)

    const exempt = runCli(conformant(), flattened, null, {
      modeArgs: ['--plan-file-exemption', 'consumer'],
    })
    assert.equal(exempt.status, 0, `expected consumer exemption to pass: ${exempt.stderr}`)

    const residue = runCli(conformant(), `${flattened}\n</invoke>\n`, null, {
      modeArgs: ['--plan-file-exemption', 'consumer'],
    })
    assert.notEqual(residue.status, 0)
    assert.match(residue.stderr, /\[plan-file-residue\]/)
  })

  test('violations exit non-zero with one tagged stderr line each', () => {
    const res = runCli('the full markdown plan description as specified in Step 7 of the brief')
    assert.notEqual(res.status, 0, 'a violating description must exit non-zero')
    const tagged = res.stderr.split('\n').filter((l) => /\[[a-z-]+\]$/.test(l.trim()))
    assert.ok(tagged.length >= 1, `expected tagged violation lines, got: ${res.stderr}`)
    assert.ok(tagged.every((l) => /\[(not-a-description|missing-sections)\]$/.test(l.trim())))
    assert.match(res.stderr, /contract violation\(s\) — do not write/)
  })

  test('an unreadable description is a violation tagged [unreadable-input], never a pass', () => {
    const res = runCli(conformant(), null, path.join(tmpdir(), 'plan-contract-guard-absent.md'))
    assert.notEqual(res.status, 0, 'an unreadable input must never exit 0')
    assert.match(res.stderr, /\[unreadable-input\]/)
  })

  test('could-not-evaluate emits a tagged CLI line and exits non-zero', () => {
    const res = runCli(conformant(), null, null, {
      env: { ...process.env, PLAN_CONTRACT_GUARD_CWD: '' },
    })
    assert.notEqual(res.status, 0, 'an unknown citation check must not exit 0')
    assert.match(res.stderr, /\[citation-could-not-evaluate\]/)
    assert.match(res.stderr, /could not be evaluated/)
  })

  test('accepts --mode epic-parent and defaults to child-plan', () => {
    const epic = epicParentConformant()
    const accepted = runCli(epic, null, null, { modeArgs: ['--mode', 'epic-parent'] })
    assert.equal(accepted.status, 0, `expected epic-parent mode to pass: ${accepted.stderr}`)

    const rejected = runCli(epic)
    assert.notEqual(rejected.status, 0, 'default child-plan mode must reject an epic overview')
    assert.match(rejected.stderr, /\[missing-sections\]/)
    assert.match(rejected.stderr, /\[unknown-section\]/)
  })

  test('rejects an unknown --mode and a missing --mode value', () => {
    const unknown = runCli(conformant(), null, null, { modeArgs: ['--mode', 'future-parent'] })
    assert.notEqual(unknown.status, 0)
    assert.match(
      unknown.stderr,
      /--mode must be one of child-plan, epic-parent; got "future-parent"/,
    )

    const missing = spawnSync(
      process.execPath,
      [GUARD, '--description', GUARD, '--mode', '--plan', GUARD],
      { encoding: 'utf8' },
    )
    assert.notEqual(missing.status, 0)
    assert.match(missing.stderr, /--mode <value> is required/)
  })
})

describe('checkPlanContract — argument order', () => {
  test('a swapped config argument throws the validator’s named argument-order error', () => {
    assert.throws(
      () => checkPlanContract({ description: conformant(), config: conformant() }),
      /validatePlanDescription\(config, description\) — arguments look swapped/,
    )
  })

  test('a non-string description throws a named error rather than a raw TypeError', () => {
    assert.throws(
      () => checkPlanContract({ description: DEFAULT_CONFIG, config: conformant() }),
      /arguments look swapped/,
    )
  })
})

describe('exported helpers', () => {
  test('linesOutsideFences skips fenced content and reports 0-based indices', () => {
    const lines = linesOutsideFences('a\n```\nhidden\n```\nb\n')
    assert.deepEqual(
      lines.map((l) => l.line),
      ['a', 'b', ''],
    )
    assert.equal(lines[1].index, 4)
  })

  test('emittedContractHeadings excludes off-contract headings', () => {
    const description = conformant().replace('## Planning', '## Notes\n\nx\n\n## Planning')
    assert.ok(!emittedContractHeadings(DEFAULT_CONFIG, description).includes('## Notes'))
  })

  test('isContractOrdered accepts a subsequence and rejects a transposition', () => {
    assert.equal(isContractOrdered(DEFAULT_CONFIG, ['## Summary', '## Planning']), true)
    assert.equal(isContractOrdered(DEFAULT_CONFIG, ['## Planning', '## Summary']), false)
  })

  test('parseContractGuardArgs requires --description and accepts --plan', () => {
    assert.deepEqual(parseContractGuardArgs(['--description', 'd.md', '--plan', 'p.md']), {
      description: 'd.md',
      plan: 'p.md',
      mode: 'child-plan',
      planFileExemption: null,
    })
    assert.throws(() => parseContractGuardArgs([]), /--description <path> is required/)
  })

  test('parseContractGuardArgs accepts only known description modes', () => {
    assert.deepEqual(parseContractGuardArgs(['--description', 'd.md', '--mode', 'epic-parent']), {
      description: 'd.md',
      plan: null,
      mode: 'epic-parent',
      planFileExemption: null,
    })
    assert.throws(
      () => parseContractGuardArgs(['--description', 'd.md', '--mode', 'future-parent']),
      /--mode must be one of child-plan, epic-parent; got "future-parent"/,
    )
    assert.throws(
      () => parseContractGuardArgs(['--description', 'd.md', '--mode', '--plan', 'p.md']),
      /--mode <value> is required/,
    )
  })

  test('parseContractGuardArgs accepts a plan-file exemption reason', () => {
    assert.deepEqual(
      parseContractGuardArgs([
        '--description',
        'd.md',
        '--plan',
        'p.md',
        '--plan-file-exemption',
        'adopted-child-redraft',
      ]),
      {
        description: 'd.md',
        plan: 'p.md',
        mode: 'child-plan',
        planFileExemption: 'adopted-child-redraft',
      },
    )
  })

  test('parseContractGuardArgs rejects unknown arguments', () => {
    assert.throws(
      () => parseContractGuardArgs(['--description', 'd.md', '--plan-file-exempt', 'consumer']),
      /unknown argument: --plan-file-exempt/,
    )
    assert.throws(
      () => parseContractGuardArgs(['--description', 'd.md', 'p.md']),
      /unknown argument: p[.]md/,
    )
  })
})

// ---------------------------------------------------------------------------
// BOS-1199 U5 — the normalizer-hostile line-spanning emphasis lint.
//
// A tracker's markdown normalizer can close an emphasis span at a hard line break and store the
// remainder as a literal delimiter run, silently damaging the only surviving copy of the
// description. The lint is deliberately narrow: the verbatim block, fenced blocks and inline code
// spans are excluded, and the suite is weighted toward over-detection guards because a sibling
// ticket owns the broader false-rejection problem.
// ---------------------------------------------------------------------------

describe('line-spanning emphasis lint (BOS-1199)', () => {
  // Splice a body into the ## Summary section of a conformant description.
  const withSummary = (body) =>
    conformant().replace(
      '## Summary\n\nSubstantive body prose for this section, long enough to be a real plan.',
      `## Summary\n\n${body}`,
    )

  const spans = (description) => lineSpanningEmphasis(DEFAULT_CONFIG, description)

  test('a span opened and closed on the same line passes', () => {
    assert.deepEqual(spans(withSummary('This is **one bold span** on a single line.')), [])
  })

  test('a span opened on one line and closed on the next is reported with line and column', () => {
    const description = withSummary('found **7 of the 17\nalready fixed** and 10 still live.')
    const found = spans(description)
    assert.equal(found.length, 1)
    // The Summary body starts on line 3 (heading, blank, body), and the run opens after `found `.
    assert.equal(found[0].line, 3)
    assert.equal(found[0].column, 7)
    assert.ok(
      codes(checkPlanContract({ description, config: DEFAULT_CONFIG })).includes(
        'line-spanning-emphasis',
      ),
      'the lint must surface as a contract violation',
    )
  })

  test('the same line-spanning span inside the verbatim block passes (exemption is load-bearing)', () => {
    const description = conformant().replace(
      '## Original notes\n\nSubstantive body prose for this section, long enough to be a real plan.',
      '## Original notes\n\nfound **7 of the 17\nalready fixed** and 10 still live.',
    )
    assert.deepEqual(spans(description), [], 'the copied block is not ours to rewrap')
    assert.ok(
      !codes(checkPlanContract({ description, config: DEFAULT_CONFIG })).includes(
        'line-spanning-emphasis',
      ),
    )
  })

  test('the same line-spanning span inside a fenced code block passes', () => {
    assert.deepEqual(
      spans(withSummary('```md\nfound **7 of the 17\nalready fixed** here.\n```')),
      [],
    )
  })

  test('a line-spanning span inside an inline code span passes', () => {
    assert.deepEqual(
      spans(withSummary('The literal `**7 of the 17` and\n`already fixed**` tokens.')),
      [],
    )
  })

  test('an odd number of delimiter runs does not crash and reports no unpairable span', () => {
    assert.deepEqual(spans(withSummary('This **is bold** and this **is not closed\nanywhere.')), [])
  })

  test('multiple offending spans are each reported, not only the first', () => {
    const found = spans(
      withSummary('first **span opens\nand closes** here, then **another opens\nand closes** too.'),
    )
    assert.equal(found.length, 2)
    assert.deepEqual(
      found.map((f) => f.line),
      [3, 4],
    )
  })

  test('nested and adjacent same-line spans pass (over-detection guard)', () => {
    assert.deepEqual(spans(withSummary('**alpha** and **beta** and *gamma* all on one line.')), [])
    assert.deepEqual(
      spans(withSummary('An ***emphatic*** phrase plus **bold *inner* bold** text.')),
      [],
    )
  })

  test('arithmetic prose across a hard wrap does not pair into a phantom span', () => {
    // Flanking, not shape, is what keeps this out: neither `*` is adjacent to a non-space.
    assert.deepEqual(
      spans(withSummary('The budget is 2 * 3 units\nand the ceiling is 4 * 5 units.')),
      [],
    )
  })

  test('a leading list marker is not an emphasis delimiter', () => {
    assert.deepEqual(spans(withSummary('* first item with **bold** text\n* second item here.')), [])
  })

  test('a conformant description with no offending span produces no violation', () => {
    assert.deepEqual(spans(conformant()), [])
    assert.ok(
      !codes(checkPlanContract({ description: conformant(), config: DEFAULT_CONFIG })).includes(
        'line-spanning-emphasis',
      ),
    )
  })
})
