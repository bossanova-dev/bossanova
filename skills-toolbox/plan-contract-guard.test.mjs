// Coverage for the producer-side plan-contract guard (BOS-741). Every violation code must fire on
// a minimal fixture, and every documented non-violation must pass — in particular the two scoping
// guarantees (`## Original notes` is not scanned for placeholders; a plan that merely DOCUMENTS the
// scaffolding element names is not residue), which are the regressions most likely to be
// "simplified" away later. Node builtins only.
import { describe, test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  checkPlanCitations,
  checkPlanContract,
  checkPrBodyOnlyEvidence,
  checkSelfFalsifiedLiteralSearch,
  emittedContractHeadings,
  hasUnterminatedFence,
  isContractOrdered,
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

// A plan file that merely documents the guard: it names the scaffolding elements in prose and
// quotes one inside a fenced block, and ends on ordinary prose.
const documentingPlan = `# Plan

The guard rejects a plan whose last line is a bare closing invoke or function_calls tag.

\`\`\`xml
</invoke>
</function_calls>
\`\`\`

That is why the check is anchored to the last non-blank line outside fenced code.
`

describe('checkPlanContract — conformant input', () => {
  test('a fully conformant description passes, with --plan supplied', () => {
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

  test('unresolvable-citation fires only for premises and acceptance criteria citations', () => {
    const dir = mkdtempSync(path.join(tmpdir(), 'plan-contract-citation-'))
    writeFileSync(path.join(dir, 'present.md'), 'one\ntwo\n')
    const description = conformant()
      .replace(
        '## Acceptance criteria\n\nSubstantive body prose for this section, long enough to be a real plan.',
        [
          '## Premises',
          '',
          '- [ ] present.md:2 resolves',
          '- [ ] present.md:5 does not',
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
    const dir = mkdtempSync(path.join(tmpdir(), 'plan-contract-guard-'))
    const description = path.join(dir, 'description.md')
    writeFileSync(description, descriptionMd)
    const args = [GUARD, '--description', override ?? description]
    if (planMd !== null) {
      const plan = path.join(dir, 'plan.md')
      writeFileSync(plan, planMd)
      args.push('--plan', plan)
    }
    return spawnSync(process.execPath, args, { encoding: 'utf8', ...options })
  }

  test('a conformant description exits 0 and says nothing', () => {
    const res = runCli(conformant(), documentingPlan)
    assert.equal(res.status, 0, `expected a clean exit, got ${res.status}: ${res.stderr}`)
    assert.equal(res.stderr.trim(), '')
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
    })
    assert.throws(() => parseContractGuardArgs([]), /--description <path> is required/)
  })
})
