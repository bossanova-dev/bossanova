#!/usr/bin/env node

import assert from 'node:assert/strict'
import test from 'node:test'

import {
  CAPTION_BAR_STYLE,
  MAX_CAPTION,
  OVERLAY_CAPTION_CSS,
  TIMER_CLEARANCE_PX,
} from './proof-caption-spec.mjs'
import { OVERLAY_CAPTION_CSS as RUNNER_OVERLAY_CAPTION_CSS } from './proof-playwright-runner.mjs'
import { CAPTION_BAR_STYLE as TERMINAL_CAPTION_BAR_STYLE } from './proof-render-terminal.mjs'

// Byte-pin: this is the EXACT literal that lived at
// proof-playwright-runner.mjs:18-22 before extraction (verified against main
// @ a4938a9fb). Pinning it here proves the extraction changed nothing.
const PRE_EXTRACTION_OVERLAY_CAPTION_CSS =
  'position:absolute;top:24px;left:50%;transform:translateX(-50%);' +
  'background:rgba(0,0,0,0.72);color:#fff;font:600 15px/1.4 sans-serif;' +
  'padding:6px 18px;border-radius:6px;white-space:pre-wrap;max-width:max(60%, calc(100% - 380px));' +
  'text-align:center;pointer-events:none;display:none;'

// Byte-pin: the EXACT literal that lived at proof-render-terminal.mjs:82-83
// before extraction.
const PRE_EXTRACTION_CAPTION_BAR_STYLE =
  'background:#1d4ed8;color:#fff;font:600 14px/1.5 sans-serif;padding:6px 14px;'

test('OVERLAY_CAPTION_CSS is byte-identical to the pre-extraction literal', () => {
  assert.equal(OVERLAY_CAPTION_CSS, PRE_EXTRACTION_OVERLAY_CAPTION_CSS)
})

test('CAPTION_BAR_STYLE is byte-identical to the pre-extraction literal', () => {
  assert.equal(CAPTION_BAR_STYLE, PRE_EXTRACTION_CAPTION_BAR_STYLE)
})

test('MAX_CAPTION is 140', () => {
  assert.equal(MAX_CAPTION, 140)
})

test('OVERLAY_CAPTION_CSS reserves TIMER_CLEARANCE_PX for the burned-in timer', () => {
  assert.ok(OVERLAY_CAPTION_CSS.includes(`calc(100% - ${TIMER_CLEARANCE_PX}px)`))
})

test('proof-playwright-runner re-exports the same OVERLAY_CAPTION_CSS reference (import-equality)', () => {
  assert.equal(RUNNER_OVERLAY_CAPTION_CSS, OVERLAY_CAPTION_CSS)
})

test('proof-render-terminal re-exports the same CAPTION_BAR_STYLE reference (import-equality)', () => {
  assert.equal(TERMINAL_CAPTION_BAR_STYLE, CAPTION_BAR_STYLE)
})
