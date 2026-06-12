import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

import {
  collectSkillSources,
  compareDirectories,
  GENERATED_HEADER,
  rewriteClaudeSkillMarkdown,
  syncCodexSkills,
} from './sync-codex-skills.mjs';

const tmpRoots = [];
const scriptPath = fileURLToPath(new URL('./sync-codex-skills.mjs', import.meta.url));
const privateDebtSkillPath = fileURLToPath(
  new URL('../.claude/skills/bs-technical-debt/SKILL.md', import.meta.url),
);
const privateMutationSkillPath = fileURLToPath(
  new URL('../.claude/skills/bs-mutation-test/SKILL.md', import.meta.url),
);

function tmpDir() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'codex-skills-test-'));
  tmpRoots.push(dir);
  return dir;
}

function writeFile(filePath, content, mode) {
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
  fs.writeFileSync(filePath, content);

  if (mode !== undefined) {
    fs.chmodSync(filePath, mode);
  }
}

function skillMarkdown(name, body = 'Claude Code reads CLAUDE.md') {
  return `---
name: ${name}
description: ${name} description
---

${body}
`;
}

afterEach(() => {
  for (const dir of tmpRoots.splice(0)) {
    fs.rmSync(dir, { force: true, recursive: true });
  }
});

describe('sync-codex-skills', () => {
  it(
    'keeps the bs-technical-debt daily automation safety contract explicit',
    {
      skip: !fs.existsSync(privateDebtSkillPath) && 'private skill fixture is absent',
    },
    () => {
      const skill = fs.readFileSync(privateDebtSkillPath, 'utf8');
      assert.match(skill, /^name: bs-technical-debt/m);
      assert.match(skill, /Push at most one PR-worthy session-branch commit per run/);
      assert.match(skill, /Windows WSL/);
      assert.match(skill, /macOS, Linux, and Windows WSL/);
      assert.match(skill, /`\/boss-finalize`/);
      assert.match(skill, /READY_GREEN_PR/);
      assert.match(skill, /NO_CHANGE/);
      assert.match(skill, /gh pr ready/);
      assert.match(skill, /gh pr checks/);
      assert.match(skill, /isDraft=false/);
      assert.doesNotMatch(skill, /NO_PR/);
      assert.doesNotMatch(skill, /BRANCH_PUSHED/);
      assert.doesNotMatch(skill, /BLOCKED/);
      assert.match(skill, /Platform Portability Scan/);
    },
  );

  it(
    'keeps cron mutation PR creation owned by the skill',
    {
      skip: !fs.existsSync(privateMutationSkillPath) && 'private mutation skill fixture is absent',
    },
    () => {
      const skill = fs.readFileSync(privateMutationSkillPath, 'utf8');

      assert.match(skill, /^name: bs-mutation-test/m);
      assert.match(skill, /current session branch/);
      assert.match(skill, /READY_GREEN_PR/);
      assert.match(skill, /NO_CHANGE/);
      assert.match(skill, /gh pr create/);
      assert.match(skill, /gh pr ready/);
      assert.match(skill, /gh pr checks/);
      assert.match(skill, /isDraft=false/);
      assert.doesNotMatch(skill, /git switch -c "\$BRANCH"/);
      assert.doesNotMatch(skill, /NO_PR/);
      assert.doesNotMatch(skill, /BRANCH_PUSHED/);
      assert.doesNotMatch(skill, /BLOCKED/);
    },
  );

  it('fails when a skill is missing required frontmatter fields', () => {
    const root = tmpDir();
    const sourceRoot = path.join(root, '.claude', 'skills');

    writeFile(
      path.join(sourceRoot, 'broken', 'SKILL.md'),
      `---
name: broken
---

Body
`,
    );

    assert.throws(() => collectSkillSources(sourceRoot), /description/);
  });

  it('fails when two skills declare the same frontmatter name', () => {
    const root = tmpDir();
    const sourceRoot = path.join(root, '.claude', 'skills');

    writeFile(path.join(sourceRoot, 'first', 'SKILL.md'), skillMarkdown('same-name'));
    writeFile(path.join(sourceRoot, 'second', 'SKILL.md'), skillMarkdown('same-name'));

    assert.throws(() => collectSkillSources(sourceRoot), /Duplicate skill name/);
  });

  it('normalizes lowercase skill.md to SKILL.md and adds a generated header', () => {
    const root = tmpDir();
    const sourceRoot = path.join(root, '.claude', 'skills');
    const destRoot = path.join(root, '.codex', 'skills');

    writeFile(path.join(sourceRoot, 'lowercase', 'skill.md'), skillMarkdown('lowercase'));

    syncCodexSkills({ destRoot, sourceRoot });

    const outputFiles = fs.readdirSync(path.join(destRoot, 'lowercase'));

    assert.deepEqual(outputFiles.includes('SKILL.md'), true);
    assert.deepEqual(outputFiles.includes('skill.md'), false);
    assert.match(
      fs.readFileSync(path.join(destRoot, 'lowercase', 'SKILL.md'), 'utf8'),
      new RegExp(GENERATED_HEADER.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')),
    );
  });

  it('copies resources recursively and preserves executable bits', () => {
    const root = tmpDir();
    const sourceRoot = path.join(root, '.claude', 'skills');
    const destRoot = path.join(root, '.codex', 'skills');
    const executablePath = path.join(sourceRoot, 'with-assets', 'scripts', 'run.sh');

    writeFile(path.join(sourceRoot, 'with-assets', 'SKILL.md'), skillMarkdown('with-assets'));
    writeFile(executablePath, '#!/usr/bin/env bash\necho ok\n', 0o755);
    writeFile(path.join(sourceRoot, 'with-assets', 'references', 'notes.md'), '# Notes\n');
    writeFile(path.join(sourceRoot, 'with-assets', 'assets', 'data.json'), '{"ok":true}\n');

    syncCodexSkills({ destRoot, sourceRoot });

    assert.equal(
      fs.readFileSync(path.join(destRoot, 'with-assets', 'scripts', 'run.sh'), 'utf8'),
      '#!/usr/bin/env bash\necho ok\n',
    );
    assert.equal(
      fs.statSync(path.join(destRoot, 'with-assets', 'scripts', 'run.sh')).mode & 0o111,
      0o111,
    );
    assert.equal(fs.existsSync(path.join(destRoot, 'with-assets', 'references', 'notes.md')), true);
    assert.equal(fs.existsSync(path.join(destRoot, 'with-assets', 'assets', 'data.json')), true);
  });

  it('removes stale destination files during sync', () => {
    const root = tmpDir();
    const sourceRoot = path.join(root, '.claude', 'skills');
    const destRoot = path.join(root, '.codex', 'skills');

    writeFile(path.join(sourceRoot, 'current', 'SKILL.md'), skillMarkdown('current'));
    writeFile(path.join(destRoot, 'stale', 'SKILL.md'), skillMarkdown('stale'));

    syncCodexSkills({ destRoot, sourceRoot });

    assert.equal(fs.existsSync(path.join(destRoot, 'stale')), false);
    assert.equal(fs.existsSync(path.join(destRoot, 'current', 'SKILL.md')), true);
  });

  it('rewrites representative Claude-specific wording for Codex', () => {
    const rewritten = rewriteClaudeSkillMarkdown(`---
name: example
description: example description
---

Claude Code should update CLAUDE.md, use TodoWrite, \`Read\`, \`Edit\`, and the Playwright MCP server.
Run ~/.claude/skills/bossanova/boss-finalize/add-pr-numbers.sh after creating a PR.
\`AGENTS.md\`, \`CLAUDE.md\`
`);

    assert.match(rewritten, /Codex should update AGENTS\.md/);
    assert.match(rewritten, /~\/\.codex\/skills\/bossanova\/boss-finalize\/add-pr-numbers\.sh/);
    assert.match(rewritten, /`AGENTS\.md`, `CLAUDE\.md`/);
    assert.match(rewritten, /update_plan/);
    assert.match(rewritten, /file-reading tool/);
    assert.match(rewritten, /apply_patch/);
    assert.match(rewritten, /Codex browser automation/);
  });

  it('rewrites leading-slash skill references to the Codex $ prefix', () => {
    const rewritten = rewriteClaudeSkillMarkdown(`---
name: example
description: example description
---

Run \`/bs-linear-plan\` then **/boss-finalize**.
Also run /bs-proof now and use /superpowers:writing-plans for plans.
`);

    assert.match(rewritten, /`\$bs-linear-plan`/);
    assert.match(rewritten, /\*\*\$boss-finalize\*\*/);
    assert.match(rewritten, /run \$bs-proof now/);
    assert.match(rewritten, /use \$superpowers:writing-plans for plans/);
    assert.doesNotMatch(rewritten, /\/bs-linear-plan/);
    assert.doesNotMatch(rewritten, /\/boss-finalize/);
  });

  it('leaves paths, URLs, redirects, and or-constructs untouched', () => {
    const rewritten = rewriteClaudeSkillMarkdown(`---
name: example
description: example description
---

Edit /Users/dave/x and docs/plans/a.md, fetch https://proof.bossanova.dev/x,
run \`gh\`/network checks, redirect 2>/dev/null, press [y/enter], pick and/or.
Score is /20 then /5 out of 5.
`);

    assert.match(rewritten, /\/Users\/dave\/x/);
    assert.match(rewritten, /docs\/plans\/a\.md/);
    assert.match(rewritten, /https:\/\/proof\.bossanova\.dev\/x/);
    assert.match(rewritten, /`gh`\/network checks/);
    assert.match(rewritten, /2>\/dev\/null/);
    assert.match(rewritten, /\[y\/enter\]/);
    assert.match(rewritten, /and\/or/);
    assert.match(rewritten, /Score is \/20 then \/5 out of 5/);
    assert.doesNotMatch(rewritten, /\$Users/);
    assert.doesNotMatch(rewritten, /\$network/);
    assert.doesNotMatch(rewritten, /\$dev/);
    assert.doesNotMatch(rewritten, /\$20/);
  });

  it('check mode reports stale generated output without changing it', () => {
    const root = tmpDir();
    const sourceRoot = path.join(root, '.claude', 'skills');
    const destRoot = path.join(root, '.codex', 'skills');

    writeFile(path.join(sourceRoot, 'current', 'SKILL.md'), skillMarkdown('current'));
    writeFile(path.join(destRoot, 'current', 'SKILL.md'), 'stale\n');

    const result = syncCodexSkills({ check: true, destRoot, sourceRoot });

    assert.equal(result.changed, true);
    assert.notEqual(result.differences.length, 0);
    assert.equal(fs.readFileSync(path.join(destRoot, 'current', 'SKILL.md'), 'utf8'), 'stale\n');
  });

  it('check mode skips public mirror checkouts without Claude skill sources', () => {
    const root = tmpDir();
    const destRoot = path.join(root, '.codex', 'skills');

    writeFile(path.join(root, 'Makefile'), 'test:\n\t@echo test\n');
    writeFile(path.join(destRoot, 'current', 'SKILL.md'), 'generated\n');

    const result = syncCodexSkills({
      check: true,
      destRoot,
      sourceRoot: path.join(root, '.claude', 'skills'),
    });

    assert.equal(result.changed, false);
    assert.equal(result.skipped, true);

    const output = execFileSync(process.execPath, [scriptPath, '--root', root, '--check'], {
      cwd: root,
      encoding: 'utf8',
    });

    assert.match(output, /Skipped Codex skills check/);
  });

  it('compares executable mode differences', () => {
    const root = tmpDir();
    const expected = path.join(root, 'expected');
    const actual = path.join(root, 'actual');

    writeFile(path.join(expected, 'run.sh'), '#!/bin/sh\n', 0o755);
    writeFile(path.join(actual, 'run.sh'), '#!/bin/sh\n', 0o644);

    assert.deepEqual(compareDirectories(expected, actual).includes('mode mismatch: run.sh'), true);
  });
});
