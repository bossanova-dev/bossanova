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
  validatePlanDescription,
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

const HEADINGS = planSections(DEFAULT_CONFIG).map((s) => s.heading)

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

  test('`## Proof harness analysis` is advisory — not a required contract section (contract stays v1)', () => {
    // BOS-111 deliberately keeps the section out of planContract.sections so the contract stays v1
    // and every pre-existing v1 plan + boss-build Step-4 validation keeps passing.
    assert.equal(planContractVersion(DEFAULT_CONFIG), 1, 'plan contract must remain v1')
    assert.ok(
      !HEADINGS.includes('## Proof harness analysis'),
      '`## Proof harness analysis` must NOT be a required contract heading (advisory only)',
    )
  })

  test('a template rendered from the required sections validates clean', () => {
    const rendered = planSections(DEFAULT_CONFIG)
      .filter((s) => s.required === 'always')
      .map((s) => `${s.heading}\n\nbody`)
      .join('\n\n')
      .replace('## Planning\n\nbody', '## Planning\n\n- Contract: v1')
    assert.equal(validatePlanDescription(DEFAULT_CONFIG, rendered).ok, true)
  })
})
