// Compatibility gate for the versioned plan description-section contract (BOS-204).
// Fails if the machine-readable planContract, the boss-plan producer template/docs, or the
// consumer skills (boss-implement, bs-sweep-plan) drift apart. Node builtins only.
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
// boss-plan and boss-implement are published cores: their canonical committed home
// is the skillinstall payload (BOS-271), no longer .claude/skills. bs-sweep-plan
// stays a repo-local .claude skill.
const CORE = 'services/boss/internal/skillinstall/skills'
const PLAN_SKILL = read(`${CORE}/boss-plan/SKILL.md`)
const BRIEF = read(`${CORE}/boss-plan/references/headless-drafting-brief.md`)
const SWEEP = read('.claude/skills/bs-sweep-plan/SKILL.md')
const IMPLEMENT = read(`${CORE}/boss-implement/SKILL.md`)

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
    // boss-implement seeds its PR-body checklist from `## Acceptance criteria` and validates the
    // `- Contract:` stamp against this contract; bs-sweep-plan relies on `## Original notes`
    // preservation. The producer (boss-plan) documents the FULL section set — asserted above — so a
    // renamed/removed heading still fails the gate even when a consumer only parses a subset.
    assert.ok(
      IMPLEMENT.includes('## Acceptance criteria'),
      'boss-implement must reference consumed section ## Acceptance criteria',
    )
    assert.ok(IMPLEMENT.includes('- Contract:'), 'boss-implement must validate the contract stamp')
    assert.ok(SWEEP.includes('## Original notes'), 'bs-sweep-plan must reference ## Original notes')
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
