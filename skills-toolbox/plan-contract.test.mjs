// Compatibility gate for the versioned plan description-section contract (BOS-204).
// Fails if the machine-readable planContract, the boss-plan producer template/docs, or the
// consumer skills (boss-build, bs-sweep-plan) drift apart. Node builtins only.
import { describe, test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import {
  DEFAULT_CONFIG,
  planSections,
  planContractVersion,
  requiredPlanSections,
  validatePlanDescription,
  validateVerifyOnlyEvidence,
  VERIFY_ONLY_MARKER,
  VERIFY_ONLY_CHECK,
  VERIFY_ONLY_CHECKED,
  VERIFY_ONLY_RESULT,
} from './skill-config.mjs'

// Skill bodies live one level up from skills-toolbox/.
const read = (p) => readFileSync(new URL(`../${p}`, import.meta.url), 'utf8')
// boss-plan and boss-build are published cores: their canonical committed home
// is the skillinstall payload (BOS-271), no longer .claude/skills. bs-sweep-plan
// stays a repo-local .claude skill.
const CORE = 'services/boss/internal/skillinstall/skills'
const PLAN_SKILL = read(`${CORE}/boss-plan/SKILL.md`)
const BRIEF = read(`${CORE}/boss-plan/references/headless-drafting-brief.md`)
const SWEEP = read('.claude/skills/bs-sweep-plan/SKILL.md')
const IMPLEMENT = read(`${CORE}/boss-build/SKILL.md`)
const FINALIZE = read(`${CORE}/boss-build/references/finalize-and-stop.md`)
// The whole boss-build tree, so a literal may live in the resident body or in any reference —
// the contract is that the consumer names the bytes, not which file it names them in.
const IMPLEMENT_TREE = `${IMPLEMENT}\n${FINALIZE}`

const HEADINGS = planSections(DEFAULT_CONFIG).map((s) => s.heading)

/** A parsed criterion whose named check command carries actual content. */
const nonEmptyCheck = (criterion) =>
  typeof criterion.check === 'string' && criterion.check.trim().length > 0

describe('plan-contract sync', () => {
  test('every contract heading is documented in the boss-plan resident body', () => {
    for (const h of HEADINGS) {
      assert.ok(PLAN_SKILL.includes(h), `boss-plan SKILL.md must document contract section ${h}`)
    }
  })

  test('the drafting brief Step 7 template carries every contract heading', () => {
    for (const h of HEADINGS) {
      assert.ok(BRIEF.includes(h), `drafting brief must emit contract section ${h}`)
    }
  })

  test('the brief stamps a Contract version equal to the config version', () => {
    const m = /^-\s*Contract:\s*v(\d+)\s*$/m.exec(BRIEF)
    assert.ok(m, 'brief Step 7 template must carry a `- Contract: vN` stamp')
    assert.equal(Number(m[1]), planContractVersion(DEFAULT_CONFIG))
  })

  test('consumer skills reference the sections they parse', () => {
    // boss-build seeds its PR-body checklist from `## Acceptance criteria` and validates the
    // `- Contract:` stamp against this contract; bs-sweep-plan relies on `## Original notes`
    // preservation. The producer (boss-plan) documents the FULL section set — asserted above — so a
    // renamed/removed heading still fails the gate even when a consumer only parses a subset.
    assert.ok(
      IMPLEMENT.includes('## Acceptance criteria'),
      'boss-build must reference consumed section ## Acceptance criteria',
    )
    assert.ok(IMPLEMENT.includes('- Contract:'), 'boss-build must validate the contract stamp')
    assert.ok(SWEEP.includes('## Original notes'), 'bs-sweep-plan must reference ## Original notes')
  })

  test('boss-build consumes the plan `## Proof harness analysis` for in-PR affordances (BOS-111)', () => {
    // boss-plan writes a `## Proof harness analysis` gap list at plan time; boss-build Step 5
    // reads it as the source of the affordances to build in-PR. The cross-reference closes the loop.
    assert.ok(
      IMPLEMENT.includes('## Proof harness analysis'),
      'boss-build must reference the plan `## Proof harness analysis` section',
    )
  })

  test('`## Proof harness analysis` is registered `optional` — recognised, never required (contract stays v1)', () => {
    // BOS-111 kept the section advisory. BOS-741 registers it as `optional` so the producer-side
    // unknown-heading check can be strict about the template boss-plan itself prescribes, WITHOUT
    // newly requiring the section of the hundreds of tickets already stamped v1. Both halves matter:
    // present in `sections`, absent from `requiredPlanSections`, contract still v1.
    assert.equal(planContractVersion(DEFAULT_CONFIG), 1, 'plan contract must remain v1')
    const entry = planSections(DEFAULT_CONFIG).find(
      (s) => s.heading === '## Proof harness analysis',
    )
    assert.ok(entry, '`## Proof harness analysis` must be a recognised contract section')
    assert.equal(
      entry.required,
      'optional',
      '`## Proof harness analysis` must be classed optional, never required',
    )
    assert.ok(
      !requiredPlanSections(DEFAULT_CONFIG).includes('## Proof harness analysis'),
      '`## Proof harness analysis` must NOT be a required heading',
    )
  })

  test('the verify-only marker/clause literals are byte-identical across producer and consumer', () => {
    // The marker is a LITERAL token, so classification needs zero heuristics — but only while the
    // producer prose, the consumer prose and the parser agree byte-for-byte. A hand-typed copy in
    // three places is exactly how that agreement rots, so `skill-config.mjs` owns the definition
    // and this test pins both prose sides to it. Removing the literal from the drafting brief must
    // turn this red; that direction was verified by running this test with the literal removed.
    assert.ok(
      BRIEF.includes(VERIFY_ONLY_MARKER),
      `drafting brief must instruct the drafter to write the literal ${VERIFY_ONLY_MARKER} marker`,
    )
    // Assert the UNTRIMMED literals — the exact bytes the parser matches. `.trim()` here was
    // vacuous in the precise way this ticket is about: it drops the surrounding spaces, which are
    // load-bearing, so deleting one space before the em dash left this "byte-identical" test GREEN
    // while `parseAcceptanceCriteria` stopped finding the clause at all and every correctly
    // discharged criterion started failing the gate. A sync test that trims the bytes it exists to
    // pin is not a sync test.
    assert.ok(
      BRIEF.includes(VERIFY_ONLY_CHECK),
      `drafting brief must carry the plan-time \`${VERIFY_ONLY_CHECK}\` clause verbatim`,
    )
    assert.ok(
      IMPLEMENT_TREE.includes(VERIFY_ONLY_MARKER),
      `boss-build must reference the literal ${VERIFY_ONLY_MARKER} marker it classifies on`,
    )
    for (const literal of [VERIFY_ONLY_CHECKED, VERIFY_ONLY_RESULT]) {
      assert.ok(
        IMPLEMENT_TREE.includes(literal),
        `boss-build must carry the discharge literal \`${literal}\` verbatim`,
      )
    }
    // The gate itself must be named where it is executed, not merely described. The symbol gate
    // (scripts/check-skill-symbols.mjs) separately resolves this citation against the real export.
    assert.ok(
      FINALIZE.includes('validateVerifyOnlyEvidence'),
      'boss-build Step 9 must run validateVerifyOnlyEvidence over the PR body before readying',
    )
  })

  test('the brief`s own Step 7 verify-only row round-trips through the evidence gate', () => {
    // End-to-end producer→parser sync: the row the drafting brief tells planners to emit must
    // parse as a verify-only criterion. If the brief's bytes drift from the parser's, this fails
    // even when both substring assertions above still pass.
    const row = BRIEF.split('\n').find(
      (line) => line.startsWith('- [ ] ') && line.includes(VERIFY_ONLY_MARKER),
    )
    assert.ok(row, 'the Step 7 template must show a verify-only acceptance-criteria row')
    const body = `## Acceptance criteria\n\n${row}\n\n## Original notes\n\nx`
    const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
    assert.equal(result.verifyOnly.length, 1, 'the brief`s row must classify as verify-only')
    assert.ok(nonEmptyCheck(result.verifyOnly[0]), 'the brief`s row must name a check command')
    // Unticked, so it is reported but never a gate failure — the open-criterion rule owns that.
    assert.equal(result.ok, true)
    // The addition is additive INSIDE an existing required section: no section added, renamed or
    // removed, so every already-planned v1 ticket keeps validating and keeps building.
    assert.equal(planContractVersion(DEFAULT_CONFIG), 1, 'plan contract must remain v1')
  })

  test('boss-build`s own discharge template row round-trips through the evidence gate', () => {
    // The CONSUMER side of the same sync, and the row that actually ships: a run copies this
    // template into the PR body verbatim. The substring assertions above prove the literals are
    // present somewhere in the tree; only parsing the real row proves the row a builder copies
    // still satisfies the gate that will judge it. A drift here fails every run, not just this test.
    const row = IMPLEMENT.split('\n').find(
      (line) => line.startsWith('- [x] ') && line.includes(VERIFY_ONLY_MARKER),
    )
    assert.ok(row, 'the Step 7 PR-body template must show a discharged verify-only row')
    const body = `## Acceptance criteria\n\n${row}\n\n## Original notes\n\nx`
    const result = validateVerifyOnlyEvidence(DEFAULT_CONFIG, body)
    assert.equal(result.verifyOnly.length, 1, 'the template row must classify as verify-only')
    assert.equal(
      result.ok,
      true,
      'the ticked template row must satisfy the gate it will be judged by',
    )
    assert.ok(nonEmptyCheck(result.verifyOnly[0]), 'the template row must name a command')
    assert.ok(
      typeof result.verifyOnly[0].result === 'string' && result.verifyOnly[0].result.trim() !== '',
      'the template row must name a result',
    )
  })

  test('a template rendered from the required sections validates clean', () => {
    const rendered = planSections(DEFAULT_CONFIG)
      .filter((s) => s.required === 'always')
      .map((s) => `${s.heading}\n\nbody`)
      .join('\n\n')
      .replace('## Planning\n\nbody', '## Planning\n\n- Contract: v1')
    const result = validatePlanDescription(DEFAULT_CONFIG, rendered)
    assert.equal(result.ok, true)
    // The rendered required-section set must also be free of off-contract headings, otherwise the
    // producer-side guard would reject the very template this contract emits.
    assert.deepEqual(result.unknown, [])
  })
})
