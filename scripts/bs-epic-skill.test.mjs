// Content/contract test for the boss-epic skill (BOS-177).
//
// boss-epic orchestrates an entire epic of planned Linear tickets to merged PRs,
// unattended: it assembles the epic's sub-issues, computes a dependency-ordered
// schedule, spawns parallel boss-implement sessions, drives repair on failures,
// serializes merges, and reports progress on the parent issue. This test follows
// the BOS-144 content-test pattern (scripts/bs-<skill>-skill.test.mjs, mirroring
// scripts/boss-plan-skill.test.mjs). It pins:
//   * the shared helper module + contract symbols the SKILL references,
//   * the merge-serialization and session-isolation safety statements,
//   * the never-mutate-outside-the-epic-set guarantee,
//   * the no-interactive-questions-after-preflight rule,
//   * frontmatter identity + default agent,
//   * a size-ratchet keeping SKILL.md below the committed baseline rounded up,
//   * codex-mirror parity (exact rewrite of the claude source),
//   * a NEGATIVE assertion against stub/placeholder prose.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { rewriteClaudeSkillMarkdown } from './sync-codex-skills.mjs'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')

const CLAUDE = read('../.claude/skills/boss-epic/SKILL.md')
const CODEX = read('../.codex/skills/boss-epic/SKILL.md')

test('size ratchet', () => {
  // Ratchet = committed size rounded up to the next KiB. Never raise this
  // casually — a growing SKILL.md erodes the headless context budget; split
  // situational sections into references/ (boss-plan-skill precedent) before
  // bumping. Bumped 19456 → 24576 for BOS-179, the "make boss-epic work
  // headlessly" ticket: Phase 3 was rewritten from live-chat steering to
  // headless detached dispatch (detach+model create_session, unattended
  // preamble, fresh /boss-repair watch runs, merge-time external re-check,
  // ./bin/boss fallback) — the core deliverable, prose trimmed to keep the
  // bump to one KiB above actual.
  // Bumped 24576 → 25600 for BOS-243: Phase 3c now documents the
  // `attached_existing` create_session signal (attach vs orphan-PR fallback)
  // so the repair driver can distinguish an attach from a fresh session.
  // Bumped 25600 → 27648 for BOS-198, the "rebuild boss-epic on the extracted
  // adapters" ticket: the SKILL now exposes the three pluggable seams (pure DAG
  // scheduler dag-scheduler.mjs, resolveTrackerAdapter, resolveSessionRunnerAdapter)
  // and routes assembly/state/progress + session choreography + sub-skill
  // dispatch (subSkills.implement/repair) through them — the core deliverable.
  const RATCHET = 27648
  const bytes = Buffer.byteLength(CLAUDE, 'utf8')
  assert.ok(bytes <= RATCHET, `CLAUDE SKILL.md is ${bytes} bytes; must stay <= ${RATCHET}`)
})

test('frontmatter identifies the skill', () => {
  assert.match(CLAUDE, /^---\r?\nname: boss-epic\r?\n/, 'frontmatter must declare name: boss-epic')
})

test('codex mirror is exactly the rewrite of the claude source', () => {
  assert.equal(
    CODEX,
    rewriteClaudeSkillMarkdown(CLAUDE, '.claude/skills/boss-epic/SKILL.md'),
    'codex SKILL.md mirror must equal the generated rewrite of the claude skill',
  )
})

test('contract tokens present in both mirrors', () => {
  for (const body of [CLAUDE, CODEX]) {
    for (const token of [
      'bs-epic-lib.mjs',
      'merge_session',
      'confirm',
      'get_session_statuses',
      'list_check_snapshots',
      'create_session',
      'send_chat_message',
      'transitiveDependents',
      'nextToMerge',
      'boss-epic-progress',
      '/boss-repair watch',
    ]) {
      assert.ok(body.includes(token), `missing token: ${token}`)
    }
  }
})

test('default agent is claude in both mirrors', () => {
  for (const body of [CLAUDE, CODEX]) {
    assert.ok(body.includes('--agent claude'), 'missing default `--agent claude`')
  }
})

test('safety pins', () => {
  // at most one merge in flight at a time (serialized merges)
  assert.match(CLAUDE, /at most one merge in flight/i)
  // repair/dispatch does not tear down an in-progress session
  assert.match(CLAUDE, /leave the session open/i)
  // the epic set boundary is a hard guarantee, never crossed
  assert.match(CLAUDE, /outside the epic set are NEVER mutated/i)
  // headless unattended discipline: no interactive prompts past preflight
  assert.match(CLAUDE, /never call AskUserQuestion after Phase 0/i)
  // wiring contract: done siblings clear via externallyCleared, never merged
  assert.match(CLAUDE, /`done` ids are folded into\s+`externallyCleared`/)
  // negative pin: the done-bucket-into-merged misstatement must never return
  // (backticked `done` = the classifyTickets bucket; the plain-prose "Done"
  // state name legitimately appears near `merged` on the 3d success path)
  assert.doesNotMatch(CLAUDE, /`done`[^\n]*into `merged`/)
  // loop termination: greens keep their concurrency slot until merged
  assert.match(CLAUDE, /`greens` is a \*\*subset\*\*\s+of `inFlight`/)
})

test('no stub or placeholder prose in the claude source', () => {
  // Case-sensitive: the Linear status name `Todo` appears legitimately
  // throughout (e.g. "eligible: `Todo` + agent-friendly"); the stub-marker
  // convention this guards against is an all-caps `TODO` note.
  assert.doesNotMatch(CLAUDE, /\bTODO\b/, 'CLAUDE SKILL.md must not contain TODO markers')
  assert.doesNotMatch(
    CLAUDE,
    /\bplaceholder\b/i,
    'CLAUDE SKILL.md must not contain placeholder prose',
  )
})
