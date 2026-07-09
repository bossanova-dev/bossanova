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
//   * a NEGATIVE assertion against stub/placeholder prose.
//
// BOS-271 collapsed the published cores onto the boss-repair single-source
// topology: the canonical committed home is the embedded skillinstall payload
// (services/boss/internal/skillinstall/skills/boss-epic/), with no .claude/.codex
// committed copy. This test reads that canonical home; there is no codex-mirror
// copy of the four published cores to compare against anymore.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')

const CLAUDE = read('../services/boss/internal/skillinstall/skills/boss-epic/SKILL.md')

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
  // Bumped 27648 → 28672 for the settled-green merge gate: Phase 3b/3c now require
  // the child session to have SETTLED (chat IDLE + stale last_agent_activity_at,
  // or STOPPED + stale/missing last_agent_activity_at) before a Passing green is merge-eligible, fixing
  // premature merges of still-working children whose own boss-implement review +
  // comment resolution had not finished (recurser/bossanova#1174). This makes the
  // classified greens actually match nextToMerge's "passed-review" contract.
  // Bumped 28672 → 29696 for BOS-322, the "planning-only epic work must not
  // spawn PR-backed sessions or surface false pr_no_changes" ticket: the
  // Operating Contract now carries a concrete three-case routing contract naming
  // the session-runner capabilities (implementation → `createSession` +
  // `tmux_unattended`; unattended planning → subagent; visible planning →
  // `createPlanningChat` / `quick_chat: true`) and Phase 3a states the
  // `createSession` block is implementation-only — the core deliverable.
  const RATCHET = 29696
  const bytes = Buffer.byteLength(CLAUDE, 'utf8')
  assert.ok(bytes <= RATCHET, `CLAUDE SKILL.md is ${bytes} bytes; must stay <= ${RATCHET}`)
})

test('frontmatter identifies the skill', () => {
  assert.match(CLAUDE, /^---\r?\nname: boss-epic\r?\n/, 'frontmatter must declare name: boss-epic')
})

test('contract tokens present in the canonical skill', () => {
  for (const body of [CLAUDE]) {
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

test('passing greens require settled child chat before merge eligibility', () => {
  assert.match(
    CLAUDE,
    /GREEN_DRAFT or READY_FOR_REVIEW \+ DisplayStatus Passing \+ chat SETTLED/,
    'passing greens must require a settled child chat',
  )
  assert.match(
    CLAUDE,
    /Passing but chat still WORKING\/QUESTION\/LIMITED[^\n]*hold/i,
    'unsettled passing children must be held, not merged or repaired',
  )
  assert.match(
    CLAUDE,
    /treat the\s+child as \*\*not settled\*\* and re-poll; never assume settled on an unreadable status/,
    'unreadable chat status must block merge eligibility',
  )
  assert.match(
    CLAUDE,
    /STOPPED \+\s+missing `last_agent_activity_at` = settled/,
    'stopped chats with no activity timestamp must not be held until wall-clock failure',
  )
  assert.doesNotMatch(
    CLAUDE,
    /GREEN_DRAFT or READY_FOR_REVIEW \+ DisplayStatus Passing\*\*?[^+\n]*→ add to the\s+\*\*greens\*\*/,
    'the old Passing-only green transition must not return',
  )
})

test('planning-only work is routed away from PR-backed sessions', () => {
  assert.match(CLAUDE, /planning-only/i, 'skill must explicitly classify planning-only work')
  assert.match(
    CLAUDE,
    /subagent/i,
    'unattended planning fan-out should be routed to subagents, not sessions',
  )
  assert.match(
    CLAUDE,
    /quick_chat:\s*true/,
    'visible planning conversations should use quick_chat:true',
  )
  assert.match(
    CLAUDE,
    /must not use\s+`?create_session`?.*tmux_unattended/i,
    'planning-only work must not use PR-backed tmux_unattended sessions',
  )
})

test('planning-only routing names concrete capability boundaries', () => {
  // BOS-322: the three routing cases must be concrete and name the session-runner
  // adapter capabilities (createSession vs createPlanningChat), not prose-only
  // advice — so a planning subtask can never regress into the PR-backed
  // implementation path. Each case must live on a single line (capability + its
  // discriminating field co-located).
  assert.match(
    CLAUDE,
    /implementation work uses[^\n]*createSession[^\n]*tmux_unattended/i,
    'implementation work must name the createSession capability + tmux_unattended',
  )
  assert.match(
    CLAUDE,
    /unattended[^\n]*planning[^\n]*subagent/i,
    'unattended planning fan-out must route to a subagent',
  )
  assert.match(
    CLAUDE,
    /visible planning chat uses[^\n]*createPlanningChat/i,
    'visible planning chat must name the createPlanningChat capability',
  )
  assert.doesNotMatch(
    CLAUDE,
    /planning[- ]only[^\n]*(?:use|via|route|through)[^\n]*create_session[^\n]*tmux_unattended/i,
    'planning-only work must never be routed to create_session + tmux_unattended',
  )
})

test('non-claude runners never fall back to merging without settlement', () => {
  assert.doesNotMatch(
    CLAUDE,
    /still schedules\/merges greens/,
    'runner fallback must not claim it can merge greens without chat settlement',
  )
  assert.match(
    CLAUDE,
    /runner without\s+readable chat status must\s+hold or fail-isolate/i,
    'non-claude fallback must preserve the settled-green gate',
  )
})

test('default agent is claude', () => {
  for (const body of [CLAUDE]) {
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
