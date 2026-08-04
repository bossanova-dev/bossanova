#!/usr/bin/env node

import { spawnSync } from 'node:child_process'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  buildSpec,
  validateRecipe,
  slugify,
  parseArgs,
  stageEnvForArgs,
  OVERLAY_CAPTION_CSS,
  VIDEO_ACTIONS,
  uploadServerScript,
} from './proof-playwright-runner.mjs'
import { OVERLAY_CAPTION_CSS as SPEC_OVERLAY_CAPTION_CSS } from './proof-caption-spec.mjs'

const repoRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)))
const runnerPath = path.join(repoRoot, 'scripts/proof-playwright-runner.mjs')

test('buildSpec video branch records webm via its own context and screenshots a poster', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      surface: 'web',
      capture: 'video',
      steps: [
        { action: 'goto', route: '/' },
        { action: 'click', selector: '[data-testid="row"]' },
        { action: 'type', selector: 'input[name="q"]', value: 'hello' },
        { action: 'press', key: '?' },
        { action: 'wait', timeoutMs: 500 },
      ],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /recordVideo/)
  assert.match(spec, /newContext/)
  assert.match(spec, /web-flow\.webm/)
  assert.match(spec, /web-flow\.png/) // poster
  assert.match(spec, /\.click\(\)/)
  assert.match(spec, /pressSequentially\('hello', \{ delay: 60 \}\)/)
  assert.match(spec, /page\.keyboard\.press\('\?'\)/)
  assert.match(spec, /context\.close\(\)/) // finalizes the video before rename
})

test('buildSpec stages a closing attach socket only for the reconnecting chat recipe', () => {
  const reconnectingSpec = buildSpec({
    recipe: { id: 'web-chat-terminal-reconnecting', surface: 'web', route: '/' },
    outputDir: '/tmp/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })
  const healthySpec = buildSpec({
    recipe: { id: 'web-chat-terminal', surface: 'web', route: '/' },
    outputDir: '/tmp/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })

  assert.match(reconnectingSpec, /ws\.close\(\)/)
  assert.doesNotMatch(reconnectingSpec, /ws\.send\(\)/)
  assert.match(healthySpec, /ws\.send\(Buffer\.from/)
})

test('buildSpec replays the BOS-658 glyph line only for the plain chat-terminal still', () => {
  const specFor = (id) =>
    buildSpec({
      recipe: { id, surface: 'web', route: '/' },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })

  // The captured canvas must carry real terminal output, so the healthy
  // chat-terminal socket replays a kind=0 data frame holding the evidence
  // glyphs. Every other web recipe keeps the original clients-frame-only
  // socket.
  const chatTerminal = specFor('web-chat-terminal')
  assert.match(chatTerminal, /✓ ↳ ─/)
  assert.match(chatTerminal, /Buffer\.concat\(\[header, payload\]\)/)

  assert.doesNotMatch(specFor('web-sessions'), /✓ ↳ ─/)
  assert.doesNotMatch(specFor('web-chat-terminal-reconnecting'), /✓ ↳ ─/)
})

test('buildSpec stages the BOS-661 upload responder only for the upload recipe', () => {
  const specFor = (id) =>
    buildSpec({
      recipe: { id, surface: 'web', route: '/' },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })

  // Without a staged responder the browser's uploadFile() never receives a
  // kind=11 result, so the 'Upload complete' banner the recipe promises never
  // renders and the capture would hang on its final wait step.
  const upload = specFor('web-chat-terminal-upload')
  assert.match(upload, /ws\.onMessage\(/)
  assert.match(upload, /__frame\(10, Buffer\.concat\(\[id, seq, acked\]\)\)/) // chunk ack
  assert.match(upload, /__frame\(11, Buffer\.concat\(\[id, Buffer\.from\(\[0x01\]\)/) // ok result
  // The control's only job is to click the hidden <input type=file>, which
  // Chromium reports as a filechooser — the click step is inert without this.
  assert.match(upload, /page\.on\('filechooser'/)
  assert.match(upload, /agent-brief\.txt/)
  // The page clears its "Connecting…" overlay on the first data byte only, so
  // the upload capture replays one too.
  assert.match(upload, /✓ ↳ ─/)

  for (const id of ['web-sessions', 'web-chat-terminal', 'web-chat-terminal-reconnecting']) {
    assert.doesNotMatch(specFor(id), /filechooser/)
    assert.doesNotMatch(specFor(id), /ws\.onMessage\(/)
  }
})

// ----- BOS-661 staged upload responder: the ok flag must be CONDITIONAL -----
//
// The web-chat-terminal-upload video is the only end-to-end artefact for the
// browser side of BOS-661, and its final step waits for the 'Upload complete'
// banner. That banner is painted from the responder's kind=11 ok flag, so a
// responder that answers ok unconditionally makes the capture unfalsifiable:
// a browser that sent upload_start + upload_finish and NO chunks would still
// pass. These tests drive the generated snippet directly against a fake socket
// and pin the failure paths, so the proof keeps its ability to fail.

const UPLOAD_ID = Buffer.from('proof-upload-1', 'ascii')
const UPLOAD_ID_PREFIX = Buffer.concat([Buffer.from([UPLOAD_ID.length]), UPLOAD_ID])

function u64be(value) {
  const buf = Buffer.alloc(8)
  buf.writeUInt32BE(Math.floor(value / 4294967296), 0)
  buf.writeUInt32BE(value >>> 0, 4)
  return buf
}

function attachEnvelope(kind, payload) {
  const header = Buffer.from([
    kind,
    (payload.length >>> 16) & 0xff,
    (payload.length >>> 8) & 0xff,
    payload.length & 0xff,
  ])
  return Buffer.concat([header, payload])
}

function uploadStartFrame(sizeBytes, filename = 'agent-brief.txt') {
  const name = Buffer.from(filename, 'utf8')
  const nameLen = Buffer.alloc(2)
  nameLen.writeUInt16BE(name.length, 0)
  return attachEnvelope(6, Buffer.concat([UPLOAD_ID_PREFIX, u64be(sizeBytes), nameLen, name]))
}

function uploadChunkFrame(seq, data) {
  return attachEnvelope(7, Buffer.concat([UPLOAD_ID_PREFIX, u64be(seq), Buffer.from(data, 'utf8')]))
}

function uploadFinishFrame() {
  return attachEnvelope(8, UPLOAD_ID_PREFIX)
}

// Runs the staged responder snippet against a fake `ws` and returns every
// frame it sent back, decoded to { kind, ok, message }.
function runUploadResponder(frames) {
  const source = uploadServerScript({ id: 'web-chat-terminal-upload' })
  assert.ok(source.includes('ws.onMessage('), 'responder snippet is empty')
  const sent = []
  let handler = null
  const ws = {
    send: (buf) => sent.push(Buffer.from(buf)),
    onMessage: (fn) => {
      handler = fn
    },
  }
  // eslint-disable-next-line no-new-func -- the snippet is generated by this repo
  new Function('ws', 'Buffer', source)(ws, Buffer)
  assert.ok(handler, 'responder registered no message handler')
  for (const frame of frames) {
    handler(frame)
  }
  return sent.map((buf) => {
    const payload = buf.subarray(4)
    const idLen = payload[0]
    return {
      kind: buf[0],
      ok: buf[0] === 11 ? (payload[1 + idLen] & 0x01) === 0x01 : undefined,
      message: buf[0] === 11 ? payload.subarray(2 + idLen).toString('utf8') : '',
    }
  })
}

const UPLOAD_BODY = 'Fixture upload for the BOS-661 chat file upload proof.\n'
const UPLOAD_SIZE = Buffer.byteLength(UPLOAD_BODY, 'utf8')

test('staged upload responder reports ok only for a complete, in-order transfer', () => {
  const results = runUploadResponder([
    uploadStartFrame(UPLOAD_SIZE),
    uploadChunkFrame(0, UPLOAD_BODY),
    uploadFinishFrame(),
  ]).filter((frame) => frame.kind === 11)

  assert.equal(results.length, 1, 'exactly one terminal frame per upload id')
  assert.equal(results[0].ok, true)
  assert.equal(results[0].message, '', 'error_message is empty when ok')
})

test('staged upload responder FAILS an upload whose chunks never arrived', () => {
  // The exact regression the video has to be able to catch: start + finish
  // with zero bytes in between.
  const results = runUploadResponder([uploadStartFrame(UPLOAD_SIZE), uploadFinishFrame()]).filter(
    (frame) => frame.kind === 11,
  )

  assert.equal(results.length, 1)
  assert.equal(results[0].ok, false)
  assert.match(results[0].message, /incomplete upload: received 0 of \d+ bytes/)
})

test('staged upload responder FAILS a short transfer', () => {
  const results = runUploadResponder([
    uploadStartFrame(UPLOAD_SIZE),
    uploadChunkFrame(0, UPLOAD_BODY.slice(0, 10)),
    uploadFinishFrame(),
  ]).filter((frame) => frame.kind === 11)

  assert.equal(results.length, 1)
  assert.equal(results[0].ok, false)
  assert.match(results[0].message, /incomplete upload/)
})

test('staged upload responder FAILS an out-of-order chunk and an unstarted upload', () => {
  const skipped = runUploadResponder([
    uploadStartFrame(UPLOAD_SIZE),
    uploadChunkFrame(1, UPLOAD_BODY), // seq 0 was never sent
  ]).filter((frame) => frame.kind === 11)
  assert.equal(skipped.length, 1)
  assert.equal(skipped[0].ok, false)
  assert.match(skipped[0].message, /out-of-order chunk/)

  const unstarted = runUploadResponder([uploadFinishFrame()]).filter((frame) => frame.kind === 11)
  assert.equal(unstarted.length, 1)
  assert.equal(unstarted[0].ok, false)
  assert.match(unstarted[0].message, /unknown upload id/)
})

test('staged upload responder acks each chunk with the cumulative byte count', () => {
  const half = Math.floor(UPLOAD_SIZE / 2)
  const acks = runUploadResponder([
    uploadStartFrame(UPLOAD_SIZE),
    uploadChunkFrame(0, UPLOAD_BODY.slice(0, half)),
    uploadChunkFrame(1, UPLOAD_BODY.slice(half)),
    uploadFinishFrame(),
  ])

  assert.equal(acks.filter((frame) => frame.kind === 10).length, 2)
  const results = acks.filter((frame) => frame.kind === 11)
  assert.equal(results.length, 1)
  assert.equal(results[0].ok, true, 'a chunked-but-complete transfer still succeeds')
})

test('the shipped catalog keeps the upload recipe wired to the web path rule', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find((r) => r.id === 'web-chat-terminal-upload')
  assert.ok(recipe, 'web-chat-terminal-upload recipe is missing from the catalog')
  assert.doesNotThrow(() => validateRecipe(recipe))
  // A recipe absent from the pathRule is never selected for a services/web
  // diff, so it would silently prove nothing.
  const webRule = catalog.pathRules.find((rule) => rule.patterns.includes('services/web/'))
  assert.ok(webRule.recipeIds.includes('web-chat-terminal-upload'))
})

test('buildSpec waits for the rendered glyph row before capturing the chat-terminal still', () => {
  // The capture selector ([data-testid='chat-terminal-canvas']) goes visible as
  // soon as xterm mounts, which is before the staged socket's data frame is
  // painted — so toBeVisible() alone can screenshot an empty pane. The gate
  // must come from the rendered rows, and only for the recipe that stages the
  // glyph line.
  const specFor = (id, stageEnv) =>
    buildSpec({
      recipe: { id, surface: 'web', route: '/', selector: "[data-testid='chat-terminal-canvas']" },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv,
    })

  const staged = specFor('web-chat-terminal', { VITE_E2E: '1' })
  assert.ok(staged.includes(String.raw`page.locator('.xterm-rows').first()`))
  // Tolerant spacing: xterm pads a row with cell spaces between style runs.
  assert.ok(staged.includes(String.raw`toContainText(new RegExp("✓\\s*↳\\s*─")`))
  // The wait must precede the screenshot, or it gates nothing.
  assert.ok(staged.indexOf('.xterm-rows') < staged.indexOf('.screenshot('))

  // No staging → no glyph line → the wait would hang forever.
  assert.doesNotMatch(specFor('web-chat-terminal', undefined), /\.xterm-rows/)
  assert.doesNotMatch(specFor('web-sessions', { VITE_E2E: '1' }), /\.xterm-rows/)
  assert.doesNotMatch(specFor('web-chat-terminal-reconnecting', { VITE_E2E: '1' }), /\.xterm-rows/)
})

test('validateRecipe requires a key for press steps and accepts a valid one', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'press', key: '?' }],
    }),
  )
  assert.throws(
    () =>
      validateRecipe({ id: 'v', surface: 'web', capture: 'video', steps: [{ action: 'press' }] }),
    /video press step requires key/,
  )
})

test('buildSpec video drives a native select with selectOption and reloads in place', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-filter-flow',
      surface: 'web',
      capture: 'video',
      steps: [
        { action: 'goto', route: '/' },
        { action: 'select', selector: '[data-testid="sessions-daemon-filter"]', value: 'daemon-b' },
        { action: 'reload' },
      ],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  // A click would only open the OS popup, which never paints into the video.
  assert.match(spec, /\.selectOption\('daemon-b'\)/)
  // reload, not goto: a fresh navigation would discard the localStorage the
  // persistence proof exists to demonstrate.
  assert.match(spec, /await page\.reload\(\)/)
})

test('validateRecipe requires selector and value for select steps, allowing an empty value', () => {
  const recipe = (step) => ({ id: 'v', surface: 'web', capture: 'video', steps: [step] })
  assert.doesNotThrow(() => validateRecipe(recipe({ action: 'select', selector: 's', value: 'x' })))
  // '' is the leading "All …" filter option, so clearing a filter is valid.
  assert.doesNotThrow(() => validateRecipe(recipe({ action: 'select', selector: 's', value: '' })))
  assert.throws(
    () => validateRecipe(recipe({ action: 'select', value: 'x' })),
    /video select step requires selector/,
  )
  assert.throws(
    () => validateRecipe(recipe({ action: 'select', selector: 's' })),
    /video select step requires value/,
  )
  assert.doesNotThrow(() => validateRecipe(recipe({ action: 'reload' })))
})

// The video-step contract is encoded twice — VIDEO_ACTIONS + the validateRecipe
// ladder here, and the action enum in proof/recipes/schema.json — and the two
// HAVE drifted: `press` was accepted by this runner while the schema enum
// omitted it, and the step schema is additionalProperties:false, so valid
// recipes were schema-invalid until someone noticed by hand. Pin the schema to
// the runner's own constant, the way proof-scenario.test.mjs pins the scenarios
// schema to STEP_OPS, so a one-sided addition fails here instead.
test('recipe schema step actions agree with the runner VIDEO_ACTIONS set', () => {
  const schema = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/schema.json', import.meta.url), 'utf8'),
  )
  const stepSchema = schema.$defs.browserRecipe.allOf[1].properties.steps.items
  assert.deepEqual(
    [...stepSchema.properties.action.enum].sort(),
    [...VIDEO_ACTIONS].sort(),
    'proof/recipes/schema.json step actions must match proof-playwright-runner.mjs VIDEO_ACTIONS',
  )
})

test('buildSpec video: emits test.use slowMo with default 350 when recipe has no slowMo', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /test\.use\(\{ launchOptions:/)
  assert.match(spec, /slowMo: 350/)
})

test('buildSpec video: bakes recipe.slowMo as numeric literal when provided', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      surface: 'web',
      capture: 'video',
      slowMo: 700,
      steps: [{ action: 'goto', route: '/' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /slowMo: 700/)
  assert.doesNotMatch(spec, /slowMo: 350/)
})

test('buildSpec video branch preserves playwright baseURL for relative goto steps', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })

  assert.match(spec, /async \(\{ browser, baseURL \}\)/)
  assert.match(spec, /baseURL,/)
  assert.doesNotMatch(spec, /chromium\.launch/)
})

test('validateRecipe accepts a video recipe and rejects an unknown action', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    }),
  )
  assert.throws(
    () =>
      validateRecipe({ id: 'v', surface: 'web', capture: 'video', steps: [{ action: 'nope' }] }),
    /unsupported video step action/,
  )
})

test('validateRecipe rejects incomplete video steps before playwright starts', () => {
  assert.throws(
    () =>
      validateRecipe({ id: 'v', surface: 'web', capture: 'video', steps: [{ action: 'goto' }] }),
    /video goto step requires route/,
  )
  assert.throws(
    () =>
      validateRecipe({ id: 'v', surface: 'web', capture: 'video', steps: [{ action: 'click' }] }),
    /video click step requires selector/,
  )
  assert.throws(
    () =>
      validateRecipe({
        id: 'v',
        surface: 'web',
        capture: 'video',
        steps: [{ action: 'type', selector: 'input' }],
      }),
    /video type step requires value/,
  )
})

test('rejects recipe ids that are unsafe as screenshot filenames', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-bad-id-'))
  const recipePath = path.join(dir, 'recipe.json')
  const outputDir = path.join(dir, 'out')
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'bad/id',
      route: '/',
      selector: 'main',
    }),
  )

  const result = runRunner(['--surface', 'web', '--recipe', recipePath, '--output-dir', outputDir])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /invalid recipe id/)
  assert.equal(fs.existsSync(path.join(outputDir, 'bad')), false)
})

test('rejects malformed browser proof surface ids before playwright starts', () => {
  // BOS-202: surfaces are no longer a closed allowlist (a consumer declares its
  // own), but the id must still be a safe slug — a malformed id is rejected.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-bad-surface-'))
  const recipePath = path.join(dir, 'recipe.json')
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: '/',
      selector: 'main',
    }),
  )

  const result = runRunner([
    '--surface',
    'Bad_Surface',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /invalid --surface/)
})

test('rejects external browser proof routes before playwright starts', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-external-route-'))
  const recipePath = path.join(dir, 'recipe.json')
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: 'http://localhost:3000/admin',
      selector: 'main',
    }),
  )

  const result = runRunner([
    '--surface',
    'marketing',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /proof browser route must be relative/)
})

test('rejects protocol-relative browser proof routes before playwright starts', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-protocol-route-'))
  const recipePath = path.join(dir, 'recipe.json')
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: '//example.com/admin',
      selector: 'main',
    }),
  )

  const result = runRunner([
    '--surface',
    'marketing',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /proof browser route must be relative/)
})

test('rejects backslash browser proof routes before playwright starts', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-backslash-route-'))
  const recipePath = path.join(dir, 'recipe.json')
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: '/\\example.com/admin',
      selector: 'main',
    }),
  )

  const result = runRunner([
    '--surface',
    'marketing',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /proof browser route must be relative/)
})

// ── slugify ───────────────────────────────────────────────────────────────────

test('slugify: basic cases', () => {
  assert.equal(slugify('Open home'), 'open-home')
  assert.equal(slugify('  A/B  '), 'a-b')
  assert.equal(slugify(''), 'step')
  assert.equal(slugify('goto'), 'goto')
})

// ── buildSpec video: per-step stills + video-meta.json ───────────────────────

const labelledVideoRecipe = {
  id: 'web-new-session-flow',
  surface: 'web',
  capture: 'video',
  cropToSelector: '.data-table-wrap',
  viewport: { width: 1440, height: 1000 },
  steps: [
    { action: 'goto', route: '/', label: 'Open home' },
    { action: 'wait', selector: '.data-table-wrap', label: 'Sessions list' },
    { action: 'click', selector: '.data-table-wrap .row-clickable', label: 'Open session' },
    { action: 'wait', timeoutMs: 800, label: 'Session view' },
  ],
}

test('buildSpec video: emits captureStill call after each step', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  // Should have exactly 4 captureStill calls
  const matches = spec.match(/captureStill\(/g) ?? []
  assert.equal(matches.length, 4 + 1) // 4 calls + 1 helper definition
})

test('buildSpec video: still filenames use 2-digit NN and slug', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /01-open-home\.png/)
  assert.match(spec, /02-sessions-list\.png/)
  assert.match(spec, /03-open-session\.png/)
  assert.match(spec, /04-session-view\.png/)
})

test('buildSpec video: emits __stills.push with fileName and label', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /__stills\.push\(\{ fileName: '01-open-home\.png', label: 'Open home' \}\)/)
  assert.match(
    spec,
    /__stills\.push\(\{ fileName: '02-sessions-list\.png', label: 'Sessions list' \}\)/,
  )
  assert.match(
    spec,
    /__stills\.push\(\{ fileName: '03-open-session\.png', label: 'Open session' \}\)/,
  )
  assert.match(
    spec,
    /__stills\.push\(\{ fileName: '04-session-view\.png', label: 'Session view' \}\)/,
  )
})

test('buildSpec video: writes video-meta.json with cropHeight and stills', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /video-meta\.json/)
  assert.match(spec, /cropHeight/)
  assert.match(spec, /__stills/)
})

test('buildSpec video: preserves the tallest measured crop height across stills', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /__cropHeight = Math\.max\(__cropHeight \?\? 0, __h\)/)
  assert.doesNotMatch(spec, /__cropHeight = __h;/)
})

test('buildSpec video: disables crop metadata after any full-frame still fallback', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /let __disableCrop = false;/)
  assert.match(spec, /if \(__h === null\) __disableCrop = true;/)
  assert.match(spec, /cropHeight: __disableCrop \? null : __cropHeight/)
})

test('buildSpec video: uses recipe cropToSelector when present', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /\.data-table-wrap/)
})

test('buildSpec video: uses #root default for web surface without cropToSelector', () => {
  const recipe = {
    id: 'web-flow',
    surface: 'web',
    capture: 'video',
    viewport: { width: 1440, height: 1000 },
    steps: [{ action: 'goto', route: '/', label: 'Open home' }],
  }
  const spec = buildSpec({ recipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /'#root'/)
})

test('buildSpec video: uses main default for marketing surface without cropToSelector', () => {
  const recipe = {
    id: 'mkt-flow',
    surface: 'marketing',
    capture: 'video',
    viewport: { width: 1440, height: 1000 },
    steps: [{ action: 'goto', route: '/', label: 'Open home' }],
  }
  const spec = buildSpec({ recipe, outputDir: '/tmp/out', surface: 'marketing' })
  assert.match(spec, /'main'/)
})

// ── Behavior 2: overlay harness ───────────────────────────────────────────────

test('buildSpec video: injects overlay harness with addInitScript, __proofOverlay, pointer-events:none, and z-index', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /addInitScript/)
  assert.match(spec, /__proofOverlay/)
  assert.match(spec, /pointer-events:none/)
  assert.match(spec, /2147483647/)
})

test('buildSpec video: emits caption call for step with caption field', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'goto', route: '/', caption: 'Hi' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /window\.__proofOverlay\?\.caption\(/)
  assert.match(spec, /'Hi'/)
})

test('buildSpec video: goto step emits its caption AFTER the navigation', () => {
  // A caption set before goto would be destroyed by the navigation, so for goto
  // steps the caption must be emitted after page.goto() completes.
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'goto', route: '/', caption: 'Open home' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  const gotoIdx = spec.indexOf('page.goto(')
  const captionIdx = spec.indexOf('window.__proofOverlay?.caption(')
  assert.ok(gotoIdx >= 0 && captionIdx >= 0, 'spec should contain goto and caption')
  assert.ok(captionIdx > gotoIdx, 'goto caption must be emitted after page.goto()')
})

test('buildSpec video: scrolls click target into view before measuring ripple position', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'click', selector: 'button' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /window\.__proofOverlay\?\.ripple\(/)
  assert.match(spec, /boundingBox\(\)/)
  const actionBlock = spec.slice(spec.indexOf("const __loc = page.locator('button').first();"))
  assert.ok(
    actionBlock.indexOf('scrollIntoViewIfNeeded()') < actionBlock.indexOf('boundingBox()'),
    'click ripple must measure after target is in the viewport',
  )
})

test('buildSpec video: scrolls type target into view before measuring ripple position', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'type', selector: 'input', value: 'hello' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /window\.__proofOverlay\?\.ripple\(/)
  assert.match(spec, /boundingBox\(\)/)
  const actionBlock = spec.slice(spec.indexOf("const __loc = page.locator('input').first();"))
  assert.ok(
    actionBlock.indexOf('scrollIntoViewIfNeeded()') < actionBlock.indexOf('boundingBox()'),
    'type ripple must measure after target is in the viewport',
  )
})

test('buildSpec video: step without caption does NOT emit caption call', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.doesNotMatch(spec, /window\.__proofOverlay\?\.caption\(/)
})

// ── validateRecipe: label field ───────────────────────────────────────────────

test('validateRecipe: video step with label: "" throws', () => {
  assert.throws(
    () =>
      validateRecipe({
        id: 'v',
        surface: 'web',
        capture: 'video',
        steps: [{ action: 'goto', route: '/', label: '' }],
      }),
    /video step label must be a non-empty string/,
  )
})

test('validateRecipe: video step with label: "X" passes', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/', label: 'X' }],
    }),
  )
})

test('validateRecipe: video step without label passes', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    }),
  )
})

// ── Behavior 3: scroll step ───────────────────────────────────────────────────

test('validateRecipe: scroll step with toSelector passes', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      capture: 'video',
      steps: [{ action: 'scroll', toSelector: 'main' }],
    }),
  )
})

test('validateRecipe: scroll step with byPx passes', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      capture: 'video',
      steps: [{ action: 'scroll', byPx: 600 }],
    }),
  )
})

test('validateRecipe: scroll step with neither toSelector nor byPx throws', () => {
  assert.throws(
    () =>
      validateRecipe({
        id: 'v',
        capture: 'video',
        steps: [{ action: 'scroll' }],
      }),
    /scroll step requires toSelector, byPx, or fullPage/,
  )
})

test('validateRecipe: scroll step with byPx as non-finite string throws', () => {
  assert.throws(
    () =>
      validateRecipe({
        id: 'v',
        capture: 'video',
        steps: [{ action: 'scroll', byPx: 'bad' }],
      }),
    /scroll step byPx must be a finite number/,
  )
})

test('buildSpec video: scroll with toSelector emits scrollIntoView with smooth behavior', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'scroll', toSelector: 'main' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /scrollIntoView/)
  assert.match(spec, /behavior: 'smooth'/)
  assert.match(spec, /waitForTimeout\(800\)/)
})

test('buildSpec video: scroll with byPx emits scrollBy with smooth behavior', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'scroll', byPx: 600 }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /window\.scrollBy/)
  assert.match(spec, /behavior: 'smooth'/)
  assert.match(spec, /waitForTimeout\(800\)/)
})

// ── fullPage scroll step ──────────────────────────────────────────────────────

const videoRecipe = (steps) => ({
  id: 'web-x',
  surface: 'web',
  capture: 'video',
  viewport: { width: 1440, height: 1000 },
  steps,
})

test('validateRecipe accepts a fullPage scroll step', () => {
  assert.doesNotThrow(() =>
    validateRecipe(
      videoRecipe([
        { action: 'goto', route: '/' },
        { action: 'scroll', fullPage: true },
      ]),
    ),
  )
})

test('validateRecipe still rejects a scroll step with no target', () => {
  assert.throws(
    () => validateRecipe(videoRecipe([{ action: 'goto', route: '/' }, { action: 'scroll' }])),
    /scroll step requires toSelector, byPx, or fullPage/,
  )
})

test('buildSpec emits a whole-page scroll loop for a fullPage scroll step', () => {
  const spec = buildSpec({
    recipe: videoRecipe([
      { action: 'goto', route: '/' },
      { action: 'scroll', fullPage: true },
    ]),
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /scrollHeight/)
  assert.match(spec, /window\.scrollTo/)
})

test('OVERLAY_CAPTION_CSS re-export is import-equal to the shared caption-spec module (BOS-140)', () => {
  assert.equal(OVERLAY_CAPTION_CSS, SPEC_OVERLAY_CAPTION_CSS)
  const spec = buildSpec({
    recipe: videoRecipe([{ action: 'goto', route: '/' }]),
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.ok(spec.includes(SPEC_OVERLAY_CAPTION_CSS))
})

test('overlay caption is anchored to the top of the viewport', () => {
  assert.match(OVERLAY_CAPTION_CSS, /top:\s*24px/)
  assert.doesNotMatch(OVERLAY_CAPTION_CSS, /bottom:\s*\d/)
})

test('overlay caption reserves horizontal space for the burned-in timer', () => {
  // Wide viewports reserve the timer corner via calc; narrow (390px mobile)
  // viewports fall back to the 60% floor so the caption stays readable.
  assert.match(OVERLAY_CAPTION_CSS, /max-width:\s*max\(60%,\s*calc\(100% - 380px\)\)/)
  assert.doesNotMatch(OVERLAY_CAPTION_CSS, /max-width:\s*80%/)
})

function runRunner(args) {
  return spawnSync(process.execPath, [runnerPath, ...args], {
    cwd: repoRoot,
    encoding: 'utf8',
    timeout: 5000,
  })
}

// ── Surface-agnostic runner: spec-root, crop, stage env via CLI args (BOS-202) ──

test('parseArgs accepts an arbitrary surface when spec-root is supplied', () => {
  const parsed = parseArgs([
    '--surface',
    'portal',
    '--recipe',
    'r.json',
    '--output-dir',
    'o',
    '--spec-root',
    'e2e',
    '--default-crop',
    'main',
  ])
  assert.equal(parsed.surface, 'portal')
  assert.equal(parsed['spec-root'], 'e2e')
  assert.equal(parsed['default-crop'], 'main')
})

test('parseArgs still rejects a malformed surface id', () => {
  assert.throws(
    () => parseArgs(['--surface', 'Bad Surface', '--recipe', 'r.json', '--output-dir', 'o']),
    /invalid --surface/,
  )
})

test('stageEnvForArgs preserves legacy direct web staging without --stage-env', () => {
  assert.deepEqual(stageEnvForArgs({ surface: 'web' }), { VITE_E2E: '1' })
  assert.equal(stageEnvForArgs({ surface: 'marketing' }), undefined)
  assert.deepEqual(stageEnvForArgs({ surface: 'web', 'stage-env': '{"CUSTOM":"1"}' }), {
    CUSTOM: '1',
  })
})

test('buildSpec video honors an explicit defaultCrop over the surface heuristic', () => {
  const spec = buildSpec({
    recipe: {
      id: 'v',
      surface: 'docs',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
      viewport: { width: 1280, height: 1000 },
    },
    outputDir: '/out',
    surface: 'docs',
    defaultCrop: '.docs-main',
  })
  assert.ok(spec.includes('.docs-main'))
})

test('buildSpec video falls back to the surface crop heuristic when no defaultCrop', () => {
  const spec = buildSpec({
    recipe: {
      id: 'v',
      surface: 'marketing',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
      viewport: { width: 1280, height: 1000 },
    },
    outputDir: '/out',
    surface: 'marketing',
  })
  assert.ok(spec.includes("'main'"))
})

test('buildSpec stages the web fixture only when a stageEnv is supplied', () => {
  const base = {
    id: 's',
    surface: 'web',
    route: '/',
    viewport: { width: 1280, height: 1000 },
  }
  const staged = buildSpec({
    recipe: base,
    outputDir: '/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })
  assert.ok(staged.includes('window.bossanovaE2e'))
  const unstaged = buildSpec({ recipe: base, outputDir: '/out', surface: 'web' })
  assert.ok(!unstaged.includes('window.bossanovaE2e'))
})
