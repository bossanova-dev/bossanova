import assert from 'node:assert/strict'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import fs from 'node:fs'
import { constructSkill, GENERATED_HEADER, skillsRootAvailable } from './construct-skills.mjs'

const rootDir = fileURLToPath(new URL('..', import.meta.url))
const manifest = JSON.parse(
  fs.readFileSync(path.join(rootDir, '.claude/skills/bs-implement/construct.json'), 'utf8'),
)
// Construction reads the component skills from the installed superpowers plugin.
// Skip (don't throw at import) where it is absent — e.g. a fresh CI runner.
const sourceSkip =
  !skillsRootAvailable(manifest) && `superpowers ${manifest.superpowers_version} not installed`
const out = sourceSkip ? '' : constructSkill(manifest, { rootDir })

test(
  'SDD spine is inlined (de-nested), not invoked as a nested skill',
  { skip: sourceSkip },
  () => {
    assert.match(out, /fresh implementer subagent per task/)
    assert.doesNotMatch(out, /Skill\(subagent-driven-development\)/)
  },
)

test('reviewer template + fix discipline are inlined', { skip: sourceSkip }, () => {
  assert.match(out, /Senior Code Reviewer/)
  assert.match(out, /receiving code review/i)
})

test('SDD final-review + lifecycle siblings are stripped', { skip: sourceSkip }, () => {
  assert.doesNotMatch(out, /Required workflow skills/)
  assert.doesNotMatch(out, /finishing-a-development-branch/)
})

test('scar tissue is gone', { skip: sourceSkip }, () => {
  assert.doesNotMatch(out, /sdd-ledger\.mjs/)
  assert.doesNotMatch(out, /single controller/i)
  assert.doesNotMatch(out, /phantom peer/i)
})

test('headless mode + mode-aware proof are present', { skip: sourceSkip }, () => {
  assert.match(out, /BS_HEADLESS/)
  assert.match(out, /proof\.mjs plan/)
})

test(
  'frontmatter is first: output starts with --- and GENERATED_HEADER appears after closing frontmatter',
  { skip: sourceSkip },
  () => {
    assert.match(out, /^---\n/, 'output must start with YAML frontmatter delimiter')
    const frontmatterCloseIdx = out.indexOf('\n---\n', 3) // skip opening ---
    assert.ok(frontmatterCloseIdx !== -1, 'must have a closing frontmatter delimiter')
    const headerIdx = out.indexOf(GENERATED_HEADER)
    assert.ok(headerIdx !== -1, 'GENERATED_HEADER must be present in output')
    assert.ok(
      headerIdx > frontmatterCloseIdx,
      `GENERATED_HEADER must appear after frontmatter closing ---, but header is at ${headerIdx} and frontmatter closes at ${frontmatterCloseIdx}`,
    )
  },
)

test('runtime helper references are local to generated Claude and Codex skills', () => {
  const expectedHelpers = [
    'support/superpowers-6.0.3/subagent-driven-development/implementer-prompt.md',
    'support/superpowers-6.0.3/subagent-driven-development/task-reviewer-prompt.md',
    'support/superpowers-6.0.3/subagent-driven-development/scripts/review-package',
    'support/superpowers-6.0.3/subagent-driven-development/scripts/task-brief',
    'support/superpowers-6.0.3/test-driven-development/testing-anti-patterns.md',
  ]

  const skillDirs = [
    path.join(rootDir, '.claude/skills/bs-implement'),
    path.join(rootDir, '.codex/skills/bs-implement'),
  ]

  for (const skillDir of skillDirs) {
    const skill = fs.readFileSync(path.join(skillDir, 'SKILL.md'), 'utf8')
    assert.doesNotMatch(
      skill,
      /\.claude\/skills\/_construct/,
      `${skillDir} must not reference construct inputs`,
    )

    for (const helper of expectedHelpers) {
      assert.match(
        skill,
        new RegExp(helper.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')),
        `${helper} must be referenced`,
      )
      assert.equal(
        fs.existsSync(path.join(skillDir, helper)),
        true,
        `${helper} must exist under ${skillDir}`,
      )
    }
  }
})
