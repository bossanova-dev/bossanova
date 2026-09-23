import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const SKILL = read('../.claude/skills/bs-sweep-plan/SKILL.md')
const CODEX = read('../.codex/skills/bs-sweep-plan/SKILL.md')

function frontmatterValue(skill, key) {
  const match = skill.match(new RegExp(`^${key}:\\s*(.+)$`, 'm'))
  assert.ok(match, `missing frontmatter key ${key}`)
  return match[1]
}

function phaseSection(skill, heading) {
  const section = skill.split(/\n## /).find((part) => part.startsWith(heading))
  assert.ok(section, `missing section ${heading}`)
  return section
}

for (const [label, skill] of [
  ['source', SKILL],
  ['codex mirror', CODEX],
]) {
  test(`${label}: frontmatter stays non-model-invocable and describes the pre-delegate gate`, () => {
    assert.equal(frontmatterValue(skill, 'disable-model-invocation'), 'true')
    assert.doesNotMatch(
      frontmatterValue(skill, 'description'),
      /then\s+removes\s+the\s+`agent-plan`\s+label\s+so\s+it\s+is\s+not\s+re-swept/,
      'description must not advertise the old Phase 4-only removal ordering',
    )
    assert.match(
      frontmatterValue(skill, 'description'),
      /skips\s+tickets\s+already\s+carrying\s+an\s+implementation\s+plan/i,
      'description should advertise the planned-ticket skip gate',
    )
  })

  test(`${label}: Phase 3 re-reads before delegating and has proceed skip blocked outcomes`, () => {
    const phase3 = phaseSection(skill, 'Phase 3')
    assert.match(phase3, /Before\s+delegating[\s\S]{0,160}`get_issue`/i)
    assert.match(phase3, /proceed/i)
    assert.match(phase3, /skip\s+\(already\s+planned\)/i)
    assert.match(phase3, /BLOCKED\s+\(could\s+not\s+read\)/i)
    assert.match(phase3, /still\s+in\s+the\s+config-resolved\s+unplanned\s+state/i)
    assert.match(phase3, /still\s+carries\s+`agent-plan`/i)
    assert.match(phase3, /attachment\s+by\s+itself\s+is\s+not\s+enough\s+to\s+skip/i)
    assert.match(
      phase3,
      /SAFE\s+branch\s+before\s+metadata\/state\s+writeback[\s\S]{0,120}retryable/i,
    )
    assert.match(
      phase3,
      /both\s+an\s+exact-title\s+plan\s+attachment\s+and\s+finalized\s+planning\s+signals/i,
    )
  })

  test(`${label}: already-planned skip removes the queue label and re-selects`, () => {
    const phase3 = phaseSection(skill, 'Phase 3')
    assert.match(phase3, /skip\s+\(already\s+planned\)[\s\S]{0,360}remove\s+`agent-plan`/i)
    assert.match(phase3, /re-run\s+Phase\s+2\s+selection/i)
  })

  // A peer mid-drafting the same ticket has made zero tracker mutations, so the `get_issue`
  // re-read beside this one correctly answers `proceed` while the duplicate dispatch is already
  // running. Pin the helper the check calls and the fourth outcome's ROUTING — the label must
  // survive a deferral, which is the one thing that separates it from the already-planned skip.
  test(`${label}: Phase 3 defers to a live peer claim without dropping the queue label`, () => {
    const phase3 = phaseSection(skill, 'Phase 3')
    assert.match(phase3, /plan-peer-claim\.mjs/, 'Phase 3 must name the peer-claim detector')
    assert.match(
      phase3,
      /defer\s+\(peer\s+claim\)[\s\S]{0,460}leave\s+`agent-plan`\s+in\s+place/i,
      'the deferral outcome must leave the queue label in place so the ticket is re-swept',
    )
  })

  test(`${label}: Phase 5 has the skipped terminal outcome`, () => {
    const phase5 = phaseSection(skill, 'Phase 5')
    assert.match(phase5, /`skipped\s+<ISSUE-ID>:\s+already\s+planned`/)
  })

  test(`${label}: Phase 5 can report a peer deferral as a terminal outcome`, () => {
    const phase5 = phaseSection(skill, 'Phase 5')
    assert.match(phase5, /`deferred\s+<ISSUE-ID>:\s+peer\s+run\s+in\s+flight`/)
  })

  // Phase 2's all-unprioritized branch used to hand the run an unnamed "most impactful"
  // judgement, so two runs over the same queue could select differently and neither was wrong.
  // Pin the RULE NAME and the ranking it encodes (not the prose that explains it), plus the
  // absence of the improvisation marker the rule replaced.
  test(`${label}: the all-unprioritized branch names the durable-corruption-first discriminator`, () => {
    const phase2 = phaseSection(skill, 'Phase 2')
    assert.match(phase2, /`durable-corruption-first`/, 'Phase 2 must name the selection rule')
    assert.match(
      phase2,
      /\*\*always\*\*[^.]{0,40}beats[^.]{0,40}in-run/i,
      'the rule must rank durable failure above in-run cost, not merely list the buckets',
    )
    assert.match(
      phase2,
      /oldest\s+`createdAt`\s+first/,
      'the rule must carry a deterministic within-bucket tie-break',
    )
    assert.doesNotMatch(
      phase2,
      /most\s+impactful/i,
      'the unnamed-judgement marker must be gone, not merely supplemented',
    )
  })

  test(`${label}: the edge-case row routes to the one discriminator definition`, () => {
    const edgeCases = phaseSection(skill, 'Edge cases')
    assert.match(
      edgeCases,
      /All\s+candidates\s+unprioritized[^\n]*`durable-corruption-first`/,
      'the edge-case row must point at the Phase 2 rule by name',
    )
    assert.doesNotMatch(
      edgeCases,
      /most\s+impactful/i,
      'the edge-case row must not keep a second improvisable copy of the judgement',
    )
  })

  // An epic outcome has no singular attachment id / estimate / priority, so the single-ticket
  // report shape could only be improvised. Pin the outcome TOKEN and the roster fields.
  test(`${label}: Phase 5 has an epic terminal outcome carrying a per-child roster`, () => {
    const phase5 = phaseSection(skill, 'Phase 5')
    assert.match(phase5, /`planned\s+epic\s+<PARENT-ID>`/, 'must add the epic terminal outcome')
    for (const field of [/per-child\s+roster/i, /`id`/, /`estimate`/, /`priority`/]) {
      assert.match(phase5, field, `the epic roster must carry ${field.source}`)
    }
    assert.match(
      phase5,
      /`attached\s+<attachment-id>`[\s\S]{0,40}`missing`/,
      'the roster must carry both plan-attachment states',
    )
    assert.match(
      phase5,
      /status\s+change\s+`<unplanned\s+state>\s+→\s+planned`/,
      'the epic outcome must report the parent state flip',
    )
  })

  // The row used to offer the whole-sweep `mkdir` lock as the guard that did not exist yet.
  // A per-ticket guard exists now, so the row must say so AND say why the whole-sweep lock was
  // still not taken — the heartbeat citation is retained, with its verdict inverted.
  test(`${label}: no-lock edge case points at supersede and the existing heartbeat option`, () => {
    const edgeCases = phaseSection(skill, 'Edge cases')
    assert.doesNotMatch(edgeCases, /bossd\s+schedules\s+one\s+cron\s+session\s+per\s+job/)
    assert.match(edgeCases, /publish-side\s+supersede/i)
    assert.match(edgeCases, /`mkdir`\s+heartbeat\s+lock\s+from\s+`bs-sweep-security`/)
    assert.match(edgeCases, /plan-peer-claim\.mjs/, 'the row must state the per-ticket guard')
  })
}
