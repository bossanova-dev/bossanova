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

  test(`${label}: Phase 5 has the skipped terminal outcome`, () => {
    const phase5 = phaseSection(skill, 'Phase 5')
    assert.match(phase5, /`skipped\s+<ISSUE-ID>:\s+already\s+planned`/)
  })

  test(`${label}: no-lock edge case points at supersede and the existing heartbeat option`, () => {
    const edgeCases = phaseSection(skill, 'Edge cases')
    assert.doesNotMatch(edgeCases, /bossd\s+schedules\s+one\s+cron\s+session\s+per\s+job/)
    assert.match(edgeCases, /publish-side\s+supersede/i)
    assert.match(edgeCases, /`mkdir`\s+heartbeat\s+lock\s+from\s+`bs-sweep-security`/)
  })
}
