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
// Raised to 174 for 6371e6bf3 ("docs(skills): authorise protocol-mandated subagent dispatch
// in this repo"), which added the 24-line standing subagent-dispatch grant. That grant only
// works if it is in every session's context, so it cannot be moved behind a link. It landed
// on main without this bookkeeping because `scripts` is `branches-ignore: main` on push and
// the commit went in without a PR — the breach first surfaced on the next PR to touch a
// path in the workflow's filter. BOS-882 removes the section; drop this back to 150 then.
const CLAUDE_MD_MAX_LINES = 174

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
