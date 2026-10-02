// Behaviour tests for the bs-sweep-notes cron gate.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'

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

// BOS-788: the gate must resolve the `boss` binary before shelling out, so a
// stale $BOSS_BIN (a cron session's context prompt naming an already-deleted
// sibling worktree) falls through to the next candidate instead of dying with an
// opaque ENOENT that a run reads as "capability unavailable".
//
// Each case injects BOSS_BIN and PATH and runs from a cwd outside this checkout,
// so no case depends on the machine's real `boss` install or on ./bin/boss
// happening to exist here. The gate is invoked by absolute path, so its own
// relative imports still resolve from import.meta.url.
const GATE_PATH = new URL('../.claude/skills/bs-sweep-notes/gate/gate.mjs', import.meta.url)
  .pathname
const HEALTHY_NOTES = '[{"id":"note-1","tags":["improvement"]}]'

function writeFakeBoss(dir, output = HEALTHY_NOTES) {
  const fake = join(dir, 'boss')
  writeFileSync(fake, `#!/bin/sh\nprintf '%s\\n' '${output}'\n`)
  chmodSync(fake, 0o755)
  return fake
}

// A cwd with no ancestor `.git`, so the resolver's ./bin/boss arm cannot reach
// this repo's build and every case is decided by BOSS_BIN and PATH alone.
function runGate({ bossBin, path: pathEntries, cwd }) {
  return spawnSync(process.execPath, [GATE_PATH], {
    cwd,
    env: { BOSS_BIN: bossBin, PATH: pathEntries },
    encoding: 'utf8',
  })
}

test('live gate succeeds when BOSS_BIN names a healthy boss binary', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-boss-ok-'))
  try {
    const result = runGate({ bossBin: writeFakeBoss(dir), path: '', cwd: dir })
    assert.equal(result.status, 0, result.stderr)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('live gate falls back to PATH when BOSS_BIN names a deleted worktree', () => {
  const pathDir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-boss-path-'))
  // The reported scenario: BOSS_BIN was exported by a sibling worktree that has
  // since been reaped, while a healthy `boss` is still reachable another way.
  const reaped = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-reaped-worktree-'))
  const stale = join(reaped, 'bin', 'boss')
  rmSync(reaped, { recursive: true, force: true })
  try {
    writeFakeBoss(pathDir)
    const result = runGate({ bossBin: stale, path: pathDir, cwd: pathDir })
    assert.equal(result.status, 0, result.stderr)
  } finally {
    rmSync(pathDir, { recursive: true, force: true })
  }
})

test('live gate reports the resolver reason when no boss binary resolves at all', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-boss-missing-'))
  const stale = join(dir, 'gone', 'bin', 'boss')
  try {
    const result = runGate({ bossBin: stale, path: '', cwd: dir })
    assert.notEqual(result.status, 0)
    // The resolver's own `reason`, not a bare spawn failure: a run that reads
    // ENOENT as "capability unavailable" is the bug this test pins.
    assert.match(result.stderr, /no\s+usable\s+boss\s+executable/)
    assert.ok(result.stderr.includes(stale), `reason must name BOSS_BIN: ${result.stderr}`)
    assert.doesNotMatch(result.stderr, /ENOENT|spawnSync/)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})
