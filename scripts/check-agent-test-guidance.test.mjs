#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')

test('AGENTS.md is a symlink to CLAUDE.md, so one edit reaches both agents', () => {
  const stat = fs.lstatSync(path.join(repoRoot, 'AGENTS.md'))
  assert.ok(stat.isSymbolicLink(), 'AGENTS.md must be a symlink, not a copy')
  assert.equal(fs.readlinkSync(path.join(repoRoot, 'AGENTS.md')), 'CLAUDE.md')
})
