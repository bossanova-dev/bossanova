import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
import {
  captureManifest,
  createPin,
  pinDir,
  resolveInstalledSkill,
  verifyPin,
} from './skill-pin.mjs'

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'skill-pin-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  const home = path.join(root, 'home')
  const skillsHome = path.join(home, '.claude', 'skills')
  const source = path.join(skillsHome, 'boss-build')
  const gitDir = path.join(root, 'git')
  fs.mkdirSync(path.join(source, 'toolbox'), { recursive: true })
  fs.writeFileSync(path.join(source, 'SKILL.md'), 'current instructions')
  fs.writeFileSync(path.join(source, 'toolbox', 'helper.mjs'), 'current helper')
  fs.writeFileSync(path.join(source, 'toolbox', 'run.sh'), '#!/bin/sh\nexit 0\n', { mode: 0o755 })
  return { root, home, skillsHome, source, gitDir, skill: 'boss-build', env: {} }
}

test('a read-only pin survives a peer install and records payload provenance', async (t) => {
  const f = fixture(t)
  fs.writeFileSync(path.join(f.skillsHome, '.bossanova-skills.payload.json'), '{"revision":"abc"}')
  fs.symlinkSync('helper.mjs', path.join(f.source, 'toolbox', 'linked.mjs'))
  const result = await createPin(f)
  assert.equal(result.skillDir, pinDir(f.gitDir, f.skill, 'local'))
  assert.deepEqual(result.payloadRecord, { revision: 'abc' })
  const before = captureManifest(result.skillDir)
  assert.equal(fs.statSync(path.join(result.toolbox, 'helper.mjs')).mode & 0o777, 0o444)
  assert.equal(fs.statSync(path.join(result.toolbox, 'run.sh')).mode & 0o777, 0o555)
  assert.equal(fs.lstatSync(path.join(result.toolbox, 'linked.mjs')).isSymbolicLink(), false)
  fs.rmSync(f.source, { recursive: true, maxRetries: 3 })
  fs.mkdirSync(f.source)
  fs.writeFileSync(path.join(f.source, 'SKILL.md'), 'older instructions')
  assert.deepEqual(captureManifest(result.skillDir), before)
  assert.equal(verifyPin(f).status, 0)
  assert.equal(fs.readFileSync(path.join(result.toolbox, 'helper.mjs'), 'utf8'), 'current helper')
})

test('capture retries after a mutation and rejects a source that never settles', async (t) => {
  const f = fixture(t)
  let captures = 0
  await createPin({
    ...f,
    onCaptured: () => {
      if (++captures === 1) fs.writeFileSync(path.join(f.source, 'SKILL.md'), 'updated')
    },
  })
  assert.equal(captures, 2)
  assert.equal(
    fs.readFileSync(path.join(pinDir(f.gitDir, f.skill, 'local'), 'SKILL.md'), 'utf8'),
    'updated',
  )
  await assert.rejects(
    createPin({
      ...f,
      onCaptured: () => {
        fs.appendFileSync(path.join(f.source, 'SKILL.md'), 'changing')
      },
    }),
    /kept changing/,
  )
  assert.equal(verifyPin(f).status, 0)
})

test('candidate resolution falls through an unrelated skills home and follows namespace links', (t) => {
  const f = fixture(t)
  const installed = path.join(f.skillsHome, 'namespace', f.skill)
  fs.mkdirSync(path.dirname(installed))
  fs.renameSync(f.source, installed)
  fs.symlinkSync(path.join('namespace', f.skill), f.source)
  assert.equal(
    resolveInstalledSkill({ ...f, env: { BOSS_SKILLS_HOME: f.root } }).source,
    fs.realpathSync(installed),
  )
})

test('missing capabilities and broken links are rejected without replacing a good pin', async (t) => {
  const f = fixture(t)
  await createPin(f)
  fs.symlinkSync('missing', path.join(f.source, 'broken'))
  await assert.rejects(createPin(f), /BLOCKED/)
  fs.unlinkSync(path.join(f.source, 'broken'))
  fs.rmSync(path.join(f.source, 'toolbox'), { recursive: true, maxRetries: 3 })
  await assert.rejects(createPin(f), /toolbox/)
  assert.equal(verifyPin(f).status, 0)
})

test('environment candidate takes precedence, falls through when absent, then uses Codex', (t) => {
  const f = fixture(t)
  const options = { skill: f.skill, home: f.home, env: { BOSS_SKILLS_HOME: f.root } }
  assert.equal(resolveInstalledSkill(options).source, fs.realpathSync(f.source))
  const alternate = path.join(f.root, f.skill)
  fs.mkdirSync(alternate)
  fs.writeFileSync(path.join(alternate, 'SKILL.md'), 'explicit')
  assert.equal(resolveInstalledSkill(options).source, fs.realpathSync(alternate))
  fs.rmSync(alternate, { recursive: true, maxRetries: 3 })
  const codexHome = path.join(f.home, '.codex', 'skills')
  fs.mkdirSync(codexHome, { recursive: true })
  fs.renameSync(f.source, path.join(codexHome, f.skill))
  assert.equal(
    resolveInstalledSkill(options).source,
    fs.realpathSync(path.join(codexHome, f.skill)),
  )
})

test('a new capture replaces an earlier pin, and verify detects tampering and missing pins', async (t) => {
  const f = fixture(t)
  assert.equal(verifyPin(f).status, 2)
  await createPin(f)
  fs.writeFileSync(path.join(f.source, 'SKILL.md'), 'new instructions')
  await createPin(f)
  const skillFile = path.join(pinDir(f.gitDir, f.skill, 'local'), 'SKILL.md')
  assert.equal(fs.readFileSync(skillFile, 'utf8'), 'new instructions')
  fs.chmodSync(skillFile, 0o644)
  fs.writeFileSync(skillFile, 'tampered')
  assert.equal(verifyPin(f).status, 1)
  assert.match(verifyPin(f).reason, /SKILL.md/)
})

test('CLI removes read-only pins, reports missing pins and refuses unsafe names', async (t) => {
  const f = fixture(t)
  await createPin(f)
  const cli = fileURLToPath(new URL('./skill-pin.mjs', import.meta.url))
  const run = (verb, skill = f.skill) =>
    spawnSync(process.execPath, [cli, verb, '--skill', skill, '--git-dir', f.gitDir], {
      encoding: 'utf8',
      env: { ...process.env, BOSS_AGENT_SESSION_ID: '' },
    })
  assert.equal(run('verify').status, 0)
  assert.equal(run('remove').status, 0)
  assert.equal(fs.existsSync(pinDir(f.gitDir, f.skill, 'local')), false)
  assert.equal(run('verify').status, 2)
  assert.equal(run('remove', '../escape').status, 1)
})

test('restrictive umask still publishes traversable pin directories', async (t) => {
  const f = fixture(t)
  fs.mkdirSync(path.join(f.source, 'toolbox', 'nested'))
  const previous = process.umask(0o077)
  try {
    const result = await createPin(f)
    assert.equal(
      fs.statSync(path.join(result.skillDir, '.boss-skill-pin.json')).mode & 0o777,
      0o444,
    )
    for (const directory of [
      result.skillDir,
      result.toolbox,
      path.join(result.toolbox, 'nested'),
    ]) {
      assert.equal(fs.statSync(directory).mode & 0o777, 0o755)
    }
  } finally {
    process.umask(previous)
  }
})

// Two runs of one core can share a worktree: a stale verify claim plus a callback's fresh run, or
// a repair that runs verify while the verify cron does. Each run must own its pin.
const chat = (id) => ({ BOSS_AGENT_SESSION_ID: id })

test("a run's remove leaves a concurrent run's pin in place", async (t) => {
  const f = fixture(t)
  const a = await createPin({ ...f, env: chat('chat-a') })
  const b = await createPin({ ...f, env: chat('chat-b') })
  const cli = fileURLToPath(new URL('./skill-pin.mjs', import.meta.url))
  const removed = spawnSync(
    process.execPath,
    [cli, 'remove', '--skill', f.skill, '--git-dir', f.gitDir],
    { encoding: 'utf8', env: { ...process.env, ...chat('chat-a') } },
  )
  assert.equal(removed.status, 0, removed.stderr)
  assert.equal(fs.existsSync(a.skillDir), false)
  assert.equal(fs.existsSync(path.join(b.skillDir, '.boss-skill-pin.json')), true)
  assert.equal(verifyPin({ ...f, env: chat('chat-b') }).status, 0)
})

test("a peer run's create never swaps the tree a running pin reads", async (t) => {
  const f = fixture(t)
  const a = await createPin({ ...f, env: chat('chat-a') })
  const inode = fs.statSync(a.skillDir).ino
  fs.writeFileSync(path.join(f.source, 'SKILL.md'), 'peer instructions')
  const b = await createPin({ ...f, env: chat('chat-b') })
  assert.equal(fs.statSync(a.skillDir).ino, inode)
  assert.equal(fs.readFileSync(path.join(a.skillDir, 'SKILL.md'), 'utf8'), 'current instructions')
  assert.equal(fs.readFileSync(path.join(b.skillDir, 'SKILL.md'), 'utf8'), 'peer instructions')
  assert.equal(verifyPin({ ...f, env: chat('chat-a') }).status, 0)
})

test('create sweeps only pins older than the TTL that a crashed run left behind', async (t) => {
  const f = fixture(t)
  const crashed = await createPin({ ...f, env: chat('chat-crashed') })
  const recent = await createPin({ ...f, env: chat('chat-recent') })
  const stale = new Date(Date.now() - 365 * 24 * 60 * 60 * 1000)
  fs.utimesSync(crashed.skillDir, stale, stale)
  await createPin({ ...f, env: chat('chat-new') })
  assert.equal(fs.existsSync(crashed.skillDir), false)
  assert.equal(verifyPin({ ...f, env: chat('chat-recent') }).status, 0)
  assert.equal(fs.existsSync(recent.skillDir), true)
})

test('a run id that is not a safe path segment is refused, and no id pins the local run', async (t) => {
  const f = fixture(t)
  await assert.rejects(createPin({ ...f, env: chat('../escape') }), /BLOCKED/)
  const local = await createPin({ ...f, env: {} })
  assert.equal(path.basename(local.skillDir), 'local')
  assert.equal(verifyPin({ ...f, env: {} }).status, 0)
  assert.equal(verifyPin({ ...f, env: chat('chat-other') }).status, 2)
})
