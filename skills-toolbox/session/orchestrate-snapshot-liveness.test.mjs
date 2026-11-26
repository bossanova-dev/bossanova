#!/usr/bin/env node
// Liveness verdicts through `collect`, with daemon dumps shaped like the 2026-11-26 rounds: build
// sessions on draft PRs whose chats bossd served WORKING, QUESTION, or briefly IDLE while their
// agents were mid-turn on a heavily loaded host.
import assert from 'node:assert/strict'
import test from 'node:test'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { collect } from './orchestrate-snapshot.mjs'

const NOW = Date.parse('2026-11-26T11:48:00Z')
const ago = (minutes) => new Date(NOW - minutes * 60 * 1000).toISOString()

// `boss show --json`'s session, trimmed to what the classifier reads.
const buildSession = (id) => ({
  id,
  agent: 'claude',
  title: `[BOS-1] Build BOS-1 (${id})`,
  state: 'AWAITING_CHECKS',
  repo_id: 'r-boss',
  branch: `build-bos-1-${id}`,
  last_agent_activity_at: ago(0),
})

// One `boss chats --json` row.
const chat = ({ status, quietMinutes, ...over }) => ({
  agent_session_id: `c-${status.toLowerCase()}-${quietMinutes ?? 'none'}`,
  title: 'build',
  role: 'build',
  created_at: ago(180),
  status,
  waiting_reason: '',
  phase: '',
  spinner_present: false,
  last_output_seeded: false,
  last_output_at: quietMinutes == null ? '' : ago(quietMinutes),
  last_substantive_output_at: quietMinutes == null ? '' : ago(quietMinutes),
  ...over,
})

// `boss pr state --json` for a draft build PR whose checks are still running.
const draftPr = (number) => ({
  pr_number: number,
  state: 'open',
  is_draft: true,
  is_mergeable: true,
  merge_state_status: 'draft',
  has_build_receipt: false,
  check_verdict: { state: 'pending', failed_count: 0, pending_count: 3 },
  verify_phase: { kind: '', reason: '', label: '' },
  checks: [],
})

// Runs one collect round over a local daemon whose dump holds one draft session per chat list.
function round(t, chatLists) {
  const sessions = chatLists.map((chats, i) => ({
    session: buildSession(`s${i}`),
    chats,
    pr: draftPr(4700 + i),
    repo: 'recurser/bossanova',
  }))
  const dump = () => ({ sessions, residuals: [], errors: [], partial: false })
  const root = tempDir('orchestrate-liveness-', t)
  const result = collect({ hosts: [], local: true, bossBin: 'boss', now: NOW, root, dump })
  assert.deepEqual(result.errors, [])
  return result.rows
}

const STALL_ACTIONS = new Set(['stalled', 'stalled-chat'])

test('a WORKING chat keeps its session live however old its last output is', (t) => {
  // A build sitting in a long `gate-run.mjs wait`: WORKING, no spinner sampled, output hours old.
  const rows = round(t, [
    [chat({ status: 'WORKING', quietMinutes: 154 })],
    [chat({ status: 'WORKING', quietMinutes: null })],
    [chat({ status: 'WORKING', quietMinutes: 40, waiting_reason: 'awaiting checks_passed' })],
  ])
  for (const row of rows) {
    assert.equal(row.action, 'working', row.session)
    assert.equal(row.next, '', row.session)
  }
})

test('a chat showing a dialog is routed to the dialog, never reported stalled', (t) => {
  const [row] = round(t, [[chat({ status: 'QUESTION', quietMinutes: 6 })]])
  assert.equal(row.action, 'question-chat')
  assert.equal(row.chat, 'c-question-6')
})

test('a draft whose chat went IDLE moments ago is not stalled', (t) => {
  // bossd served these IDLE for builds that were mid-turn one poll later (host load ~100).
  const rows = round(t, [
    [chat({ status: 'IDLE', quietMinutes: 1 })],
    [chat({ status: 'IDLE', quietMinutes: 6 })],
    // The newest chat has never reported; the build chat beside it spoke a minute ago.
    [
      chat({ status: 'IDLE', quietMinutes: 1 }),
      chat({ status: 'UNSPECIFIED', quietMinutes: null, role: 'verify', created_at: ago(5) }),
    ],
  ])
  for (const row of rows) {
    assert.ok(!STALL_ACTIONS.has(row.action), `${row.session} classified ${row.action}`)
    assert.equal(row.next, '', row.session)
  }
})

test('a draft whose chats are all idle, unreported or stopped, with nothing armed, is stalled', (t) => {
  const rows = round(t, [
    [chat({ status: 'IDLE', quietMinutes: 20 })],
    [chat({ status: 'STOPPED', quietMinutes: 30 })],
    [chat({ status: 'UNSPECIFIED', quietMinutes: null })],
    [],
    [
      chat({ status: 'IDLE', quietMinutes: 25 }),
      chat({ status: 'UNSPECIFIED', quietMinutes: null, role: 'verify', created_at: ago(5) }),
    ],
  ])
  const actions = rows.map((row) => [row.session, row.action])
  assert.deepEqual(actions, [
    ['s0', 'stalled-chat'],
    ['s1', 'stalled'],
    ['s2', 'stalled'],
    ['s3', 'stalled'],
    ['s4', 'stalled'],
  ])
  // The stuck chat gets a nudge; a session with no chat to nudge is left to the operator.
  assert.notEqual(rows[0].next, '')
})
