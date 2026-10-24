#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { classifyCronBranch, defaultDBPath } from './prune-cron-branches.mjs'

test('default database paths follow the daemon platform config directory', () => {
  assert.equal(
    defaultDBPath({ os: 'linux', home: '/home/test', env: { XDG_CONFIG_HOME: '/custom config' } }),
    '/custom config/bossanova/bossd.db',
  )
  for (const env of [{}, { XDG_CONFIG_HOME: '' }]) {
    assert.equal(
      defaultDBPath({ os: 'linux', home: '/home/test', env }),
      '/home/test/.config/bossanova/bossd.db',
    )
  }
  assert.equal(
    defaultDBPath({ os: 'darwin', home: '/Users/test', env: { XDG_CONFIG_HOME: '/custom' } }),
    '/Users/test/Library/Application Support/bossanova/bossd.db',
  )
  assert.throws(
    () => defaultDBPath({ os: 'linux', home: '/home/test', env: { XDG_CONFIG_HOME: 'relative' } }),
    /XDG_CONFIG_HOME must be absolute/,
  )
})

test('Linux live-session guard reads XDG_CONFIG_HOME instead of a stale home database', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'prune-cron-xdg-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const home = join(dir, 'home')
  const xdg = join(dir, 'custom config')
  const bin = join(dir, 'bin')
  for (const parent of [join(home, '.config', 'bossanova'), join(xdg, 'bossanova'), bin]) {
    mkdirSync(parent, { recursive: true })
  }
  // sqlite3's boundary returns the session branches stored in the selected fixture.
  writeFileSync(join(home, '.config', 'bossanova', 'bossd.db'), '')
  writeFileSync(join(xdg, 'bossanova', 'bossd.db'), 'cron-live\n')
  writeFileSync(
    join(bin, 'sqlite3'),
    `#!${process.execPath}\nprocess.stdout.write(require('node:fs').readFileSync(process.argv[2]))\n`,
    { mode: 0o755 },
  )
  const git = (...args) => execFileSync('git', args, { cwd: dir, encoding: 'utf8' }).trim()
  git('init', '--initial-branch=main', '--quiet')
  git('config', 'commit.gpgsign', 'false')
  git(
    '-c',
    'user.name=Test',
    '-c',
    'user.email=test@example.com',
    'commit',
    '--allow-empty',
    '-qm',
    'init',
  )
  git('branch', 'cron-live')
  const preload = join(dir, 'linux.mjs')
  writeFileSync(
    preload,
    "import os from 'node:os'; import { syncBuiltinESMExports } from 'node:module'; os.platform = () => 'linux'; syncBuiltinESMExports();\n",
  )
  const run = (args = [], env = {}) =>
    execFileSync(
      process.execPath,
      [
        '--import',
        preload,
        fileURLToPath(new URL('./prune-cron-branches.mjs', import.meta.url)),
        '--apply',
        '--no-gh',
        '--base',
        'main',
        ...args,
      ],
      {
        cwd: dir,
        encoding: 'utf8',
        env: {
          ...process.env,
          HOME: home,
          XDG_CONFIG_HOME: xdg,
          PATH: `${bin}:${process.env.PATH}`,
          ...env,
        },
      },
    )
  assert.match(run(), /KEEP\s+cron-live.*live bossd session/)
  assert.match(
    run(['--db', join(xdg, 'bossanova', 'bossd.db')], { XDG_CONFIG_HOME: 'relative' }),
    /KEEP\s+cron-live.*live bossd session/,
  )
  assert.equal(git('branch', '--list', 'cron-live'), 'cron-live')
})

test('keeps a branch with a live bossd session even if merged', () => {
  const r = classifyCronBranch({ hasLiveSession: true, prState: 'MERGED', mergedIntoBase: true })
  assert.equal(r.action, 'keep')
})

test('keeps a branch with an open PR', () => {
  const r = classifyCronBranch({ hasLiveSession: false, prState: 'OPEN', mergedIntoBase: false })
  assert.equal(r.action, 'keep')
})

test('deletes a branch whose PR is merged', () => {
  const r = classifyCronBranch({ hasLiveSession: false, prState: 'MERGED', mergedIntoBase: false })
  assert.equal(r.action, 'delete')
})

test('deletes a branch whose tip is merged into base (no PR)', () => {
  const r = classifyCronBranch({ hasLiveSession: false, prState: 'NONE', mergedIntoBase: true })
  assert.equal(r.action, 'delete')
})

test('flags a closed-unmerged PR for review, never auto-deletes', () => {
  const r = classifyCronBranch({ hasLiveSession: false, prState: 'CLOSED', mergedIntoBase: false })
  assert.equal(r.action, 'review')
})

test('flags an orphan with unmerged commits and no PR for review', () => {
  const r = classifyCronBranch({ hasLiveSession: false, prState: 'NONE', mergedIntoBase: false })
  assert.equal(r.action, 'review')
})

test('unknown PR state with unmerged commits is reviewed, not deleted', () => {
  const r = classifyCronBranch({
    hasLiveSession: false,
    prState: 'UNKNOWN',
    mergedIntoBase: false,
  })
  assert.equal(r.action, 'review')
})

test('live-session guard wins over an open PR (kept regardless)', () => {
  const r = classifyCronBranch({ hasLiveSession: true, prState: 'OPEN', mergedIntoBase: false })
  assert.equal(r.action, 'keep')
})
