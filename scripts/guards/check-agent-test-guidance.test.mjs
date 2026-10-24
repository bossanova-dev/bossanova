#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '../..')

// The gate: CLAUDE.md under `root` must be a symlink to AGENTS.md, never a copy.
function assertClaudeSymlinksAgents(root) {
  assert.ok(fs.lstatSync(path.join(root, 'AGENTS.md')).isFile(), 'AGENTS.md must be a regular file')
  const stat = fs.lstatSync(path.join(root, 'CLAUDE.md'))
  assert.ok(stat.isSymbolicLink(), 'CLAUDE.md must be a symlink, not a copy')
  assert.equal(fs.readlinkSync(path.join(root, 'CLAUDE.md')), 'AGENTS.md')
}

test('CLAUDE.md is a symlink to AGENTS.md, so one edit reaches both agents', () => {
  assertClaudeSymlinksAgents(repoRoot)
})

test('a copied CLAUDE.md fails the symlink gate', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'agent-test-guidance-'))
  try {
    fs.writeFileSync(path.join(root, 'AGENTS.md'), '# Project instructions\n')
    fs.copyFileSync(path.join(root, 'AGENTS.md'), path.join(root, 'CLAUDE.md'))
    assert.throws(
      () => assertClaudeSymlinksAgents(root),
      /CLAUDE\.md must be a symlink, not a copy/,
    )
  } finally {
    fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})
