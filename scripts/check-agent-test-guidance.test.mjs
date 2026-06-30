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

test('agent guidance points to the generated test command manifest', () => {
  for (const file of requiredFiles) {
    const text = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    assert.match(text, /docs\/testing\/test-command-manifest\.md/, file)
    assert.match(text, /make test-smoke/, file)
    assert.match(text, /make test-affected/, file)
  }
})
