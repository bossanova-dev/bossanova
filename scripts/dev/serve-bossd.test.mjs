import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import { execFileSync } from 'node:child_process'

import {
  DEV_AUTH_TOKEN,
  buildDevBossdEnv,
  discoverBuiltAgentPlugins,
  resolveBossdSocketPath,
  resolveGitHubToken,
  resolveGitIdentity,
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

test('git in the dev bossd env answers github.com credentials from the developer token, not a prompt', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'serve-bossd-git-'))
  try {
    const env = buildDevBossdEnv({
      homeDir: tmp,
      settingsPath: path.join(tmp, 'settings.json'),
      bossoPort: 8081,
      githubToken: 'ghp_devtoken',
      baseEnv: { PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: '1' },
    })
    assert.equal(env.GIT_TERMINAL_PROMPT, '0')
    assert.equal(env.GH_TOKEN, 'ghp_devtoken')
    const filled = execFileSync('git', ['credential', 'fill'], {
      env,
      input: 'protocol=https\nhost=github.com\n\n',
      encoding: 'utf8',
    })
    assert.match(filled, /^username=x-access-token$/m)
    assert.match(filled, /^password=ghp_devtoken$/m)
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('buildDevBossdEnv keeps config entries the caller already set', () => {
  const env = buildDevBossdEnv({
    homeDir: '/tmp/home',
    settingsPath: '/tmp/settings.json',
    bossoPort: 8081,
    githubToken: 'tok',
    baseEnv: { GIT_CONFIG_COUNT: '1', GIT_CONFIG_KEY_0: 'core.pager', GIT_CONFIG_VALUE_0: 'cat' },
  })
  assert.equal(env.GIT_CONFIG_COUNT, '3')
  assert.equal(env.GIT_CONFIG_KEY_0, 'core.pager')
  assert.equal(env.GIT_CONFIG_KEY_1, 'credential.https://github.com.helper')
  assert.equal(env.GIT_CONFIG_VALUE_1, '')
})

test('buildDevBossdEnv adds no GitHub credentials without a token', () => {
  const env = buildDevBossdEnv({
    homeDir: '/tmp/home',
    settingsPath: '/tmp/settings.json',
    bossoPort: 8081,
    baseEnv: {},
  })
  assert.equal(env.GH_TOKEN, undefined)
  assert.equal(env.GIT_CONFIG_COUNT, undefined)
  assert.equal(env.GIT_TERMINAL_PROMPT, '0')
})

test('resolveGitHubToken prefers an exported token, else asks gh, else gives null', () => {
  assert.equal(resolveGitHubToken({ GH_TOKEN: ' exported ' }, () => 'unused'), 'exported')
  assert.equal(resolveGitHubToken({}, () => 'from-gh\n'), 'from-gh')
  assert.equal(
    resolveGitHubToken({}, () => {
      throw new Error('gh: not logged in')
    }),
    null,
  )
})

test('a commit in the dev stack carries the developer identity and is not signed with their unreachable key', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'serve-bossd-commit-'))
  try {
    const devHome = path.join(tmp, 'dev')
    const isolated = path.join(tmp, 'isolated')
    const repo = path.join(tmp, 'repo')
    for (const dir of [devHome, isolated, repo]) {
      fs.mkdirSync(dir)
    }
    fs.writeFileSync(
      path.join(devHome, '.gitconfig'),
      '[user]\n\tname = Dev Person\n\temail = dev@example.com\n\tsigningkey = DEADBEEF\n[commit]\n\tgpgsign = true\n',
    )
    const devEnv = { PATH: process.env.PATH, HOME: devHome, GIT_CONFIG_NOSYSTEM: '1' }
    const identity = resolveGitIdentity(devEnv)
    assert.deepEqual(identity, { name: 'Dev Person', email: 'dev@example.com' })

    const env = buildDevBossdEnv({
      homeDir: isolated,
      settingsPath: path.join(isolated, 'settings.json'),
      bossoPort: 8081,
      gitIdentity: identity,
      // The host's own config stays out; only the dev env's settings decide signing.
      baseEnv: { PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null' },
    })
    execFileSync('git', ['init', '-q', repo], { env })
    execFileSync('git', ['-C', repo, 'commit', '--allow-empty', '-q', '-m', 'draft'], { env })
    const author = execFileSync('git', ['-C', repo, 'log', '-1', '--format=%an <%ae>|%G?'], {
      env,
      encoding: 'utf8',
    }).trim()
    assert.equal(author, 'Dev Person <dev@example.com>|N')
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('resolveGitIdentity gives null without both a name and an email', () => {
  assert.equal(resolveGitIdentity({}, () => ''), null)
  assert.deepEqual(
    resolveGitIdentity({}, (_cmd, args) => (args.includes('user.name') ? 'Dev\n' : 'dev@x\n')),
    { name: 'Dev', email: 'dev@x' },
  )
})
