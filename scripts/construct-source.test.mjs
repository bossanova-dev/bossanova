import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { resolveSkillsRoot, skillsRootAvailable } from './construct-skills.mjs';

const root = fileURLToPath(new URL('..', import.meta.url));
const manifest = JSON.parse(
  fs.readFileSync(path.join(root, '.claude/skills/bs-linear-implement/construct.json'), 'utf8'),
);

test('resolveSkillsRoot honors the SUPERPOWERS_SKILLS_DIR override', () => {
  const dir = resolveSkillsRoot(manifest, { SUPERPOWERS_SKILLS_DIR: '/tmp/sp' });
  assert.equal(dir, '/tmp/sp');
});

test('resolveSkillsRoot defaults to the pinned plugin-cache path', () => {
  const dir = resolveSkillsRoot(manifest, {});
  assert.equal(
    dir,
    path.join(
      os.homedir(),
      '.claude/plugins/cache/claude-plugins-official/superpowers',
      manifest.superpowers_version,
      'skills',
    ),
  );
});

test(
  'the installed superpowers source provides the real dispatcher + reviewer text',
  {
    skip:
      !skillsRootAvailable(manifest) && `superpowers ${manifest.superpowers_version} not installed`,
  },
  () => {
    const skills = resolveSkillsRoot(manifest);
    const sdd = fs.readFileSync(path.join(skills, 'subagent-driven-development/SKILL.md'), 'utf8');
    assert.match(sdd, /fresh implementer subagent per task/);
    const reviewer = fs.readFileSync(
      path.join(skills, 'requesting-code-review/code-reviewer.md'),
      'utf8',
    );
    assert.match(reviewer, /Senior Code Reviewer/);
  },
);

test('no vendored copy of the component skills is committed to the repo', () => {
  assert.equal(fs.existsSync(path.join(root, '.claude/skills/_construct')), false);
});
