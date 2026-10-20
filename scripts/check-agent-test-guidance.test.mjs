#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')

// The gate: AGENTS.md under `root` must be a symlink to CLAUDE.md, never a copy.
function assertAgentsSymlinksClaude(root) {
  const stat = fs.lstatSync(path.join(root, 'AGENTS.md'))
  assert.ok(stat.isSymbolicLink(), 'AGENTS.md must be a symlink, not a copy')
  assert.equal(fs.readlinkSync(path.join(root, 'AGENTS.md')), 'CLAUDE.md')
}

test('AGENTS.md is a symlink to CLAUDE.md, so one edit reaches both agents', () => {
  assertAgentsSymlinksClaude(repoRoot)
})

test('a copied AGENTS.md fails the symlink gate', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'agent-test-guidance-'))
  try {
    fs.writeFileSync(path.join(root, 'CLAUDE.md'), '# Project instructions\n')
    fs.copyFileSync(path.join(root, 'CLAUDE.md'), path.join(root, 'AGENTS.md'))
    assert.throws(
      () => assertAgentsSymlinksClaude(root),
      /AGENTS\.md must be a symlink, not a copy/,
    )
  } finally {
    fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})
