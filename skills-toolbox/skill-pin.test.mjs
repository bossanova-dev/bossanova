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
  assert.equal(result.skillDir, pinDir(f.gitDir, f.skill))
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
  assert.equal(fs.readFileSync(path.join(pinDir(f.gitDir, f.skill), 'SKILL.md'), 'utf8'), 'updated')
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
  const skillFile = path.join(pinDir(f.gitDir, f.skill), 'SKILL.md')
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
    })
  assert.equal(run('verify').status, 0)
  assert.equal(run('remove').status, 0)
  assert.equal(fs.existsSync(pinDir(f.gitDir, f.skill)), false)
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
