import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { tempDir } from '../test-infra/test-tmpdir.mjs'
import { checkSkillShellInRepo } from './check-skill-shell.mjs'
import {
  RUNNER_FORM_ROOT,
  findLegacyToolboxMentions,
  findRunnerFormViolations,
  inRunnerFormScope,
} from './skill-shell-runner-form.mjs'

const runner = 'bash <skill-dir>/toolbox/plan/boss-plan-run.sh'

for (const [reason, body] of [
  ['multi-line', `${runner} steps\n${runner} node helper.mjs`],
  ['multi-line', `${runner} steps \\`],
  ['heredoc', `${runner} node helper.mjs <<EOF\nEOF`],
  ['command-substitution', `${runner} node helper.mjs "$(echo x)"`],
  ['command-substitution', `${runner} node helper.mjs \`echo x\``],
  ['parameter-expansion', `${runner} node helper.mjs "\${VALUE}"`],
  ['sourcing', '. preamble.sh'],
  ['sourcing', `${runner} steps; source preamble.sh`],
  ['bash-c', 'bash -c "echo x"'],
  ['bash-c', 'sh -c "echo x"'],
  ['bash-c', 'zsh -c "echo x"'],
  ['not-runner', 'node helper.mjs'],
  ['not-runner', 'bash /other/runner.sh steps'],
]) {
  test(`runner form rejects ${reason}: ${body}`, () => {
    assert.ok(findRunnerFormViolations(body).includes(reason))
  })
}

for (const body of [
  `${runner} <step> [--flag value …]`,
  `${runner} node helper.mjs '$(' '\${literal}' '\`literal\`'`,
  `${runner} node helper.mjs '{"value":"$literal"}'`,
  `${runner} steps # source x; bash -c '$(x)' \${literal} <<EOF`,
  `${runner} steps\n# trailing comment\n`,
  `${runner} steps # comment ending in \\`,
  `${runner} node helper.mjs '<<literal' "<<literal"`,
  `${runner} node helper.mjs \\$(literal)`,
  `bash "/space here/toolbox/plan/boss-plan-run.sh" steps`,
  `${runner} node helper.mjs source bash -c`,
  `${runner} node helper.mjs ${String.fromCharCode(92).repeat(2)}`,
  `${runner} node helper.mjs ${String.fromCharCode(92)} `,
]) {
  test(`runner form accepts literal one-line invocation: ${body}`, () => {
    assert.deepEqual(findRunnerFormViolations(body), [])
  })
}

test('runner scope covers only the published planning core and its references', () => {
  for (const name of ['SKILL.md', 'references/epic.md', 'references/nested/epic.md']) {
    assert.equal(inRunnerFormScope(`${RUNNER_FORM_ROOT}${name}`), true)
  }
  for (const name of ['extensions/boss-plan-notes/EXTENSION.md', 'toolbox/README.md']) {
    assert.equal(inRunnerFormScope(`${RUNNER_FORM_ROOT}${name}`), false)
  }
  assert.equal(inRunnerFormScope('.claude/skills/boss-plan-feature-map/SKILL.md'), false)
  assert.equal(inRunnerFormScope(`${RUNNER_FORM_ROOT}references/epic.txt`), false)
})

test('legacy toolbox mentions include prose and fenced code with their line numbers', () => {
  assert.deepEqual(
    findLegacyToolboxMentions('Use BOSS_PLAN_TOOLBOX.\n```sh\n. boss-plan-env.sh\n```\n'),
    [
      { line: 1, reason: 'legacy-toolbox' },
      { line: 3, reason: 'legacy-toolbox' },
    ],
  )
})

test('repository guard reports fence and mention lines and excludes extensions', async (t) => {
  const root = tempDir('runner-form-')
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  const markdown = `# Fixture\nUse BOSS_PLAN_TOOLBOX.\n\n\`\`\`sh\n${runner} steps\n${runner} node helper.mjs\n\`\`\`\n`
  for (const name of ['SKILL.md', 'extensions/example/EXTENSION.md']) {
    const file = path.join(root, RUNNER_FORM_ROOT, name)
    fs.mkdirSync(path.dirname(file), { recursive: true })
    fs.writeFileSync(file, markdown)
  }
  const findings = (await checkSkillShellInRepo(root)).filter((f) => f.kind === 'runner-form')
  assert.deepEqual(
    findings.map(({ file, line }) => ({ file, line })).sort((a, b) => a.line - b.line),
    [
      { file: `${RUNNER_FORM_ROOT}SKILL.md`, line: 2 },
      { file: `${RUNNER_FORM_ROOT}SKILL.md`, line: 4 },
    ],
  )
  assert.match(findings.find((f) => f.line === 4).message, /multi-line/)
  assert.ok(findings.every((f) => f.message.includes('boss-plan-run.sh')))
})
