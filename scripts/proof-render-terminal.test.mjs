import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import nodePath from 'node:path'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import {
  CAPTION_BAR_STYLE,
  DEFAULT_TERMINAL_MIN_WIDTH_PX,
  TERMINAL_CHAR_ADVANCE_PX,
  captionBarMarkup,
  deriveTerminalMinWidthPx,
  parseManifestArg,
  renderCaptionStripHtml,
  renderHtml,
} from './proof-render-terminal.mjs'

/** A `cols`-wide, `rows`-tall block of screen text, like a rendered cast frame. */
function screen(cols, rows = 3) {
  return Array.from({ length: rows }, () => 'x'.repeat(cols)).join('\n')
}

// Regression guard (BOS-121): the CLI entry block calls renderHtml /
// renderCaptionStripHtml, which read the top-level `const CAPTION_BAR_STYLE`. A
// `const` is in its temporal dead zone until its own declaration runs, so a
// top-of-file entry block threw `Cannot access 'CAPTION_BAR_STYLE' before
// initialization` at runtime — yet every import-based unit test passed, because
// importing the module fully initializes the consts before the functions are
// called. Only the `node proof-render-terminal.mjs …` entry path hit it. Pin the
// source order so the entry block can never drift back above the const it needs.
test('CLI entry block runs after CAPTION_BAR_STYLE is initialized (no TDZ)', () => {
  const src = readFileSync(
    fileURLToPath(new URL('./proof-render-terminal.mjs', import.meta.url)),
    'utf8',
  )
  const constIndex = src.indexOf('export const CAPTION_BAR_STYLE')
  const entryIndex = src.indexOf('if (isMainModule(import.meta.url))')
  assert.ok(constIndex >= 0, 'CAPTION_BAR_STYLE declaration must exist')
  assert.ok(entryIndex >= 0, 'CLI entry guard must exist')
  assert.ok(
    entryIndex > constIndex,
    'CLI entry block must appear AFTER CAPTION_BAR_STYLE so the const is initialized before the entry block calls renderHtml/renderCaptionStripHtml',
  )
})

test('renderHtml emits a blue caption bar when caption is provided', () => {
  const html = renderHtml({ title: 'boss', text: 'home', caption: 'Opening cron list' })
  assert.match(html, /Opening cron list/)
  assert.match(html, /__proof-tui-caption/)
})

test('renderHtml omits the caption bar when caption is empty', () => {
  const html = renderHtml({ title: 'boss', text: 'home', caption: '' })
  assert.doesNotMatch(html, /__proof-tui-caption/)
})

test('renderHtml escapes caption text', () => {
  const html = renderHtml({ title: 'boss', text: 'home', caption: '<script>x</script>' })
  assert.doesNotMatch(html, /<script>x<\/script>/)
  assert.match(html, /&lt;script&gt;/)
})

test('renderHtml bounds an over-long caption to <=140 chars ending with an ellipsis', () => {
  const long = 'A'.repeat(200)
  const html = renderHtml({ title: 'boss', text: 'home', caption: long })
  // The full 200-char string must NOT appear; the bar carries a 139-char slice + '…'.
  assert.doesNotMatch(html, new RegExp('A'.repeat(200)))
  assert.match(html, new RegExp(`${'A'.repeat(139)}…`))
  // Extract the caption bar text and assert it is <=140 chars and ends with '…'.
  // Anchored on the ELEMENT, not the bare class token: the stylesheet also
  // names .__proof-tui-caption (its sizing rule, BOS-571).
  const m = html.match(/<div class="__proof-tui-caption"[^>]*>([^<]*)</)
  assert.ok(m, 'caption bar must be present')
  assert.ok(m[1].length <= 140, `caption text must be <=140 chars (was ${m[1].length})`)
  assert.equal(m[1].endsWith('…'), true)
})

test('renderHtml collapses a multi-line / multi-space caption to a single line', () => {
  const html = renderHtml({ title: 'boss', text: 'home', caption: 'open\n  the   dashboard' })
  // Anchored on the ELEMENT, not the bare class token: the stylesheet also
  // names .__proof-tui-caption (its sizing rule, BOS-571).
  const m = html.match(/<div class="__proof-tui-caption"[^>]*>([^<]*)</)
  assert.ok(m, 'caption bar must be present')
  assert.equal(m[1], 'open the dashboard')
  assert.doesNotMatch(m[1], /\n/)
  assert.doesNotMatch(m[1], /  /)
})

test('renderHtml omits the caption bar for a whitespace-only caption', () => {
  const html = renderHtml({ title: 'boss', text: 'home', caption: '   ' })
  assert.doesNotMatch(html, /__proof-tui-caption/)
})

// ── shared caption-bar markup (stills ↔ video parity, BOS-121) ────────────────

test('renderHtml uses the shared CAPTION_BAR_STYLE so stills and video cannot drift', () => {
  const html = renderHtml({ title: 'boss', text: 'home', caption: 'Step one' })
  assert.ok(html.includes(CAPTION_BAR_STYLE), 'stills bar must use the shared style constant')
})

test('captionBarMarkup: shared bar carries the blue style; empty caption → no bar', () => {
  const bar = captionBarMarkup('Open settings')
  assert.match(bar, /__proof-tui-caption/)
  assert.ok(bar.includes(CAPTION_BAR_STYLE))
  assert.match(bar, /Open settings/)
  assert.equal(captionBarMarkup('   '), '')
  assert.equal(captionBarMarkup(''), '')
})

// ── renderCaptionStripHtml (the burned-in video strip) ────────────────────────

test('renderCaptionStripHtml: width-sized, transparent, single screenshot target, shared bar', () => {
  const html = renderCaptionStripHtml({ caption: 'Toggling the flag', width: 1120 })
  assert.match(html, /data-proof-caption-strip/) // the element the strip mode screenshots
  assert.match(html, /width: 1120px/)
  assert.match(html, /background: transparent/) // omitBackground → transparent overlay
  assert.ok(html.includes(CAPTION_BAR_STYLE), 'strip reuses the stills bar style')
  assert.match(html, /Toggling the flag/)
})

test('renderCaptionStripHtml: escapes caption text and collapses whitespace', () => {
  const html = renderCaptionStripHtml({ caption: 'a <b>\n  c', width: 800 })
  assert.doesNotMatch(html, /<b>/)
  assert.match(html, /&lt;b&gt;/)
  assert.match(html, /a &lt;b&gt; c/) // formatCaption collapsed the newline + spaces
})

test('renderCaptionStripHtml: an empty caption still yields a screenshot target', () => {
  // planCaptionWindows never asks for an empty strip, but the renderer must not
  // crash the locator if it ever does — the wrapper element is always present.
  const html = renderCaptionStripHtml({ caption: '', width: 640 })
  assert.match(html, /data-proof-caption-strip/)
})

// ── deriveTerminalMinWidthPx (cast-derived still floor, BOS-571) ─────────────

test('deriveTerminalMinWidthPx: a narrow cast yields a floor proportional to its columns', () => {
  const derived = deriveTerminalMinWidthPx(screen(72))
  assert.ok(
    derived < DEFAULT_TERMINAL_MIN_WIDTH_PX,
    `a 72-column screen must not be floored at the 140-column default (got ${derived})`,
  )
  // The floor tracks the pre's own text box: cols * advance + padding + borders.
  const expected = Math.ceil(72 * TERMINAL_CHAR_ADVANCE_PX) + 2 * 18 + 2 * 1
  assert.equal(derived, expected)
})

test('deriveTerminalMinWidthPx: the floor scales with the recorded column count', () => {
  const narrow = deriveTerminalMinWidthPx(screen(60))
  const wider = deriveTerminalMinWidthPx(screen(90))
  assert.ok(wider > narrow, 'a wider cast must derive a wider floor')
  // ~30 columns of difference, within a rounding pixel.
  assert.ok(Math.abs(wider - narrow - 30 * TERMINAL_CHAR_ADVANCE_PX) <= 1)
})

test('deriveTerminalMinWidthPx: measures the WIDEST line, not the first or last', () => {
  const ragged = ['short', 'x'.repeat(72), ''].join('\n')
  assert.equal(deriveTerminalMinWidthPx(ragged), deriveTerminalMinWidthPx(screen(72)))
})

test('deriveTerminalMinWidthPx: a ~140-column cast still yields exactly the 1120px default', () => {
  // Byte-identical guard: every scenario committed before BOS-571 records 140
  // columns, whose derived floor exceeds 1120, so min-width stays 1120 and
  // `width: max-content` keeps sizing the card exactly as it does today.
  assert.equal(deriveTerminalMinWidthPx(screen(140)), DEFAULT_TERMINAL_MIN_WIDTH_PX)
  assert.equal(deriveTerminalMinWidthPx(screen(300)), DEFAULT_TERMINAL_MIN_WIDTH_PX)
})

test('deriveTerminalMinWidthPx: falls back to 1120 when the width is unknowable', () => {
  for (const input of ['', '   ', '\n\n', undefined, null, 140, {}]) {
    assert.equal(
      deriveTerminalMinWidthPx(input),
      DEFAULT_TERMINAL_MIN_WIDTH_PX,
      `expected the 1120px fallback for ${JSON.stringify(input)}`,
    )
  }
})

test('renderHtml: min-width tracks a narrow screen and stays 1120px for a wide/empty one', () => {
  const narrow = renderHtml({ title: 'boss', text: screen(72), caption: '' })
  assert.match(narrow, new RegExp(`min-width: ${deriveTerminalMinWidthPx(screen(72))}px`))
  assert.doesNotMatch(narrow, /min-width: 1120px/)

  for (const text of [screen(140), '']) {
    assert.match(
      renderHtml({ title: 'boss', text, caption: '' }),
      /min-width: 1120px/,
      'a wide or empty screen must keep the historical 1120px floor',
    )
  }
})

test('renderHtml: keeps width: max-content so a wide cast still hugs its content', () => {
  assert.match(renderHtml({ title: 'boss', text: screen(72) }), /width: max-content/)
})

test('renderHtml: the caption bar fills the card without sizing it', () => {
  // The bar is a child of the max-content card, so an unwrapped 140-char
  // caption would stretch the card past a narrow terminal and re-letterbox it
  // (measured: 50 of the 406 committed scenario captions push a 72-column card
  // from 645px to as much as 1005px). `width: 0` zeroes the max-content
  // contribution; `min-width: 100%` still fills the card during layout. Pinned
  // in source because layout is only observable in a browser.
  const html = renderHtml({ title: 'boss', text: screen(72), caption: 'A'.repeat(140) })
  const rule = html.match(/\.__proof-tui-caption \{([^}]*)\}/)
  assert.ok(rule, 'the caption bar must carry a non-contributing width rule')
  assert.match(rule[1], /width: 0;/)
  assert.match(rule[1], /min-width: 100%;/)
  assert.match(rule[1], /text-overflow: ellipsis;/)
})

// ── parseManifestArg (single-browser batch mode) ─────────────────────────────

test('parseManifestArg: resolves repo-relative paths, defaults type, mkdirs outputs', () => {
  const repoRoot = fs.mkdtempSync(nodePath.join(os.tmpdir(), 'proof-manifest-'))
  const manifest = [
    {
      input: 'raw/screen-01.txt',
      output: 'tui-agent/frame-01.png',
      title: 'boss',
      caption: 'home',
    },
    { type: 'strip', width: 1200, output: 'tui-agent/caption-strip-0.png', caption: 'step' },
  ]
  fs.writeFileSync(nodePath.join(repoRoot, 'm.json'), JSON.stringify(manifest))

  const jobs = parseManifestArg(['--manifest', 'm.json'], repoRoot)
  assert.equal(jobs[0].type, 'terminal') // defaulted
  assert.equal(jobs[0].input, nodePath.join(repoRoot, 'raw/screen-01.txt'))
  assert.equal(jobs[0].output, nodePath.join(repoRoot, 'tui-agent/frame-01.png'))
  assert.equal(jobs[1].type, 'strip')
  assert.equal(jobs[1].output, nodePath.join(repoRoot, 'tui-agent/caption-strip-0.png'))
  assert.ok(!('input' in jobs[1]), 'strip jobs carry no input path')
  // output dirs are created so the screenshot write cannot fail on a missing dir
  assert.ok(fs.existsSync(nodePath.join(repoRoot, 'tui-agent')))
})

test('parseManifestArg: missing --manifest or non-array throws', () => {
  const repoRoot = fs.mkdtempSync(nodePath.join(os.tmpdir(), 'proof-manifest-'))
  assert.throws(() => parseManifestArg([], repoRoot), /missing --manifest/)
  fs.writeFileSync(nodePath.join(repoRoot, 'bad.json'), JSON.stringify({ not: 'array' }))
  assert.throws(
    () => parseManifestArg(['--manifest', 'bad.json'], repoRoot),
    /must contain a JSON array/,
  )
})
