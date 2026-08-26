#!/usr/bin/env node

import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'

import { assertNonVacuousScan, readTrackedText, textLines, trackedFiles } from './tree-scan-lib.mjs'

test('trackedFiles parses git ls-files -z output without newline splitting', () => {
  const files = trackedFiles('/repo', {
    execFile: (cmd, args, options) => {
      assert.equal(cmd, 'git')
      assert.deepEqual(args, ['ls-files', '-z'])
      assert.equal(options.cwd, '/repo')
      return Buffer.from('alpha.js\0space name.md\0')
    },
  })

  assert.deepEqual(files, ['alpha.js', 'space name.md'])
})

test('readTrackedText reads symlink target text and regular file content', (t) => {
  const root = mkdtempSync(join(tmpdir(), 'tree-scan-lib-'))
  t.after(() => rmSync(root, { recursive: true, force: true }))

  mkdirSync(join(root, 'target-dir'))
  writeFileSync(join(root, 'target-dir', 'clean.md'), 'target content\n')
  symlinkSync(join('target-dir', 'clean.md'), join(root, 'link.md'))
  writeFileSync(join(root, 'plain.md'), 'plain content\n')

  assert.equal(readTrackedText('link.md', root), join('target-dir', 'clean.md'))
  assert.equal(readTrackedText('plain.md', root), 'plain content\n')
})

test('textLines returns null for binary-looking files and binary bytes', (t) => {
  const root = mkdtempSync(join(tmpdir(), 'tree-scan-lib-'))
  t.after(() => rmSync(root, { recursive: true, force: true }))

  writeFileSync(join(root, 'image.png'), 'not relevant')
  writeFileSync(join(root, 'blob.txt'), Buffer.from([0x61, 0x00, 0x62]))

  assert.equal(textLines('image.png', root), null)
  assert.equal(textLines('blob.txt', root), null)
})

test('assertNonVacuousScan delegates empty-set failures to assertArtifactSet', () => {
  assert.throws(
    () => assertNonVacuousScan([], 1, 'tracked tree'),
    /size-ratchet: tracked tree is empty, so every assertion in the loop over it runs zero times/,
  )
})
