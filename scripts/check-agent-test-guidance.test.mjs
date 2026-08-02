#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')
const requiredFiles = [
  'AGENTS.md',
  'CLAUDE.md',
  'docs/testing/agent-fast-tests.md',
  '.claude/skills/agent-fast-testing/SKILL.md',
]

// CLAUDE.md is loaded into every session, so its length is a standing cost paid by every
// run. This is a shrink-only ceiling, not a target: to raise it, do so deliberately in the
// same commit as the growth and record the reason on this line. BOS-636 set it at 150.
const CLAUDE_MD_MAX_LINES = 150

test('agent guidance points to the generated test command manifest', () => {
  for (const file of requiredFiles) {
    const text = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    assert.match(text, /docs\/testing\/test-command-manifest\.md/, file)
    assert.match(text, /make test-smoke/, file)
    assert.match(text, /make test-affected/, file)
  }
})

test('CLAUDE.md stays within its line ceiling', () => {
  const text = fs.readFileSync(path.join(repoRoot, 'CLAUDE.md'), 'utf8')
  const lines = text.split('\n').length - (text.endsWith('\n') ? 1 : 0)
  assert.ok(
    lines <= CLAUDE_MD_MAX_LINES,
    `CLAUDE.md is ${lines} lines, over the ${CLAUDE_MD_MAX_LINES}-line ceiling`,
  )
})
