import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import {
  DEV_AUTH_TOKEN,
  buildDevBossdEnv,
  discoverBuiltAgentPlugins,
  resolveBossdSocketPath,
  resolveRepoDisplayName,
} from './serve-bossd.mjs'

test('discoverBuiltAgentPlugins finds built local agent runners and ignores cross-platform bins', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'serve-bossd-plugins-'))
  try {
    const binDir = path.join(tmp, 'bin')
    fs.mkdirSync(binDir, { recursive: true })
    fs.writeFileSync(path.join(binDir, 'bossd-plugin-claude'), '')
    fs.writeFileSync(path.join(binDir, 'bossd-plugin-claude-linux-amd64'), '')
    fs.writeFileSync(path.join(binDir, 'bossd-plugin-stub-runner'), '')
    fs.writeFileSync(path.join(binDir, 'bossd-plugin-linear'), '')

    assert.deepEqual(discoverBuiltAgentPlugins(tmp), [
      { name: 'claude', path: path.join(binDir, 'bossd-plugin-claude'), enabled: true },
      { name: 'stub', path: path.join(binDir, 'bossd-plugin-stub-runner'), enabled: true },
    ])
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('buildDevBossdEnv isolates HOME/keyring and targets local bosso with dev JWT', () => {
  const env = buildDevBossdEnv({
    homeDir: '/tmp/home',
    settingsPath: '/tmp/settings.json',
    bossoPort: 8081,
    baseEnv: { PATH: '/usr/bin', BOSSD_ORCHESTRATOR_URL: 'https://prod.example' },
  })
  assert.equal(env.HOME, '/tmp/home')
  assert.equal(env.BOSS_KEYRING_BACKEND, 'file')
  assert.equal(env.BOSS_SETTINGS_PATH, '/tmp/settings.json')
  assert.equal(env.BOSSD_ORCHESTRATOR_URL, 'http://127.0.0.1:8081')
  assert.equal(env.BOSSD_USER_JWT, DEV_AUTH_TOKEN)
  assert.equal(env.BOSS_REFUSE_DEFAULT_SETTINGS, '1')
})

test('resolveBossdSocketPath uses configDir when short and falls back when path is too long', () => {
  assert.equal(
    resolveBossdSocketPath('/tmp/wt/.config/serve', '/tmp/s'),
    '/tmp/wt/.config/serve/bossd.sock',
  )
  const longDir = `/Users/dave/.bossanova/worktrees/bossanova/${'a'.repeat(70)}/.config/serve`
  assert.equal(resolveBossdSocketPath(longDir, '/tmp/s'), '/tmp/s/bossd.sock')
})

test('resolveRepoDisplayName falls back to directory basename when not a git repo', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'serve-bossd-repo-'))
  try {
    const repoDir = path.join(tmp, 'my-project')
    fs.mkdirSync(repoDir, { recursive: true })
    assert.equal(resolveRepoDisplayName(repoDir), 'my-project')
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true, maxRetries: 3 })
  }
})
