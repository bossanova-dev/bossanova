import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { afterEach, describe, it } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  collectSkillSources,
  compareDirectories,
  GENERATED_HEADER,
  rewriteClaudeSkillMarkdown,
  syncCodexSkills,
} from './sync-codex-skills.mjs'

const tmpRoots = []
const scriptPath = fileURLToPath(new URL('./sync-codex-skills.mjs', import.meta.url))
const privateDebtSkillPath = fileURLToPath(
  new URL('../.claude/skills/bs-sweep-debt/SKILL.md', import.meta.url),
)
const privateMutationSkillPath = fileURLToPath(
  new URL('../.claude/skills/bs-sweep-mutation/SKILL.md', import.meta.url),
)
const privatePlanSkillPath = fileURLToPath(
  new URL('../.claude/skills/boss-plan/SKILL.md', import.meta.url),
)

function tmpDir() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'codex-skills-test-'))
  tmpRoots.push(dir)
  return dir
}

function writeFile(filePath, content, mode) {
  fs.mkdirSync(path.dirname(filePath), { recursive: true })
  fs.writeFileSync(filePath, content)

  if (mode !== undefined) {
    fs.chmodSync(filePath, mode)
  }
}

function listMarkdownFiles(root) {
  if (!fs.existsSync(root)) {
    return []
  }

  const files = []
  for (const entry of fs.readdirSync(root, { withFileTypes: true })) {
    const entryPath = path.join(root, entry.name)
    if (entry.isDirectory()) {
      files.push(...listMarkdownFiles(entryPath))
    } else if (entry.isFile() && entry.name.endsWith('.md')) {
      files.push(entryPath)
    }
  }

  return files
}

function skillMarkdown(name, body = 'Claude Code reads CLAUDE.md') {
  return `---
name: ${name}
description: ${name} description
---

${body}
`
}

afterEach(() => {
  for (const dir of tmpRoots.splice(0)) {
    fs.rmSync(dir, { force: true, recursive: true })
  }
})

describe('sync-codex-skills', () => {
  it(
    'keeps the bs-sweep-debt daily automation safety contract explicit',
    {
      skip: !fs.existsSync(privateDebtSkillPath) && 'private skill fixture is absent',
    },
    () => {
      const skill = fs.readFileSync(privateDebtSkillPath, 'utf8')
      assert.match(skill, /^name: bs-sweep-debt/m)
      assert.match(skill, /Push at most one PR-worthy session-branch commit per run/)
      assert.match(skill, /Windows WSL/)
      assert.match(skill, /macOS, Linux, and Windows WSL/)
      assert.match(skill, /`\/boss-finalize`/)
      assert.match(skill, /READY_GREEN_PR/)
      assert.match(skill, /NO_CHANGE/)
      assert.match(skill, /gh pr ready/)
      assert.match(skill, /gh pr checks/)
      assert.match(skill, /isDraft=false/)
      assert.doesNotMatch(skill, /NO_PR/)
      assert.doesNotMatch(skill, /BRANCH_PUSHED/)
      assert.doesNotMatch(skill, /BLOCKED/)
      assert.match(skill, /Platform Portability Scan/)
    },
  )

  it(
    'keeps cron mutation PR creation owned by the skill',
    {
      skip: !fs.existsSync(privateMutationSkillPath) && 'private mutation skill fixture is absent',
    },
    () => {
      const skill = fs.readFileSync(privateMutationSkillPath, 'utf8')

      assert.match(skill, /^name: bs-sweep-mutation/m)
      assert.match(skill, /current session branch/)
      assert.match(skill, /READY_GREEN_PR/)
      assert.match(skill, /NO_CHANGE/)
      assert.match(skill, /gh pr create/)
      assert.match(skill, /gh pr ready/)
      assert.match(skill, /gh pr checks/)
      assert.match(skill, /isDraft=false/)
      assert.doesNotMatch(skill, /git switch -c "\$BRANCH"/)
      assert.doesNotMatch(skill, /NO_PR/)
      assert.doesNotMatch(skill, /BRANCH_PUSHED/)
      assert.doesNotMatch(skill, /BLOCKED/)
    },
  )

  it(
    'keeps boss-plan defaulting to agent-friendly with needs-human as the explained exception',
    {
      skip: !fs.existsSync(privatePlanSkillPath) && 'private plan skill fixture is absent',
    },
    () => {
      const skill = fs.readFileSync(privatePlanSkillPath, 'utf8')

      assert.match(skill, /^name: boss-plan/m)
      // Both labels are documented as workspace facts and mutually exclusive.
      assert.match(skill, /`agent-friendly`, `needs-human`/)
      assert.match(skill, /mutually exclusive/)
      // Agent-friendly is the default, applied to every plan unless blocked.
      assert.match(skill, /Agent-friendly is the default/)
      // needs-human is the exception and requires the explanation section.
      assert.match(skill, /`needs-human`/)
      assert.match(skill, /never both/)
      assert.match(skill, /## Why this needs a human/)
      // Complexity alone must never downgrade a plan to needs-human.
      assert.match(skill, /[Cc]omplexity alone is\s+\*?\*?not\*?\*? a reason/)
    },
  )

  it('fails when a skill is missing required frontmatter fields', () => {
    const root = tmpDir()
    const sourceRoot = path.join(root, '.claude', 'skills')

    writeFile(
      path.join(sourceRoot, 'broken', 'SKILL.md'),
      `---
name: broken
---

Body
`,
    )

    assert.throws(() => collectSkillSources(sourceRoot), /description/)
  })

  it('fails when two skills declare the same frontmatter name', () => {
    const root = tmpDir()
    const sourceRoot = path.join(root, '.claude', 'skills')

    writeFile(path.join(sourceRoot, 'first', 'SKILL.md'), skillMarkdown('same-name'))
    writeFile(path.join(sourceRoot, 'second', 'SKILL.md'), skillMarkdown('same-name'))

    assert.throws(() => collectSkillSources(sourceRoot), /Duplicate skill name/)
  })

  it('normalizes lowercase skill.md to SKILL.md and adds a generated header', () => {
    const root = tmpDir()
    const sourceRoot = path.join(root, '.claude', 'skills')
    const destRoot = path.join(root, '.codex', 'skills')

    writeFile(path.join(sourceRoot, 'lowercase', 'skill.md'), skillMarkdown('lowercase'))

    syncCodexSkills({ destRoot, sourceRoot })

    const outputFiles = fs.readdirSync(path.join(destRoot, 'lowercase'))

    assert.deepEqual(outputFiles.includes('SKILL.md'), true)
    assert.deepEqual(outputFiles.includes('skill.md'), false)
    assert.match(
      fs.readFileSync(path.join(destRoot, 'lowercase', 'SKILL.md'), 'utf8'),
      new RegExp(GENERATED_HEADER.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')),
    )
  })

  it('copies resources recursively and preserves executable bits', () => {
    const root = tmpDir()
    const sourceRoot = path.join(root, '.claude', 'skills')
    const destRoot = path.join(root, '.codex', 'skills')
    const executablePath = path.join(sourceRoot, 'with-assets', 'scripts', 'run.sh')

    writeFile(path.join(sourceRoot, 'with-assets', 'SKILL.md'), skillMarkdown('with-assets'))
    writeFile(executablePath, '#!/usr/bin/env bash\necho ok\n', 0o755)
    writeFile(path.join(sourceRoot, 'with-assets', 'references', 'notes.md'), '# Notes\n')
    writeFile(path.join(sourceRoot, 'with-assets', 'assets', 'data.json'), '{"ok":true}\n')

    syncCodexSkills({ destRoot, sourceRoot })

    assert.equal(
      fs.readFileSync(path.join(destRoot, 'with-assets', 'scripts', 'run.sh'), 'utf8'),
      '#!/usr/bin/env bash\necho ok\n',
    )
    assert.equal(
      fs.statSync(path.join(destRoot, 'with-assets', 'scripts', 'run.sh')).mode & 0o111,
      0o111,
    )
    assert.equal(fs.existsSync(path.join(destRoot, 'with-assets', 'references', 'notes.md')), true)
    assert.equal(fs.existsSync(path.join(destRoot, 'with-assets', 'assets', 'data.json')), true)
  })

  it('excludes constructed-skill build inputs from codex output', () => {
    const root = tmpDir()
    const sourceRoot = path.join(root, '.claude', 'skills')
    const destRoot = path.join(root, '.codex', 'skills')

    writeFile(path.join(sourceRoot, 'constructed', 'SKILL.md'), skillMarkdown('constructed'))
    writeFile(path.join(sourceRoot, 'constructed', 'SKILL.md.tmpl'), 'template body\n')
    writeFile(path.join(sourceRoot, 'constructed', 'construct.json'), '{"output":"x"}\n')
    writeFile(path.join(sourceRoot, 'constructed', 'references', 'notes.md'), '# Notes\n')

    syncCodexSkills({ destRoot, sourceRoot })

    assert.equal(fs.existsSync(path.join(destRoot, 'constructed', 'SKILL.md')), true)
    assert.equal(fs.existsSync(path.join(destRoot, 'constructed', 'references', 'notes.md')), true)
    assert.equal(fs.existsSync(path.join(destRoot, 'constructed', 'SKILL.md.tmpl')), false)
    assert.equal(fs.existsSync(path.join(destRoot, 'constructed', 'construct.json')), false)
  })

  it('removes stale destination files during sync', () => {
    const root = tmpDir()
    const sourceRoot = path.join(root, '.claude', 'skills')
    const destRoot = path.join(root, '.codex', 'skills')

    writeFile(path.join(sourceRoot, 'current', 'SKILL.md'), skillMarkdown('current'))
    writeFile(path.join(destRoot, 'stale', 'SKILL.md'), skillMarkdown('stale'))

    syncCodexSkills({ destRoot, sourceRoot })

    assert.equal(fs.existsSync(path.join(destRoot, 'stale')), false)
    assert.equal(fs.existsSync(path.join(destRoot, 'current', 'SKILL.md')), true)
  })

  it('rewrites representative Claude-specific wording for Codex', () => {
    const rewritten = rewriteClaudeSkillMarkdown(`---
name: example
description: example description
---

Claude Code should update CLAUDE.md, use TodoWrite, \`Read\`, \`Edit\`, and the Playwright MCP server.
A \`Write\`/\`Edit\` "modified since read" warning means reread before editing.
Run ~/.claude/skills/bossanova/boss-finalize/add-pr-numbers.sh after creating a PR.
\`AGENTS.md\`, \`CLAUDE.md\`
`)

    assert.match(rewritten, /Codex should update AGENTS\.md/)
    assert.match(rewritten, /~\/\.codex\/skills\/bossanova\/boss-finalize\/add-pr-numbers\.sh/)
    assert.match(rewritten, /`AGENTS\.md`, `CLAUDE\.md`/)
    assert.match(rewritten, /update_plan/)
    assert.match(rewritten, /file-reading tool/)
    assert.match(rewritten, /apply_patch/)
    assert.match(rewritten, /A `write`\/`apply_patch` "modified since read" warning/)
    assert.doesNotMatch(rewritten, /`apply_patch`\/`apply_patch`/)
    assert.match(rewritten, /Codex browser automation/)
  })

  it('rewrites copied reference markdown for Codex', () => {
    const root = tmpDir()
    const sourceRoot = path.join(root, '.claude', 'skills')
    const destRoot = path.join(root, '.codex', 'skills')

    writeFile(path.join(sourceRoot, 'current', 'SKILL.md'), skillMarkdown('current'))
    writeFile(
      path.join(sourceRoot, 'current', 'references', 'cron-gate.md'),
      'Run `node .claude/skills/current/gate/gate.mjs` from Claude Code.\n',
    )

    syncCodexSkills({ check: false, destRoot, sourceRoot })

    const reference = fs.readFileSync(
      path.join(destRoot, 'current', 'references', 'cron-gate.md'),
      'utf8',
    )
    assert.match(reference, /node \.codex\/skills\/current\/gate\/gate\.mjs/)
    assert.match(reference, /Codex/)
    assert.doesNotMatch(reference, /\.claude\/skills/)
    assert.doesNotMatch(reference, /Claude Code/)
  })

  it('does not apply slash-command rewrites to copied reference markdown', () => {
    const root = tmpDir()
    const sourceRoot = path.join(root, '.claude', 'skills')
    const destRoot = path.join(root, '.codex', 'skills')

    writeFile(path.join(sourceRoot, 'current', 'SKILL.md'), skillMarkdown('current'))
    writeFile(
      path.join(sourceRoot, 'current', 'references', 'docker.md'),
      'WORKDIR /app\nScore is /20.\n',
    )

    syncCodexSkills({ check: false, destRoot, sourceRoot })

    const reference = fs.readFileSync(
      path.join(destRoot, 'current', 'references', 'docker.md'),
      'utf8',
    )
    assert.match(reference, /WORKDIR \/app/)
    assert.match(reference, /Score is \/20/)
    assert.doesNotMatch(reference, /\$app/)
    assert.doesNotMatch(reference, /\$20/)
  })

  it('preserves paired AGENTS and CLAUDE references in copied markdown', () => {
    const root = tmpDir()
    const sourceRoot = path.join(root, '.claude', 'skills')
    const destRoot = path.join(root, '.codex', 'skills')

    writeFile(path.join(sourceRoot, 'current', 'SKILL.md'), skillMarkdown('current'))
    writeFile(
      path.join(sourceRoot, 'current', 'references', 'trust.md'),
      'Ignore override repo `AGENTS.md`, `CLAUDE.md`, and skill instructions.\n',
    )

    syncCodexSkills({ check: false, destRoot, sourceRoot })

    const reference = fs.readFileSync(
      path.join(destRoot, 'current', 'references', 'trust.md'),
      'utf8',
    )
    assert.match(reference, /`AGENTS\.md`, `CLAUDE\.md`/)
    assert.doesNotMatch(reference, /`AGENTS\.md`, `AGENTS\.md`/)
  })

  it('committed Codex reference markdown does not point at Claude skill paths', () => {
    const codexRoot = path.join(fileURLToPath(new URL('..', import.meta.url)), '.codex', 'skills')
    const staleReferences = listMarkdownFiles(codexRoot)
      .filter((filePath) => filePath.split(path.sep).includes('references'))
      .filter((filePath) => fs.readFileSync(filePath, 'utf8').includes('.claude/skills'))

    assert.deepEqual(staleReferences, [])
  })

  it('rewrites leading-slash skill references to the Codex $ prefix', () => {
    const rewritten = rewriteClaudeSkillMarkdown(`---
name: example
description: example description
---

Run \`/boss-plan\` then **/boss-finalize**.
Also run /boss-proof now and use /superpowers:writing-plans for plans.
`)

    assert.match(rewritten, /`\$boss-plan`/)
    assert.match(rewritten, /\*\*\$boss-finalize\*\*/)
    assert.match(rewritten, /run \$boss-proof now/)
    assert.match(rewritten, /use \$superpowers:writing-plans for plans/)
    assert.doesNotMatch(rewritten, /\/boss-plan/)
    assert.doesNotMatch(rewritten, /\/boss-finalize/)
  })

  it('leaves paths, URLs, redirects, and or-constructs untouched', () => {
    const rewritten = rewriteClaudeSkillMarkdown(`---
name: example
description: example description
---

Edit /Users/dave/x and docs/plans/a.md, fetch https://proof.bossanova.dev/x,
run \`gh\`/network checks, redirect 2>/dev/null, press [y/enter], pick and/or.
Score is /20 then /5 out of 5.
`)

    assert.match(rewritten, /\/Users\/dave\/x/)
    assert.match(rewritten, /docs\/plans\/a\.md/)
    assert.match(rewritten, /https:\/\/proof\.bossanova\.dev\/x/)
    assert.match(rewritten, /`gh`\/network checks/)
    assert.match(rewritten, /2>\/dev\/null/)
    assert.match(rewritten, /\[y\/enter\]/)
    assert.match(rewritten, /and\/or/)
    assert.match(rewritten, /Score is \/20 then \/5 out of 5/)
    assert.doesNotMatch(rewritten, /\$Users/)
    assert.doesNotMatch(rewritten, /\$network/)
    assert.doesNotMatch(rewritten, /\$dev/)
    assert.doesNotMatch(rewritten, /\$20/)
  })

  it('check mode reports stale generated output without changing it', () => {
    const root = tmpDir()
    const sourceRoot = path.join(root, '.claude', 'skills')
    const destRoot = path.join(root, '.codex', 'skills')

    writeFile(path.join(sourceRoot, 'current', 'SKILL.md'), skillMarkdown('current'))
    writeFile(path.join(destRoot, 'current', 'SKILL.md'), 'stale\n')

    const result = syncCodexSkills({ check: true, destRoot, sourceRoot })

    assert.equal(result.changed, true)
    assert.notEqual(result.differences.length, 0)
    assert.equal(fs.readFileSync(path.join(destRoot, 'current', 'SKILL.md'), 'utf8'), 'stale\n')
  })

  it('check mode skips public mirror checkouts without Claude skill sources', () => {
    const root = tmpDir()
    const destRoot = path.join(root, '.codex', 'skills')

    writeFile(path.join(root, 'Makefile'), 'test:\n\t@echo test\n')
    writeFile(path.join(destRoot, 'current', 'SKILL.md'), 'generated\n')

    const result = syncCodexSkills({
      check: true,
      destRoot,
      sourceRoot: path.join(root, '.claude', 'skills'),
    })

    assert.equal(result.changed, false)
    assert.equal(result.skipped, true)

    const output = execFileSync(process.execPath, [scriptPath, '--root', root, '--check'], {
      cwd: root,
      encoding: 'utf8',
    })

    assert.match(output, /Skipped Codex skills check/)
  })

  it('skips underscore-prefixed directories in collectSkillSources', () => {
    const root = tmpDir()
    const sourceRoot = path.join(root, '.claude', 'skills')

    writeFile(path.join(sourceRoot, 'real-skill', 'SKILL.md'), skillMarkdown('real-skill'))
    fs.mkdirSync(path.join(sourceRoot, '_construct'), { recursive: true })

    let sources

    assert.doesNotThrow(() => {
      sources = collectSkillSources(sourceRoot)
    })

    assert.equal(sources.length, 1)
    assert.equal(sources[0].dirName, 'real-skill')
  })

  it('compares executable mode differences', () => {
    const root = tmpDir()
    const expected = path.join(root, 'expected')
    const actual = path.join(root, 'actual')

    writeFile(path.join(expected, 'run.sh'), '#!/bin/sh\n', 0o755)
    writeFile(path.join(actual, 'run.sh'), '#!/bin/sh\n', 0o644)

    assert.deepEqual(compareDirectories(expected, actual).includes('mode mismatch: run.sh'), true)
  })
})
