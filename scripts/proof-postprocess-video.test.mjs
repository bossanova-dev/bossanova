import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { readCaptionTimings } from './proof-postprocess-video.mjs'

function tmpDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'proof-postprocess-'))
}

test('readCaptionTimings: reads a raw/ child caption-timings.json', () => {
  const dir = tmpDir()
  const raw = path.join(dir, 'raw')
  fs.mkdirSync(raw, { recursive: true })
  const timings = [{ seq: 1, caption: 'Open settings', startMs: 0 }]
  fs.writeFileSync(path.join(raw, 'caption-timings.json'), JSON.stringify(timings))
  assert.deepEqual(readCaptionTimings(dir), timings)
})

test('readCaptionTimings: finds the sibling ../raw/ file (TUI agent layout)', () => {
  // TUI layout: webm in <local>/tui-agent/, timings in <local>/raw/.
  const local = tmpDir()
  const recipeDir = path.join(local, 'tui-agent')
  const raw = path.join(local, 'raw')
  fs.mkdirSync(recipeDir, { recursive: true })
  fs.mkdirSync(raw, { recursive: true })
  const timings = [{ seq: 2, caption: 'Cron list', startMs: 1200 }]
  fs.writeFileSync(path.join(raw, 'caption-timings.json'), JSON.stringify(timings))
  assert.deepEqual(readCaptionTimings(recipeDir), timings)
})

test('readCaptionTimings: missing file → [] (no-caption re-burn)', () => {
  assert.deepEqual(readCaptionTimings(tmpDir()), [])
})

test('readCaptionTimings: malformed JSON → [] (degrade, never throw)', () => {
  const dir = tmpDir()
  fs.writeFileSync(path.join(dir, 'caption-timings.json'), '{not json')
  assert.deepEqual(readCaptionTimings(dir), [])
})

test('readCaptionTimings: a non-array JSON payload → []', () => {
  const dir = tmpDir()
  fs.writeFileSync(path.join(dir, 'caption-timings.json'), JSON.stringify({ nope: true }))
  assert.deepEqual(readCaptionTimings(dir), [])
})
