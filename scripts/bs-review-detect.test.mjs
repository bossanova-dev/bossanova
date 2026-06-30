import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { detectChangeTypes, secondVoiceAgent } from './bs-review-detect.mjs'

const scriptPath = fileURLToPath(new URL('./bs-review-detect.mjs', import.meta.url))

/** Run the CLI block as a subprocess and return { stdout, status }. */
function runCli(args = [], input = '') {
  const res = spawnSync(process.execPath, [scriptPath, ...args], {
    input,
    encoding: 'utf8',
  })
  return { stdout: res.stdout.trim(), status: res.status }
}

test('detectChangeTypes flags Go files', () => {
  const r = detectChangeTypes(['services/bossd/internal/tmux/tmux.go', 'README.md'])
  assert.equal(r.go, true)
  assert.equal(r.tui, false)
  assert.equal(r.web, false)
})

test('detectChangeTypes flags TUI changes under services/boss', () => {
  const r = detectChangeTypes(['services/boss/internal/views/attach.go'])
  assert.equal(r.tui, true)
  assert.equal(r.go, true) // a .go file under services/boss is both Go and TUI
})

test('detectChangeTypes flags web changes under services/web', () => {
  const r = detectChangeTypes(['services/web/src/App.tsx', 'services/web/src/index.css'])
  assert.equal(r.web, true)
  assert.equal(r.go, false)
})

test('detectChangeTypes on docs-only diff flags nothing', () => {
  const r = detectChangeTypes(['docs/foo.md', 'CONCEPTS.md'])
  assert.deepEqual(r, { go: false, tui: false, web: false })
})

test('secondVoiceAgent returns the opposite agent', () => {
  assert.equal(secondVoiceAgent('claude'), 'codex')
  assert.equal(secondVoiceAgent('codex'), 'claude')
})

test('secondVoiceAgent defaults unknown/empty agent to codex (claude is the common host)', () => {
  assert.equal(secondVoiceAgent(''), 'codex')
  assert.equal(secondVoiceAgent('opencode'), 'codex')
})

test('CLI --second-voice claude prints codex', () => {
  const { stdout, status } = runCli(['--second-voice', 'claude'])
  assert.equal(stdout, 'codex')
  assert.equal(status, 0)
})

test('CLI --second-voice with missing value defaults to codex', () => {
  const { stdout, status } = runCli(['--second-voice'])
  assert.equal(stdout, 'codex')
  assert.equal(status, 0)
})

test('CLI --second-voice opencode prints codex', () => {
  const { stdout, status } = runCli(['--second-voice', 'opencode'])
  assert.equal(stdout, 'codex')
  assert.equal(status, 0)
})

test('CLI classifies a newline-separated file list on stdin', () => {
  const { stdout, status } = runCli([], 'services/boss/internal/views/attach.go\nREADME.md\n')
  assert.equal(stdout, '{"go":true,"tui":true,"web":false}')
  assert.equal(status, 0)
})

test('CLI on empty stdin flags nothing', () => {
  const { stdout, status } = runCli([], '')
  assert.equal(stdout, '{"go":false,"tui":false,"web":false}')
  assert.equal(status, 0)
})
