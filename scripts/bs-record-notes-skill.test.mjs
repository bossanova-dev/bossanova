// Content contract for the repo-local bs-record-notes skill (BOS-604).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'

const here = (rel) => new URL(rel, import.meta.url)
const read = (rel) => readFileSync(here(rel), 'utf8')
const source = () => read('../.claude/skills/bs-record-notes/SKILL.md')
const codex = () => read('../.codex/skills/bs-record-notes/SKILL.md')

const NOTES_TEARDOWN =
  "Before exiting, follow `bs-record-notes` with this run's outcome. Recording is non-fatal: never change the terminal state, exit code, or `git status --porcelain`. Skip gated/no-op runs that observed nothing."

function withoutGeneratedHeader(skill) {
  return skill.replace(
    /<!-- Generated from \.claude\/skills by make codex-skills\. Do not edit directly\. -->\n\n/,
    '',
  )
}

test('declares bs-record-notes in both skill trees', () => {
  for (const [tree, skill] of [
    ['.claude', source()],
    ['.codex', codex()],
  ]) {
    assert.match(skill, /^name:\s*bs-record-notes\s*$/m, `${tree} must declare the skill name`)
    assert.match(skill, /^description:\s*.+/m, `${tree} must declare a description`)
  }
})

test('generated mirror differs only by its generated header', () => {
  assert.equal(withoutGeneratedHeader(codex()), source())
})

test('requires every repo-local sweep to carry the notes teardown contract', () => {
  const sweepNames = readdirSync(here('../.claude/skills/'), { withFileTypes: true })
    .filter((entry) => entry.isDirectory() && entry.name.startsWith('bs-sweep-'))
    .map((entry) => entry.name)
    .sort()

  assert.ok(sweepNames.length > 0, 'coverage guard requires at least one repo-local sweep')
  for (const name of sweepNames) {
    const skill = read(`../.claude/skills/${name}/SKILL.md`)
    assert.ok(skill.includes(NOTES_TEARDOWN), `${name} must include the notes teardown contract`)
  }
})

test('documents the bounded, safe notes-recording contract', () => {
  const skill = source()
  for (const literal of [
    '**Harvest.**',
    '**Split.**',
    '**Cap.**',
    '**Suppress.**',
    '**Write.**',
    '**Report.**',
    'boss notes add',
    '--tag improvement',
    'create_note',
    'resolve_context',
    'repo_id',
    'one note per issue',
    'BS_RECORD_NOTES_MAX',
    'default **5**',
    'Never a secret',
    'Never fabricate',
    'Never fatal',
    'Where:',
    'Why it matters:',
    'Suggested fix:',
    'Run:',
  ]) {
    assert.ok(skill.includes(literal), `skill must contain ${JSON.stringify(literal)}`)
  }
})
