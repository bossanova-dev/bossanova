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
  OVERLAY_CAPTION_CSS,
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

test('rejects unsupported browser proof surfaces before playwright starts', () => {
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
    'desktop',
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
