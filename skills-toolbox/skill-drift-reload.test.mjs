import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import {
  classifySkillDrift,
  parseSkillGateReloads,
  renderSkillDriftVerdict,
} from './skill-drift-verdict.mjs'

const script = fileURLToPath(new URL('./skill-drift-verdict.mjs', import.meta.url))
// Captured from TestRunSkillGateHeal_BossSkills_AC246401/recoverable (-v), with
// the temporary home/checkout replaced by /home and /repo.
const reloadLine = `boss skills gate: claude healed 2 path(s) from origin/HEAD merge-base 6cfac8c831ad: boss-build/SKILL.md, boss/SKILL.md
boss skills gate: reload core=boss agent=claude paths=boss/SKILL.md
boss skills gate: reload core=boss-build agent=claude paths=boss-build/SKILL.md
boss skills gate: claude clean — compared 2 file(s) · skills dir: /home/.claude/skills · payload source: /repo/lib/bossalib/contract/plugin/skillpayload
`
const invoke = (args, input) =>
  spawnSync(process.execPath, [script, ...args], { input, encoding: 'utf8' })

test('reload filters healed bodies by core and requires both consumer arguments', () => {
  assert.deepEqual(parseSkillGateReloads(reloadLine), [
    { core: 'boss', agent: 'claude', paths: ['boss/SKILL.md'] },
    { core: 'boss-build', agent: 'claude', paths: ['boss-build/SKILL.md'] },
  ])
  const args = ['reload', '--core', 'boss-build', '--skill-md', '/pin/SKILL.md']
  const matching = invoke(args, reloadLine)
  assert.equal(matching.status, 3, matching.stdout + matching.stderr)
  assert.match(matching.stdout, /^RELOAD:.*boss-build.*boss-build\/SKILL.md.*\/pin\/SKILL.md/)
  assert.equal(invoke(args, '').stdout, '')
  assert.equal(invoke(args, '').status, 0)
  const other = invoke(['reload', '--core', 'boss-plan', '--skill-md', '/pin/SKILL.md'], reloadLine)
  assert.equal(other.status, 0)
  assert.equal(other.stdout, '')
  for (const missing of [
    ['reload'],
    ['reload', '--core', 'boss-build'],
    ['reload', '--skill-md', '/pin/SKILL.md'],
    ['invalid'],
  ]) {
    assert.equal(invoke(missing, reloadLine).status, 2)
  }
  assert.equal(invoke(args, reloadLine.replaceAll('agent=claude', 'agent=codex')).status, 3)
})

test('reload headers do not change drift severity', () => {
  for (const exitStatus of [0, 1]) {
    const output =
      'boss skills gate: claude skill drift detected\n  - boss-build/a.mjs (content, behind)'
    assert.deepEqual(
      classifySkillDrift({ exitStatus, output }),
      classifySkillDrift({ exitStatus, output: output + '\n' + reloadLine }),
    )
  }
})

test('lost config validators block only inside the consumer closure', () => {
  for (const kind of ['content', 'unexpected']) {
    for (const direction of ['unrecoverable', 'unknown', 'ahead', 'behind']) {
      const path = 'boss-plan/toolbox/skill-config-validate.generated.mjs'
      const output = `boss skills gate: claude skill drift detected\n  - ${path} (${kind}, ${direction})\n  next: set the listed paths aside and reinstall with \`boss skills install --set-aside --agent claude\``
      const result = classifySkillDrift({ exitStatus: 1, output, cores: ['boss-plan'] })
      const lost = ['unrecoverable', 'unknown'].includes(direction)
      assert.equal(
        result.verdict,
        lost ? 'blocking' : direction === 'ahead' ? 'advisory-withheld' : 'advisory',
      )
      if (lost) {
        assert.equal(result.reason, 'config-validator-drift')
        assert.equal(result.rows[0].configValidator, true)
        const rendered = renderSkillDriftVerdict(result).join('\n')
        assert.match(rendered, /BLOCKED:.*skill-config-validate.generated.mjs/)
        assert.match(rendered, /\.boss-skills.json/)
        assert.match(rendered, /--set-aside/)
        assert.ok(!rendered.includes('bookkeeping only, work state unaffected'))
      }
      assert.equal(
        invoke(['classify', '--status', '1', '--core', 'boss-plan'], output).status,
        lost ? 1 : 0,
      )
      assert.equal(
        classifySkillDrift({ exitStatus: 1, output, cores: ['boss-repair'] }).blocking,
        false,
      )
    }
  }
})

test('verify includes its review core and finalize excludes unrelated cores', () => {
  const output = '  - boss-review/toolbox/missing.mjs (absent, lossless)'
  assert.equal(classifySkillDrift({ exitStatus: 1, output, cores: ['boss-verify'] }).blocking, true)
  assert.equal(
    classifySkillDrift({ exitStatus: 1, output, cores: ['boss-finalize'] }).blocking,
    false,
  )
})
