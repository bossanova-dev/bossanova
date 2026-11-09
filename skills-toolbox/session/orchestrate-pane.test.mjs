#!/usr/bin/env node
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { approvePrompt, chatPane, findApprovalPrompt, paneTail } from './orchestrate-pane.mjs'

// Pane captures. codex-approval, codex-reply-choice and claude-limit-modal are copies of real
// captures the agent plugins own (plugins/bossd-plugin-codex/testdata/panes/question.txt and
// reply_choice.txt, plugins/bossd-plugin-claude/testdata/panes/limit_decision_modal.txt).
// claude-bash-approval mirrors the BOS-1266 capture of Claude Code's boxed approval card, with its
// escape sequences and non-breaking space (boxedApprovalPane in statusdetect/question_test.go).
// claude-edit-approval and claude-yes-no-question are reconstructions of Claude Code's unboxed
// edit approval and of an AskUserQuestion menu.
const PANES = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  '..',
  'testdata',
  'orchestrate-panes',
)
const pane = (name) => fs.readFileSync(path.join(PANES, `${name}.txt`), 'utf8')

test('a codex command approval is answered with its own (y) shortcut', () => {
  const prompt = findApprovalPrompt(pane('codex-approval'))
  assert.equal(prompt.question, 'Would you like to run the following command?')
  assert.equal(prompt.option.label, 'Yes, proceed (y)')
  assert.equal(prompt.key, 'y')
})

test('a Claude Code approval card is answered with the number of its one-time Yes', () => {
  const bash = findApprovalPrompt(pane('claude-bash-approval'))
  assert.equal(bash.question, 'Do you want to proceed?')
  assert.equal(bash.key, '1')
  // The cursor sits on the session-wide Yes; the one-time Yes is still the answer.
  const edit = findApprovalPrompt(pane('claude-edit-approval'))
  assert.equal(edit.question, 'Do you want to make this edit to config.ts?')
  assert.equal(edit.option.label, 'Yes')
  assert.equal(edit.key, '1')
  assert.equal(edit.selected, 2)
})

test('menus that are not permission prompts are never approved', () => {
  for (const name of ['claude-limit-modal', 'codex-reply-choice', 'claude-yes-no-question'])
    assert.equal(findApprovalPrompt(pane(name)), null, name)
  // An answered prompt that has scrolled up under new output is no longer live.
  const answered = `${pane('claude-bash-approval')}⏺ Bash(rm -rf "$BUILD_DIR"/)\n  ⎿  (No content)\n\n⏺ Rebuilding now.\n\n❯ \n`
  assert.equal(findApprovalPrompt(answered), null)
  assert.equal(findApprovalPrompt(''), null)
})

// A pane that shows `screens` in turn: each read returns the current one, each press advances.
function fakePane(...screens) {
  const pressed = []
  let at = 0
  return {
    pressed,
    read: () => screens[Math.min(at, screens.length - 1)],
    press: (key) => {
      pressed.push(key)
      at++
    },
  }
}

const cleared = '⏺ Bash(rm -rf "$BUILD_DIR"/)\n  ⎿  (No content)\n'

test('approving presses the affirmative key once and confirms the prompt cleared', () => {
  const io = fakePane(pane('claude-bash-approval'), cleared)
  const result = approvePrompt(io)
  assert.deepEqual(io.pressed, ['1'])
  assert.equal(result.approved, true)
  assert.equal(result.option, '1. Yes')
})

test('approving refuses, pressing nothing, when no prompt is visible', () => {
  for (const screen of [cleared, pane('claude-limit-modal'), pane('claude-yes-no-question')]) {
    const io = fakePane(screen)
    const result = approvePrompt(io)
    assert.equal(result.approved, false)
    assert.deepEqual(io.pressed, [])
  }
})

test('a number that only moved the cursor onto Yes is confirmed with Enter', () => {
  const onYes = pane('claude-edit-approval')
    .replace('   1. Yes', ' ❯ 1. Yes')
    .replace(' ❯ 2. Yes, allow', '   2. Yes, allow')
  const io = fakePane(pane('claude-edit-approval'), onYes, cleared)
  const result = approvePrompt(io)
  assert.deepEqual(io.pressed, ['1', 'Enter'])
  assert.equal(result.approved, true)
})

test('a prompt that will not clear is reported after one key and one Enter', () => {
  const io = fakePane(pane('codex-approval'))
  const result = approvePrompt(io)
  // The cursor already sits on Yes, so Enter confirms it when the shortcut did not.
  assert.deepEqual(io.pressed, ['y', 'Enter'])
  assert.equal(result.approved, false)
  assert.match(result.reason, /still showing/)
})

test('the pane is found by the chat id and keys reach only that tmux session', () => {
  const scripts = []
  const shell = (host, script) => {
    scripts.push({ host, script })
    if (script.includes('tmux ls')) return 'boss-abc-0123abcd\n'
    if (script.includes('capture-pane')) return pane('codex-approval')
    return ''
  }
  const chat = chatPane({ host: 'build-host', chat: '0123abcd-ffff', remotePath: '/bin', shell })
  assert.equal(chat.target, 'boss-abc-0123abcd')
  const result = approvePrompt({ read: () => chat.read(), press: chat.press })
  assert.equal(result.approved, false)
  const sent = scripts.filter(({ script }) => script.includes('send-keys'))
  assert.deepEqual(
    sent.map(({ host, script }) => [host, script.split('\n').pop()]),
    [
      ['build-host', "tmux send-keys -t 'boss-abc-0123abcd' -l -- 'y'"],
      ['build-host', "tmux send-keys -t 'boss-abc-0123abcd' Enter"],
    ],
  )
  assert.throws(() => chatPane({ chat: "x'; rm -rf /", remotePath: '/bin', shell }), /invalid/)
  assert.throws(() => chat.press('C-c'), /refusing key/)
})

test('the tail drops blank rows and keeps the last lines', () => {
  assert.equal(paneTail('a\n\n  \nb\nc\n', 2), 'b\nc')
})
