#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import {
  fileHasInlineStopHook,
  findInlineStopHookCopies,
  findInlineStopHookCopiesInRepo,
  isInvokedDirectly,
} from './check-no-inline-stop-hooks.mjs'

const INLINE = 'return matcher.startsWith("bossd-agent-run-");'
const MIGRATED = 'Run: `node scripts/remove-bossd-stop-hooks.mjs` before stopping.'

test('fileHasInlineStopHook detects the inline filter signature', () => {
  assert.equal(fileHasInlineStopHook(INLINE), true)
  assert.equal(fileHasInlineStopHook("startsWith('bossd-agent-run-')"), true)
})

test('fileHasInlineStopHook detects harmless whitespace variants', () => {
  assert.equal(fileHasInlineStopHook('matcher.startsWith( "bossd-agent-run-" )'), true)
  assert.equal(fileHasInlineStopHook('matcher.startsWith ("bossd-agent-run-")'), true)
  assert.equal(fileHasInlineStopHook('matcher.startsWith\n("bossd-agent-run-")'), true)
  assert.equal(
    fileHasInlineStopHook(`matcher.startsWith(
      "bossd-agent-run-"
    )`),
    true,
  )
})

test('fileHasInlineStopHook ignores migrated prose that names the script', () => {
  assert.equal(fileHasInlineStopHook(MIGRATED), false)
})

test('findInlineStopHookCopies returns offending SKILL.md paths', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'guard-'))
  try {
    fs.mkdirSync(path.join(dir, 'bad'))
    fs.mkdirSync(path.join(dir, 'good'))
    fs.writeFileSync(path.join(dir, 'bad', 'SKILL.md'), `prefix\n${INLINE}\nsuffix`)
    fs.writeFileSync(path.join(dir, 'good', 'SKILL.md'), MIGRATED)
    const offenders = findInlineStopHookCopies(dir)
    assert.equal(offenders.length, 1)
    assert.match(offenders[0], /bad\/SKILL\.md$/)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('repo scan includes canonical and plugin skill roots', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'guard-repo-'))
  try {
    for (const root of [
      path.join(repoRoot, '.claude', 'skills', 'ok'),
      path.join(repoRoot, '.codex', 'skills', 'ok'),
      path.join(repoRoot, 'plugins', 'bossd-plugin-claude', 'skilldata', 'skills', 'plugin-bad'),
      path.join(
        repoRoot,
        'services',
        'boss',
        'internal',
        'skillinstall',
        'skills',
        'canonical-bad',
      ),
    ]) {
      fs.mkdirSync(root, { recursive: true })
      fs.writeFileSync(path.join(root, 'SKILL.md'), MIGRATED)
    }

    const pluginSkill = path.join(
      repoRoot,
      'plugins',
      'bossd-plugin-claude',
      'skilldata',
      'skills',
      'plugin-bad',
      'SKILL.md',
    )
    const canonicalSkill = path.join(
      repoRoot,
      'services',
      'boss',
      'internal',
      'skillinstall',
      'skills',
      'canonical-bad',
      'SKILL.md',
    )
    fs.writeFileSync(pluginSkill, INLINE)
    fs.writeFileSync(canonicalSkill, INLINE)

    const offenders = findInlineStopHookCopiesInRepo(repoRoot).map((file) =>
      path.relative(repoRoot, file),
    )

    assert.deepEqual(offenders.sort(), [
      path.join('plugins', 'bossd-plugin-claude', 'skilldata', 'skills', 'plugin-bad', 'SKILL.md'),
      path.join(
        'services',
        'boss',
        'internal',
        'skillinstall',
        'skills',
        'canonical-bad',
        'SKILL.md',
      ),
    ])
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

test('findInlineStopHookCopies returns empty on a clean tree', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'guard-'))
  try {
    fs.mkdirSync(path.join(dir, 'ok'))
    fs.writeFileSync(path.join(dir, 'ok', 'SKILL.md'), MIGRATED)
    assert.deepEqual(findInlineStopHookCopies(dir), [])
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('isInvokedDirectly returns false when argv path is absent', () => {
  const missing = path.join(os.tmpdir(), `missing-entrypoint-${process.pid}`)
  assert.equal(
    isInvokedDirectly(missing, import.meta.url, {
      fs: {
        existsSync: () => false,
        realpathSync: () => {
          throw new Error('realpathSync should not be called for missing argv path')
        },
      },
    }),
    false,
  )
})
