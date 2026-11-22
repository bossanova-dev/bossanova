import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import {
  checkMarkdown,
  checkSkillPinLocatorsInRepo,
  findLocatorLines,
} from './check-skill-pin-locators.mjs'

const md = (...lines) => lines.join('\n')
const marker = '<!-- pre-pin: preflight creates the run pin -->'

test('a shared-tree locator in a block after preflight fails at its file and line', () => {
  const text = md(
    '## Preflight',
    marker,
    '```bash',
    'BOSS_SKILLS_HOME="$HOME/.codex/skills"',
    'node "$BOSS_SKILLS_HOME/boss-build/toolbox/skill-pin.mjs" create --skill boss-build',
    '```',
    '## Step 3',
    '```bash',
    'for candidate in "$HOME/.claude/skills" "$HOME/.codex/skills"; do',
    '  echo "$candidate"',
    'done',
    '```',
  )
  const result = checkMarkdown('boss-build/SKILL.md', text)
  assert.equal(result.acceptedMarkers, 1)
  assert.equal(result.violations.length, 1)
  assert.equal(result.violations[0].file, 'boss-build/SKILL.md')
  assert.equal(result.violations[0].line, 9)
  assert.match(result.violations[0].message, /home/)
})

test('shared-tree locator forms are detected without matching repo paths or other identifiers', () => {
  const forms = [
    '~/.claude/skills/boss-build',
    '~/.codex/skills/boss-build',
    '$HOME/.codex/skills',
    '${HOME}/.claude/skills',
    '"$HOME"/.claude/skills',
    '${HOME}/.codex/skills',
    '"$HOME"/.codex/skills',
    '${BOSS_SKILLS_HOME:-fallback}',
    'BOSS_SKILLS_HOME=skills',
    'bash <skill-dir>/toolbox/plan/boss-plan-run.sh',
  ]
  for (const form of forms) assert.equal(findLocatorLines(form).length, 1, form)
  assert.deepEqual(
    findLocatorLines(
      md(
        '.codex/skills/x/SKILL.md',
        '.claude/skills/local/SKILL.md',
        'BOSS_REVIEW_SKILLS_HOME=skills',
        'NOT_BOSS_SKILLS_HOME=skills',
        '# ~/.claude/skills is a full-line comment',
        '"$(git rev-parse --absolute-git-dir)/boss-skill-pins/boss-build"',
      ),
    ),
    [],
  )
})

test('marked shell and untagged fences count accepted blocks in containers', () => {
  for (const info of ['bash', 'SH', 'shell', 'zsh', '']) {
    for (const prefix of ['', '  ', '> ']) {
      const text = md(
        '- setup',
        `${prefix}${marker}`,
        '',
        `${prefix}\`\`\`${info}`,
        `${prefix}echo "$HOME/.codex/skills"`,
        `${prefix}\`\`\``,
      )
      assert.deepEqual(checkMarkdown('core.md', text), { violations: [], acceptedMarkers: 1 })
    }
  }
})

test('empty, stale and orphan markers fail', () => {
  const cases = [
    [md('<!-- pre-pin: -->', '```bash', 'echo ~/.codex/skills', '```'), /empty/],
    [md(marker, '```bash', 'echo pinned', '```'), /stale/],
    [md(marker, 'prose'), /orphan/],
    [md(marker, '```markdown', '~/.codex/skills', '```'), /orphan/],
    [md(marker, '```json', '{}', '```'), /orphan/],
    [marker, /orphan/],
  ]
  for (const [text, message] of cases) {
    const result = checkMarkdown('core.md', text)
    assert.ok(
      result.violations.some((v) => message.test(v.message)),
      text,
    )
    assert.equal(result.acceptedMarkers, 0)
  }
})

test('only shell and untagged fences are scanned with exact locator coordinates', () => {
  const text = md(
    '```markdown',
    '~/.claude/skills',
    '```',
    '```json',
    '"~/.codex/skills"',
    '```',
    '```text',
    '~/.codex/skills',
    '```',
    '```',
    '# comment',
    'echo ~/.codex/skills',
    '```',
  )
  const result = checkMarkdown('core.md', text)
  assert.equal(result.violations.length, 1)
  assert.equal(result.violations[0].line, 12)
})

test('walker scans published markdown including references and excludes toolbox and other skills', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'skill-pin-locators-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  const payload = 'lib/bossalib/contract/plugin/skillpayload/skills'
  for (const rel of [
    'boss-build/SKILL.md',
    'boss-build/references/x.md',
    'boss-build/toolbox/y.md',
    'boss-build/references/toolbox/ignored.md',
    'boss/SKILL.md',
  ]) {
    const file = path.join(root, payload, rel)
    fs.mkdirSync(path.dirname(file), { recursive: true })
    fs.writeFileSync(file, md('```bash', 'echo ~/.codex/skills', '```'))
  }
  const result = checkSkillPinLocatorsInRepo(root)
  assert.deepEqual(result.violations.map((v) => v.file).sort(), [
    `${payload}/boss-build/SKILL.md`,
    `${payload}/boss-build/references/x.md`,
  ])
})
