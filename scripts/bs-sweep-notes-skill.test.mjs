// Content/contract test for the repo-local bs-sweep-notes skill (BOS-607).
//
// The execution logic lives in the skill; this test pins the deployable
// contracts: both skill frontmatters, generated-mirror synchronization, the
// documented cron cadence branch, and identical fixture-capable gate probes.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')

const SKILL = read('../.claude/skills/bs-sweep-notes/SKILL.md')
const CODEX = read('../.codex/skills/bs-sweep-notes/SKILL.md')
const GATE = read('../.claude/skills/bs-sweep-notes/gate/gate.mjs')
const CODEX_GATE = read('../.codex/skills/bs-sweep-notes/gate/gate.mjs')

const CRON_NAME = 'Bossanova sweep notes'
const CLAUDE_GATE = 'node .claude/skills/bs-sweep-notes/gate/gate.mjs'
const CODEX_GATE_COMMAND = 'node .codex/skills/bs-sweep-notes/gate/gate.mjs'

function generatedMirror(source) {
  return source
    .replace(
      /^---\n([\s\S]*?)\n---\n/,
      '---\n$1\n---\n\n<!-- Generated from .claude/skills by make codex-skills. Do not edit directly. -->\n',
    )
    .replaceAll('.claude/skills/bs-sweep-notes', '.codex/skills/bs-sweep-notes')
}

test('both skill frontmatters name bs-sweep-notes and describe it', () => {
  for (const [label, skill] of [
    ['.claude', SKILL],
    ['.codex', CODEX],
  ]) {
    assert.match(skill, /^name:\s*bs-sweep-notes\s*$/m, `${label} name`)
    assert.match(skill, /^description:\s*.+/m, `${label} description`)
  }
})

test('generated Codex skill mirrors source with only the documented path rewrite', () => {
  assert.equal(CODEX, generatedMirror(SKILL))
})

test('complete argv resolution makes either explicit write-flag order write mode', () => {
  const match = SKILL.match(/Resolve the flags before reading anything:\n\n```bash\n([\s\S]*?)```/)
  assert.ok(match, 'source skill must contain an executable flag resolver')
  const resolve = (...args) =>
    spawnSync('bash', ['-c', `${match[1]}\nprintf '%s' "$WRITE"`, '--', ...args], {
      encoding: 'utf8',
    })

  assert.equal(resolve().stdout, 'false')
  assert.equal(resolve('--dry-run').stdout, 'false')
  assert.equal(resolve('--no-dry-run').stdout, 'true')
  assert.equal(resolve('--no-dry-run', '--dry-run').stdout, 'true')
  assert.equal(resolve('--dry-run', '--no-dry-run').stdout, 'true')
  assert.equal(resolve('--unexpected').status, 2)
})

test('snapshot validation rejects malformed notes and duplicate deletion IDs', () => {
  const match = SKILL.match(
    /boss notes ls --tag improvement --json --limit 0 >"\$RUN_DIR\/notes\.json"\nnode -e '([^']+)'/,
  )
  assert.ok(match, 'source skill must contain executable snapshot validation')
  const dir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-snapshot-'))
  let fixtureNumber = 0
  const run = (notes) => {
    const notesPath = join(dir, `${fixtureNumber++}.json`)
    const idsPath = join(dir, `${fixtureNumber++}.ids`)
    writeFileSync(notesPath, JSON.stringify(notes))
    return spawnSync(process.execPath, ['-e', match[1], notesPath, idsPath], {
      encoding: 'utf8',
    })
  }
  const valid = {
    id: 'note-1',
    body: 'A problem',
    created_at: '2026-07-29T00:00:00Z',
    tags: ['improvement'],
  }
  try {
    assert.equal(run([valid]).status, 0)
    assert.notEqual(run([{}]).status, 0)
    assert.notEqual(run([{ ...valid, id: '' }]).status, 0)
    assert.notEqual(run([{ ...valid, body: null }]).status, 0)
    assert.notEqual(run([{ ...valid, created_at: null }]).status, 0)
    assert.notEqual(run([{ ...valid, tags: ['other'] }]).status, 0)
    assert.notEqual(run([valid, { ...valid }]).status, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('weekly cron registration records the no-gate cadence branch and names the probe', () => {
  for (const [label, skill, command] of [
    ['.claude', SKILL, CLAUDE_GATE],
    ['.codex', CODEX, CODEX_GATE_COMMAND],
  ]) {
    assert.ok(skill.includes(CRON_NAME), `${label} must use one cron name`)
    assert.ok(skill.includes("--schedule '@weekly'"), `${label} must use exact weekly cadence`)
    assert.ok(
      skill.includes('/bs-sweep-notes --no-dry-run'),
      `${label} must enter write mode explicitly`,
    )
    assert.ok(skill.includes(command), `${label} must document its local gate probe`)
    assert.match(
      skill,
      /GateCommand.*empty|no-gate weekly cadence|without a gate/i,
      `${label} must state the no-gate branch actually selected`,
    )
  }
})

test('production triage pins Linear injection and acted-on deletion buckets', () => {
  assert.ok(SKILL.includes('const { linearRequest } = await import(process.env.LIB)'))
  assert.ok(SKILL.includes('fetchMarkedLinearIssues({'))
  assert.ok(SKILL.includes('node "$GATE" cluster "$NOTES_JSON" >"$CLUSTERS_JSON"'))
  assert.ok(SKILL.includes('node "$GATE" merge "$CLUSTERS_JSON" "$GROUPS_JSON" >"$MERGED_JSON"'))
  assert.ok(SKILL.includes('node "$GATE" select "$MERGED_JSON" "$LINEAR_JSON"'))
  assert.match(SKILL, /group semantically\s+near-duplicate problem statements/)
  assert.match(SKILL, /every\s+mechanical cluster key must appear exactly once/)
  assert.match(SKILL, /every initial `dropped` cluster whose reason is `already-tracked`/)
  assert.match(SKILL, /If dispatch itself errors, stop with no writes or deletions/)
  assert.doesNotMatch(SKILL, /dispatch.*inline fallback/i)
  assert.ok(
    SKILL.includes(
      '{ query: "Notes: <cluster-key>", team: "Bossanova", includeArchived: true, limit: 250 }',
    ),
  )
  assert.ok(SKILL.includes('`team: "Bossanova"`'))
  assert.ok(SKILL.includes('`state: "Unplanned"`'))
  assert.ok(SKILL.includes('`labels: ["agent-plan"]`'))
  assert.match(SKILL, /`title`: a non-empty, single-line problem statement/)
  assert.match(SKILL, /failed or ambiguous\s+create stops further writes/)
  assert.match(SKILL, /Never delete a deferred cluster/)
})

test('both gate mirrors carry the same cron name and fixture-capable fail-closed probe', () => {
  assert.equal(GATE, CODEX_GATE, 'gate files must be byte-identical')
  assert.ok(GATE.includes(`const CRON_NAME = '${CRON_NAME}'`), 'gate cron name matches SKILL')
  assert.ok(GATE.includes('BS_SWEEP_NOTES_FIXTURE'), 'gate supports deterministic fixture runs')
  assert.match(GATE, /fail-closed/i, 'gate documents fail-closed behavior')
})

test('fixture gate accepts only object entries with an improvement tag', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-gate-'))
  const gate = new URL('../.claude/skills/bs-sweep-notes/gate/gate.mjs', import.meta.url)
  let fixtureNumber = 0
  const run = (fixture) => {
    const path = join(dir, `${fixtureNumber++}.json`)
    writeFileSync(path, JSON.stringify(fixture))
    return spawnSync(process.execPath, [gate.pathname], {
      env: { ...process.env, BS_SWEEP_NOTES_FIXTURE: path },
      encoding: 'utf8',
    })
  }
  try {
    assert.equal(run({ notes: [{ id: 'note-1', tags: ['improvement'] }] }).status, 0)
    assert.equal(run([{ id: 'note-raw', tags: ['improvement'] }]).status, 0)
    assert.notEqual(run({ notes: [{ id: 'note-2', tags: ['other'] }] }).status, 0)
    assert.notEqual(run({ notes: [{ id: 'missing-tags' }] }).status, 0)
    assert.notEqual(run({ notes: [null] }).status, 0)
    assert.notEqual(run({ notes: [['not', 'a', 'note']] }).status, 0)
    assert.notEqual(run({ notes: [{ id: '', tags: ['improvement'] }] }).status, 0)
    assert.notEqual(
      run({
        notes: [
          { id: 'valid', tags: ['improvement'] },
          { id: 'malformed', tags: ['other'] },
        ],
      }).status,
      0,
    )
    assert.notEqual(run({ notes: 'not-an-array' }).status, 0)
    assert.notEqual(run(null).status, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('live gate fails closed when BOSS_BIN returns malformed note entries', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-live-gate-'))
  const gate = new URL('../.claude/skills/bs-sweep-notes/gate/gate.mjs', import.meta.url)
  const fakeBoss = join(dir, 'boss')
  const run = (output) => {
    writeFileSync(fakeBoss, `#!/bin/sh\nprintf '%s\\n' '${output}'\n`)
    chmodSync(fakeBoss, 0o755)
    return spawnSync(process.execPath, [gate.pathname], {
      env: { ...process.env, BOSS_BIN: fakeBoss },
      encoding: 'utf8',
    })
  }
  try {
    assert.notEqual(run('[null]').status, 0)
    assert.notEqual(run('[{}]').status, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})
