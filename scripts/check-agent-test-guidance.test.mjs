#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'

import { assertExactSize, measureFile } from './size-ratchet-lib.mjs'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')
const requiredFiles = [
  'AGENTS.md',
  'CLAUDE.md',
  'docs/testing/agent-fast-tests.md',
  '.claude/skills/agent-fast-testing/SKILL.md',
]

// CLAUDE.md is loaded into every session, so its length is a standing cost paid by every
// run. This is an EXACT pin, not a ceiling: it reds when CLAUDE.md grows AND when it shrinks,
// because a shrink-only ceiling silently converts every trim into headroom the file can regrow
// into. To move it in either direction, do so deliberately in the same commit as the change and
// record the reason on this line. BOS-636 set it at 150.
// Raised to 174 for 6371e6bf3 ("docs(skills): authorise protocol-mandated subagent dispatch
// in this repo"), which added the 24-line standing subagent-dispatch grant. That grant only
// works if it is in every session's context, so it cannot be moved behind a link. It landed
// on main without this bookkeeping because `scripts` is `branches-ignore: main` on push and
// the commit went in without a PR — the breach first surfaced on the next PR to touch a
// path in the workflow's filter. BOS-882 removes the section; drop this back to 150 then.
// Raised to 176 for BOS-783, which added two "Commands whose result lies" bullets: fish's
// glob-abort on an unquoted option value (reads as zero hits, is not zero hits) and the
// unscoped `grep -r` context blowup under `services/docs`. Both are one-line entries in an
// existing section, and both describe a command whose result lies — the section every session
// must carry for the same reason it already carries its neighbours.
// BOS-768 converted the comparison from `<=` to exact equality without moving the number: 176
// was already the measured line count, so the conversion changed the gate's reach, not its pin.
// Raised to 178 for BOS-763, which added two one-line "Commands whose result lies" bullets for
// incidental go.work.sum churn and gofmt-after-scripted-Go-edits drift.
// Raised to 177 for BOS-771, which added one "Commands whose result lies" bullet: a clean
// git rebase exit proves textual mergeability only and must be followed by post-rebase gates.
// Rebased together, those additive bullets make the measured count 179.
const CLAUDE_MD_MAX_LINES = 179

test('agent guidance points to the generated test command manifest', () => {
  for (const file of requiredFiles) {
    const text = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    assert.match(text, /docs\/testing\/test-command-manifest\.md/, file)
    assert.match(text, /make test-smoke/, file)
    assert.match(text, /make test-affected/, file)
  }
})

test('CLAUDE.md is exactly its pinned line count', () => {
  assertExactSize({
    constFile: 'scripts/check-agent-test-guidance.test.mjs',
    constName: 'CLAUDE_MD_MAX_LINES',
    expected: CLAUDE_MD_MAX_LINES,
    label: 'CLAUDE.md',
    measured: measureFile(path.join(repoRoot, 'CLAUDE.md'), { unit: 'lines' }),
    path: 'CLAUDE.md',
    remedy: 'Move situational detail into docs/ and leave a pointer, rather than raising the pin.',
    residual:
      'line LENGTH — a 176-line CLAUDE.md whose lines doubled in width costs twice as much ' +
      'context and passes this check unchanged',
    unit: 'lines',
  })
})
