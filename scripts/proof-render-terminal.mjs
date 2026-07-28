#!/usr/bin/env node

import fs from 'node:fs'
import { createRequire } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { CAPTION_BAR_STYLE as SPEC_CAPTION_BAR_STYLE } from './proof-caption-spec.mjs'
import { formatCaption, trimTerminalBlankLines } from './proof-lib.mjs'

function parseArgs(argv) {
  const repoRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)))
  const parsed = {}
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i]
    const value = argv[i + 1]
    if (!key?.startsWith('--') || !value) {
      throw new Error(`invalid argument near ${key ?? '<end>'}`)
    }
    parsed[key.slice(2)] = value
  }
  for (const required of ['input', 'output', 'title']) {
    if (!parsed[required]) {
      throw new Error(`missing --${required}`)
    }
  }
  parsed.input = resolveRepoPath(parsed.input, repoRoot)
  parsed.output = resolveRepoPath(parsed.output, repoRoot)
  fs.mkdirSync(path.dirname(parsed.output), { recursive: true })
  parsed.caption = parsed.caption ?? ''
  return parsed
}

function resolveRepoPath(value, repoRoot) {
  return path.isAbsolute(value) ? value : path.join(repoRoot, value)
}

/** Parse `--strip` mode args: --caption (optional), --width, --output. */
function parseStripArgs(argv, repoRoot) {
  const parsed = {}
  for (let i = 0; i < argv.length; i += 1) {
    const key = argv[i]
    if (key === '--strip') continue
    if (!key?.startsWith('--')) throw new Error(`invalid argument near ${key ?? '<end>'}`)
    parsed[key.slice(2)] = argv[(i += 1)]
  }
  for (const required of ['width', 'output']) {
    if (!parsed[required]) throw new Error(`missing --${required}`)
  }
  const width = Number.parseInt(parsed.width, 10)
  if (!Number.isInteger(width) || width <= 0) throw new Error(`invalid --width ${parsed.width}`)
  const output = resolveRepoPath(parsed.output, repoRoot)
  fs.mkdirSync(path.dirname(output), { recursive: true })
  return { caption: parsed.caption ?? '', width, output }
}

/**
 * Parse `--manifest <file>` mode: read the JSON job array and resolve every
 * job's repo-relative `input`/`output` against repoRoot (mkdir-ing output dirs),
 * matching the single-shot modes. Each job is `{type:'terminal', input, output,
 * title, caption}` or `{type:'strip', caption, width, output}`; `type` defaults
 * to 'terminal'. Exported for unit testing.
 */
export function parseManifestArg(argv, repoRoot) {
  const idx = argv.indexOf('--manifest')
  const file = idx >= 0 ? argv[idx + 1] : undefined
  if (!file) throw new Error('missing --manifest <file>')
  const jobs = JSON.parse(fs.readFileSync(resolveRepoPath(file, repoRoot), 'utf8'))
  if (!Array.isArray(jobs)) throw new Error('--manifest file must contain a JSON array')
  return jobs.map((job) => {
    const output = resolveRepoPath(job.output, repoRoot)
    fs.mkdirSync(path.dirname(output), { recursive: true })
    const resolved = { ...job, type: job.type ?? 'terminal', output }
    if (resolved.type !== 'strip') resolved.input = resolveRepoPath(job.input, repoRoot)
    return resolved
  })
}

// Shared caption-bar styling: sourced from proof-caption-spec.mjs (BOS-140
// D8), the single source of truth for proof caption styling, so the stills
// (renderHtml) and the burned-in video strip (renderCaptionStripHtml, added in
// BOS-121) are pixel-identical and can never drift.
export const CAPTION_BAR_STYLE = SPEC_CAPTION_BAR_STYLE

/**
 * The blue caption bar markup for a (possibly empty) caption. Returns '' when
 * the caption is empty/whitespace-only — formatCaption('' or whitespace) === ''
 * — so callers omit the bar entirely. The caption is bound to a single line
 * <=140 chars by formatCaption.
 */
export function captionBarMarkup(caption) {
  const formatted = formatCaption(caption)
  return formatted
    ? `<div class="__proof-tui-caption" style="${CAPTION_BAR_STYLE}">${escapeHtml(formatted)}</div>`
    : ''
}

/**
 * The historical still floor: 1120px ≈ 140 columns, the bridge's default pty
 * width. Kept as the fallback whenever the cast's own width is unknowable, so a
 * still with no measurable text renders exactly as it did before BOS-571.
 */
export const DEFAULT_TERMINAL_MIN_WIDTH_PX = 1120

/**
 * Horizontal advance of one monospace cell in the still's `pre`, in px.
 * The `pre` is `font-size: 14px` in the `ui-monospace, SFMono-Regular, Menlo,
 * Consolas, monospace` stack; every font in it advances ~0.6em per cell (SF
 * Mono / Menlo / DejaVu Sans Mono are 0.60em, Consolas 0.55em), so 14 * 0.6.
 * This only sets a FLOOR — `width: max-content` still sizes the card from the
 * real glyph metrics — so a fractional mismatch is cosmetically harmless.
 */
export const TERMINAL_CHAR_ADVANCE_PX = 14 * 0.6

/** `pre { padding: 18px }` — one on each side of the text box. */
const TERMINAL_PRE_PADDING_PX = 18
/** `[data-proof-terminal] { border: 1px solid … }` — one on each side. */
const TERMINAL_CARD_BORDER_PX = 1

/**
 * The `min-width` (px) for a still whose rendered screen is `text`.
 *
 * The renderer never sees the cast header, but the widest line of the screen it
 * is handed IS the recorded column count, so that is the honest signal at this
 * layer. Returns `min(DEFAULT_TERMINAL_MIN_WIDTH_PX, derived)`: a narrow cast
 * (BOS-571's `terminal: {cols}`) gets a card that hugs its own width instead of
 * being letterboxed inside a mostly-empty 1120px block, while a default ~140
 * column cast derives more than 1120 and therefore keeps today's exact
 * rendering. Falls back to the default for empty / whitespace-only / non-string
 * input. Pure + exported for unit tests.
 * @param {string} text
 * @returns {number}
 */
export function deriveTerminalMinWidthPx(text) {
  if (typeof text !== 'string' || text.trim() === '') return DEFAULT_TERMINAL_MIN_WIDTH_PX
  let cols = 0
  for (const line of text.split('\n')) {
    // Array.from counts code points, so an astral glyph is one cell, not two.
    const width = Array.from(line.replace(/\r$/, '')).length
    if (width > cols) cols = width
  }
  if (cols <= 0) return DEFAULT_TERMINAL_MIN_WIDTH_PX
  const derived =
    Math.ceil(cols * TERMINAL_CHAR_ADVANCE_PX) +
    2 * TERMINAL_PRE_PADDING_PX +
    2 * TERMINAL_CARD_BORDER_PX
  return Math.min(DEFAULT_TERMINAL_MIN_WIDTH_PX, derived)
}

export function renderHtml({ title, text, caption = '' }) {
  const captionBar = captionBarMarkup(caption)
  const minWidthPx = deriveTerminalMinWidthPx(text)
  // Emitted only alongside the bar itself, so a caption-less still stays
  // byte-identical (and the existing "no __proof-tui-caption anywhere" guards
  // keep meaning what they say).
  const captionSizingRule = captionBar
    ? `
  /* The caption bar must FILL the card without SIZING it (BOS-571). It is a
     child of the max-content card, so its unwrapped single line would otherwise
     stretch the card past the terminal and re-letterbox a narrow cast:
     measured, 50 of the 406 committed scenario captions blow a 72-column card
     from 645px out to as much as 1005px. "width: 0" zeroes its max-content
     contribution while "min-width: 100%" still stretches it across the card in
     layout; the ellipsis mirrors renderCaptionStripHtml's burned-in strip. This
     is a no-op at the default ~140 columns — no committed caption is wide
     enough to stretch a 1214px card, so existing stills are unchanged. */
  .__proof-tui-caption {
    box-sizing: border-box;
    width: 0;
    min-width: 100%;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }`
    : ''
  return `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<style>
  :root {
    color-scheme: dark;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    background: #0a0d13;
  }
  body {
    margin: 0;
    padding: 24px;
    background: #0a0d13;
  }
  [data-proof-terminal] {
    /* max-content lets a wide cast hug its content; the floor below is derived
       from the cast's own width so a narrow one is not letterboxed (BOS-571). */
    width: max-content;
    min-width: ${minWidthPx}px;
    overflow: hidden;
    border: 1px solid #293241;
    border-radius: 8px;
    background: #05070b;
    color: #e6edf3;
    box-shadow: 0 18px 60px rgb(0 0 0 / 45%);
  }
  .titlebar {
    display: flex;
    gap: 8px;
    align-items: center;
    height: 36px;
    padding: 0 14px;
    background: #111827;
    border-bottom: 1px solid #293241;
    color: #9ca3af;
    font: 13px ui-sans-serif, system-ui, sans-serif;
  }
  .dot {
    width: 10px;
    height: 10px;
    border-radius: 999px;
    background: #ef4444;
  }
  .dot:nth-child(2) { background: #f59e0b; }
  .dot:nth-child(3) { background: #22c55e; }
  .label { margin-left: 8px; }
  pre {
    margin: 0;
    padding: 18px;
    white-space: pre;
    font-size: 14px;
    line-height: 1.25;
  }${captionSizingRule}
</style>
<body>
  <section data-proof-terminal>
    <div class="titlebar">
      <span class="dot"></span><span class="dot"></span><span class="dot"></span>
      <span class="label">${escapeHtml(title)}</span>
    </div>
    <pre>${escapeHtml(text)}</pre>${captionBar}
  </section>
</body>
</html>`
}

/**
 * A standalone, transparent caption-bar strip sized to the video width (BOS-121).
 * The bar fills the strip width and truncates to one line — the screenshot of
 * `[data-proof-caption-strip]` is overlaid along the BOTTOM edge of the TUI proof video (BOS-251 — the top row is the app's banner). Reuses
 * captionBarMarkup so the strip matches the stills' bar exactly.
 */
export function renderCaptionStripHtml({ caption, width }) {
  const bar =
    captionBarMarkup(caption) ||
    `<div class="__proof-tui-caption" style="${CAPTION_BAR_STYLE}">&nbsp;</div>`
  return `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<style>
  html, body { margin: 0; padding: 0; background: transparent; }
  [data-proof-caption-strip] { width: ${width}px; }
  .__proof-tui-caption {
    box-sizing: border-box;
    width: 100%;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
</style>
<body>
  <div data-proof-caption-strip>${bar}</div>
</body>
</html>`
}

function escapeHtml(value) {
  return String(value)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
}

// CLI entrypoint. Kept at the BOTTOM of the module: it runs immediately on
// `node proof-render-terminal.mjs …` and calls renderHtml/renderCaptionStripHtml,
// which read the top-level `const CAPTION_BAR_STYLE`. A `const` is in its temporal
// dead zone until its own declaration executes, so invoking these functions from a
// top-of-file entry block threw `Cannot access 'CAPTION_BAR_STYLE' before
// initialization`. Running last guarantees every const/function above is defined.
if (import.meta.url === `file://${process.argv[1]}`) {
  const repoRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)))
  const require = createRequire(path.join(repoRoot, 'services/web/package.json'))
  const { chromium } = require('@playwright/test')

  // Render one terminal-still job onto an existing page (reused across a batch).
  const shotStill = async (page, { input, output, title, caption }) => {
    const text = trimTerminalBlankLines(fs.readFileSync(input, 'utf8'))
    await page.setViewportSize({ width: 1400, height: 900 })
    await page.setContent(renderHtml({ title, text, caption: caption ?? '' }))
    await page.locator('[data-proof-terminal]').screenshot({ path: output })
  }
  // Render one caption-strip job onto an existing page (reused across a batch).
  const shotStrip = async (page, { caption, width, output }) => {
    await page.setViewportSize({ width, height: 200 })
    await page.setContent(renderCaptionStripHtml({ caption: caption ?? '', width }))
    await page
      .locator('[data-proof-caption-strip]')
      .screenshot({ path: output, omitBackground: true })
  }

  const browser = await chromium.launch()
  try {
    if (process.argv.includes('--manifest')) {
      // Batch mode: render many stills/strips in ONE browser, reusing a single
      // page, to avoid a cold Chromium launch per frame. The manifest is a JSON
      // array of jobs: {type:'terminal', input, output, title, caption} or
      // {type:'strip', caption, width, output}. Per-job failures are logged and
      // skipped (best-effort, like the per-frame loop) so one bad job never drops
      // the rest; paths are repo-relative, matching the single-shot modes.
      const jobs = parseManifestArg(process.argv.slice(2), repoRoot)
      const page = await browser.newPage({
        viewport: { width: 1400, height: 900 },
        deviceScaleFactor: 1,
      })
      for (const job of jobs) {
        try {
          if (job.type === 'strip') {
            await shotStrip(page, job)
          } else {
            await shotStill(page, job)
          }
        } catch (err) {
          process.stderr.write(
            `[proof-render-terminal] manifest job failed (${job.output}): ${err.message}\n`,
          )
        }
      }
    } else if (process.argv.includes('--strip')) {
      // Caption-strip mode (BOS-121): render JUST the caption bar to a
      // transparent PNG sized to the video width, for ffmpeg to overlay onto the
      // TUI proof video. Reuses the stills' caption-bar styling so the two paths
      // can never drift.
      const args = parseStripArgs(process.argv.slice(2), repoRoot)
      const page = await browser.newPage({
        viewport: { width: args.width, height: 200 },
        deviceScaleFactor: 1,
      })
      await shotStrip(page, args)
    } else {
      const args = parseArgs(process.argv.slice(2))
      const page = await browser.newPage({
        viewport: { width: 1400, height: 900 },
        deviceScaleFactor: 1,
      })
      await shotStill(page, args)
    }
  } finally {
    await browser.close()
  }
}
