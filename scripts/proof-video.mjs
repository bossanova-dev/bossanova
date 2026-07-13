// Screencast post-processing: elapsed-time overlay + idle-section speedup.
//
// Proof screencasts contain long stretches where the agent is waiting on a
// generation — minutes of near-identical frames. This module (1) detects
// those stretches by frame differencing, (2) burns a mm:ss elapsed timer
// onto the ORIGINAL timeline, then (3) retimes the static stretches so each
// plays in a few seconds. Because the timer is burned before retiming, it
// visibly fast-forwards through compressed waits (timelapse-style), and a
// ">>" marker renders next to it during those stretches.
//
// The timer is rendered in pure Node (tiny bitmap font → RGBA frames piped
// to ffmpeg's `overlay` filter) because slim ffmpeg builds — including the
// current Homebrew bottle — ship without libfreetype/drawtext. `overlay`,
// `trim`, `setpts`, `concat`, and `fps` are core filters present in every
// build.
//
// Pure planning/rendering functions are exported for unit tests; only
// `postprocessProofVideo` touches ffmpeg and the filesystem.

import { spawnSync } from 'node:child_process'
import { copyFileSync, existsSync, rmSync, writeFileSync } from 'node:fs'
import path from 'node:path'

// Intro-card ffmpeg arg builders live in proof-video-intro.mjs.
import { INTRO_SEC, buildIntroClipArgs, buildIntroConcatArgs } from './proof-video-intro.mjs'
import { formatCaption } from './proof-lib.mjs'

// ── Analysis constants (tuned on a real 11-min gen-AI proof recording) ──────
export const ANALYZE_FPS = 4 // diff sampling rate — 250ms resolution
export const ANALYZE_W = 96
export const ANALYZE_H = 54
/** Mean-abs-diff (0-255 scale) below which two sampled frames count as "the
 * same". Loose enough to absorb spinners/cursor blinks, far below the diff
 * any real action (typing, spawn, pan) produces. */
export const STATIC_DIFF_THRESHOLD = 1.5
/** Only stretches at least this long are compressed. */
export const MIN_STATIC_RUN_MS = 8_000
/** Real-time breathing room kept at each end of a compressed stretch. */
export const RETIME_PAD_MS = 1_000
/** Playback duration each compressed stretch is squeezed into. */
export const RETIME_TARGET_MS = 4_000
/** Settle pad kept after the last content screen when a trailing cut applies. */
export const TRAILING_CUT_PAD_MS = 800
/** Minimum OUTPUT duration of each burned caption window (readability floor). */
export const CAPTION_MIN_OUTPUT_MS = 2_000
export const OUTPUT_FPS = 30

// ── Leading white-flash trim ────────────────────────────────────────────────
// Chrome/Playwright paint a blank white page for a few hundred ms before the
// app mounts, leaving a white flash at the START of the recording. The app is
// dark (canvas YAVG ~25-33) and the flash is near-white (YAVG ~234) — a clean
// ~200-point gap — so a leading run of bright frames is unambiguous for THIS
// app and cannot false-trigger on dark gen-AI content.
/** Mean-luma (0-255) above which a sampled frame counts as the browser's white pre-roll. */
export const LEADING_BLANK_LUMA = 200
/** Safety buffer trimmed past the last detected white frame. */
export const LEADING_BLANK_BUFFER_MS = 150
/** Hard cap so a pathological all-bright start can never gut real content. */
export const LEADING_BLANK_CAP_MS = 2_000

// ── Leading static-frames trim ───────────────────────────────────────────────
// VHS-recorded TUI sessions begin with a dark, completely static terminal
// during the ~2.5s boot Sleep before the app paints. detectLeadingBlankMs
// ignores dark starts, so this brightness-independent fallback covers it.
// It also detects the browser white-flash (which is likewise a static run),
// but detectLeadingBlankMs fires first via short-circuit and takes precedence.
/**
 * Mean-abs-diff (0-255 scale) below which consecutive frames are considered
 * static for the purpose of leading-trim detection. Matches STATIC_DIFF_THRESHOLD
 * (1.5) in magnitude: absorbs VHS cursor blinks (~0.05 on 96×54 grayscale) and
 * is far below any real-action diff (typing, spawning a process: ≥ 5). A
 * dedicated constant lets leading-trim and idle-speedup thresholds evolve
 * independently.
 */
export const LEADING_STATIC_DIFF = 1.5

// ── Pure: frame differencing ─────────────────────────────────────────────────

/**
 * Mean absolute difference between each consecutive pair of same-size
 * grayscale frames packed in `raw`.
 * @param {Buffer|Uint8Array} raw @param {number} frameBytes
 * @returns {number[]} length = frameCount - 1
 */
export function computeFrameDiffs(raw, frameBytes) {
  const frames = Math.floor(raw.length / frameBytes)
  const diffs = []
  for (let f = 1; f < frames; f++) {
    const a = (f - 1) * frameBytes
    const b = f * frameBytes
    let sum = 0
    for (let i = 0; i < frameBytes; i++) sum += Math.abs(raw[b + i] - raw[a + i])
    diffs.push(sum / frameBytes)
  }
  return diffs
}

/**
 * Find maximal runs of consecutive sub-threshold diffs at least minRunMs
 * long. diffs[k] compares frame k to k+1, so a run over diffs [i..j] spans
 * frame i (at i/fps) through frame j+1.
 * @param {number[]} diffs
 * @param {{fps?: number, threshold?: number, minRunMs?: number}} [opts]
 * @returns {Array<{startMs: number, endMs: number}>}
 */
export function findStaticRuns(diffs, opts = {}) {
  const fps = opts.fps ?? ANALYZE_FPS
  const threshold = opts.threshold ?? STATIC_DIFF_THRESHOLD
  const minRunMs = opts.minRunMs ?? MIN_STATIC_RUN_MS
  const runs = []
  let runStart = -1
  const flush = (endIdx) => {
    if (runStart < 0) return
    const startMs = (runStart / fps) * 1000
    const endMs = ((endIdx + 1) / fps) * 1000
    if (endMs - startMs >= minRunMs)
      runs.push({ startMs: Math.round(startMs), endMs: Math.round(endMs) })
    runStart = -1
  }
  for (let k = 0; k < diffs.length; k++) {
    if (diffs[k] < threshold) {
      if (runStart < 0) runStart = k
    } else {
      flush(k - 1)
    }
  }
  flush(diffs.length - 1)
  return runs
}

/**
 * Mean byte value of each same-size frame packed in `raw` (grayscale → mean
 * luminance). Pairs with computeFrameDiffs over the same extracted buffer.
 * @param {Buffer|Uint8Array} raw @param {number} frameBytes
 * @returns {number[]} length = frameCount
 */
export function computeFrameLuma(raw, frameBytes) {
  const frames = Math.floor(raw.length / frameBytes)
  const luma = []
  for (let f = 0; f < frames; f++) {
    const start = f * frameBytes
    let sum = 0
    for (let i = 0; i < frameBytes; i++) sum += raw[start + i]
    luma.push(sum / frameBytes)
  }
  return luma
}

/**
 * Milliseconds of leading near-white pre-roll to trim from a recording. Counts
 * the LEADING run of frames brighter than `threshold` and returns runLen +
 * bufferMs, clamped to capMs. Returns 0 (NO trim) when there is no leading-white
 * run — a dark-start (gen-AI) recording is never trimmed.
 *
 * Returns 0 when EVERY sampled frame is bright: the white-flash heuristic relies
 * on a bright pre-roll giving way to darker app content, but a light-themed
 * surface (the bossanova web app, marketing pages) stays bright throughout. With
 * no dark content to anchor against, an all-bright run is the app itself, not a
 * flash, so trimming it would gut real content (and on a short clip leave an
 * empty video).
 * @param {number[]} luma per-frame mean luminance (from computeFrameLuma)
 * @param {{fps?: number, threshold?: number, bufferMs?: number, capMs?: number}} [opts]
 * @returns {number}
 */
export function detectLeadingBlankMs(luma, opts = {}) {
  const fps = opts.fps ?? ANALYZE_FPS
  const threshold = opts.threshold ?? LEADING_BLANK_LUMA
  const bufferMs = opts.bufferMs ?? LEADING_BLANK_BUFFER_MS
  const capMs = opts.capMs ?? LEADING_BLANK_CAP_MS
  let n = 0
  while (n < luma.length && luma[n] > threshold) n++
  if (n === 0) return 0
  if (n === luma.length) return 0 // all-bright → light app, not a flash
  const trimMs = Math.round((n / fps) * 1000 + bufferMs)
  return Math.min(trimMs, capMs)
}

/**
 * Milliseconds of leading static (near-identical frames) to trim from a
 * recording. Counts the LEADING run of consecutive frame-diffs below `eps`
 * and returns runLen + bufferMs, clamped to capMs.
 *
 * Returns 0 when there is no leading static run (n === 0) — a recording that
 * starts with visible activity is left untouched.
 *
 * Returns 0 when EVERY diff is sub-eps (n === diffs.length) — an all-static
 * clip (e.g. a mis-captured blank) is not a lead-in; trimming it would gut
 * the video.
 *
 * Unlike detectLeadingBlankMs, this function is brightness-independent: it
 * catches both the browser's near-white pre-roll (also a static run) and the
 * TUI's dark-blank terminal boot. It is used as the FALLBACK — detectLeadingBlankMs
 * is tried first and short-circuits when it fires (the dark-app browser path,
 * which always opens on a white flash, is therefore unchanged); this fallback
 * runs only when the bright detector returns 0 (dark/blank TUI boot, or a
 * light/all-bright surface whose static pre-roll the brightness heuristic
 * cannot safely trim).
 *
 * `diffs[k]` compares frame k to k+1 (length = frameCount - 1); the fps/ms
 * math mirrors detectLeadingBlankMs and findStaticRuns.
 *
 * @param {number[]} diffs consecutive-frame diff array from computeFrameDiffs
 * @param {{fps?: number, eps?: number, bufferMs?: number, capMs?: number}} [opts]
 * @returns {number}
 */
export function detectLeadingStaticMs(diffs, opts = {}) {
  const fps = opts.fps ?? ANALYZE_FPS
  const eps = opts.eps ?? LEADING_STATIC_DIFF
  const bufferMs = opts.bufferMs ?? LEADING_BLANK_BUFFER_MS
  const capMs = opts.capMs ?? LEADING_BLANK_CAP_MS
  let n = 0
  while (n < diffs.length && diffs[n] < eps) n++
  if (n === 0) return 0
  if (n === diffs.length) return 0 // all-static → not a lead-in
  const trimMs = Math.round((n / fps) * 1000 + bufferMs)
  return Math.min(trimMs, capMs)
}

// ── Pure: retime planning ────────────────────────────────────────────────────

/**
 * Turn static runs into a full segmentation of [0, durationMs]: 1x segments
 * interleaved with sped-up ones. Each static run keeps padMs of real time at
 * both ends; the middle is compressed to targetMs (skipped when it wouldn't
 * shrink). Speeds are rounded to 2 decimals for the ffmpeg expression.
 * @param {Array<{startMs: number, endMs: number}>} staticRuns sorted, non-overlapping
 * @param {number} durationMs
 * @param {{padMs?: number, targetMs?: number}} [opts]
 * @returns {Array<{startMs: number, endMs: number, speed: number}>}
 */
export function planRetime(staticRuns, durationMs, opts = {}) {
  const padMs = opts.padMs ?? RETIME_PAD_MS
  const targetMs = opts.targetMs ?? RETIME_TARGET_MS
  const segments = []
  let cursor = 0
  for (const run of staticRuns) {
    const fastStart = Math.max(run.startMs + padMs, cursor)
    const fastEnd = Math.min(run.endMs - padMs, durationMs)
    const fastLen = fastEnd - fastStart
    if (fastLen <= targetMs) continue
    if (fastStart > cursor) segments.push({ startMs: cursor, endMs: fastStart, speed: 1 })
    segments.push({
      startMs: fastStart,
      endMs: fastEnd,
      speed: Math.round((fastLen / targetMs) * 100) / 100,
    })
    cursor = fastEnd
  }
  if (cursor < durationMs) segments.push({ startMs: cursor, endMs: durationMs, speed: 1 })
  return segments
}

/** Output duration (ms) of a retime plan. */
export function retimedDurationMs(segments) {
  return Math.round(segments.reduce((t, s) => t + (s.endMs - s.startMs) / s.speed, 0))
}

/** Minimum OUTPUT duration each brief scene occupies in the final video so a
 * human can register what happened before the next scene starts (BOS-251 —
 * scenario replays produced ~2s videos with scenes at 0:00 and 0:01). */
export const MIN_SCENE_OUTPUT_MS = 3_000

/**
 * Enforce a per-scene watchability floor on a retime plan: any scene window
 * whose planned OUTPUT duration is under `minSceneMs` is slowed (speed < 1 —
 * the same setpts mechanism the idle speedup uses, in reverse) until it holds
 * the screen for `minSceneMs`. `sceneStartsMs` are window-opening timestamps on
 * the SAME (trimmed) clock the segments use; the last window runs to
 * `durationMs`. Non-finite entries (null cast reads) are dropped — their scenes
 * simply keep the neighbouring window's pacing. Pure; returns a new segment
 * list that still tiles [0, durationMs].
 * @param {Array<{startMs:number,endMs:number,speed:number}>} segments planRetime output
 * @param {Array<number>} sceneStartsMs
 * @param {number} durationMs
 * @param {number} [minSceneMs]
 * @returns {Array<{startMs:number,endMs:number,speed:number}>}
 */
export function applySceneFloors(
  segments,
  sceneStartsMs,
  durationMs,
  minSceneMs = MIN_SCENE_OUTPUT_MS,
) {
  const bounds = [
    ...new Set(
      (sceneStartsMs ?? [])
        .filter((b) => Number.isFinite(b))
        .map((b) => Math.min(Math.max(0, Math.round(b)), durationMs)),
    ),
  ].sort((a, b) => a - b)
  if (durationMs <= 0 || bounds.length === 0) return segments
  if (bounds[0] !== 0) bounds.unshift(0)
  const windows = bounds.map((b, i) => ({ startMs: b, endMs: bounds[i + 1] ?? durationMs }))

  // Split the plan at window boundaries so every piece lies inside one window.
  const cuts = bounds.filter((c) => c > 0 && c < durationMs)
  const pieces = []
  for (const s of segments) {
    let start = s.startMs
    for (const c of [...cuts.filter((c) => c > s.startMs && c < s.endMs), s.endMs]) {
      if (c > start) pieces.push({ startMs: start, endMs: c, speed: s.speed })
      start = c
    }
  }

  for (const w of windows) {
    if (w.endMs <= w.startMs) continue
    const inWindow = pieces.filter((p) => p.startMs >= w.startMs && p.endMs <= w.endMs)
    const outLen = inWindow.reduce((t, p) => t + (p.endMs - p.startMs) / p.speed, 0)
    if (outLen <= 0 || outLen >= minSceneMs) continue
    const factor = outLen / minSceneMs
    for (const p of inWindow) {
      p.speed = Math.max(0.01, Math.round(p.speed * factor * 100) / 100)
    }
  }
  return pieces
}

/**
 * Map a SOURCE-clock timestamp (cast clock for TUI, wall-offset for web) to the
 * OUTPUT mp4 clock, through: leading trim (trimMs), the idle-speedup retime
 * (segments from planRetime — they tile [0, durationMs], proof-video.mjs:227-247),
 * and the prepended intro card (introMs). This is the numeric twin of what the
 * burned captions get for free by being overlaid BEFORE pass 2 — chapter links
 * need the number. Pure. Returns null for a null/absent timeline (degrade:
 * chapter rendered without a timestamp link).
 * @param {{trimMs:number, segments:Array<{startMs:number,endMs:number,speed:number}>, introMs:number}|null} timeline
 * @param {number} sourceMs
 * @returns {number|null}
 */
export function mapSourceToOutputMs(timeline, sourceMs) {
  if (!timeline || !Number.isFinite(sourceMs)) return null
  const { trimMs = 0, segments = [], introMs = 0 } = timeline
  const t = Math.max(0, sourceMs - trimMs)
  let out = 0
  for (const s of segments) {
    if (t >= s.endMs) {
      out += (s.endMs - s.startMs) / s.speed
      continue
    }
    out += Math.max(0, t - s.startMs) / s.speed
    break
  }
  if (segments.length === 0) out = t
  return Math.round(introMs + out)
}

/**
 * Seconds (original timeline) that fall inside a sped-up segment — these
 * timer frames get the ">>" fast-forward marker.
 * @param {Array<{startMs: number, endMs: number, speed: number}>} segments
 * @returns {Set<number>}
 */
export function fastForwardSeconds(segments) {
  const out = new Set()
  for (const s of segments) {
    if (s.speed <= 1) continue
    for (let sec = Math.floor(s.startMs / 1000); sec * 1000 < s.endMs; sec++) out.add(sec)
  }
  return out
}

/**
 * filter_complex that plays each segment at its speed and concatenates.
 * Input timestamps are seconds on the source (timed) video.
 * @param {Array<{startMs: number, endMs: number, speed: number}>} segments
 * @returns {string}
 */
export function buildRetimeFilter(segments) {
  const parts = []
  const labels = []
  segments.forEach((s, i) => {
    const a = (s.startMs / 1000).toFixed(3)
    const b = (s.endMs / 1000).toFixed(3)
    parts.push(`[0:v]trim=start=${a}:end=${b},setpts=(PTS-STARTPTS)/${s.speed}[s${i}]`)
    labels.push(`[s${i}]`)
  })
  parts.push(`${labels.join('')}concat=n=${segments.length}:v=1:a=0,fps=${OUTPUT_FPS}[v]`)
  return parts.join(';')
}

// ── Pure: timer strip rendering (bitmap font → RGBA frames) ─────────────────

// 8x12 pixel font, scaled 2x at render time. Only the glyphs the timer needs.
const GLYPHS = {
  0: [
    '  ####  ',
    ' ##  ## ',
    '##    ##',
    '##    ##',
    '##    ##',
    '##    ##',
    '##    ##',
    '##    ##',
    '##    ##',
    ' ##  ## ',
    '  ####  ',
    '        ',
  ],
  1: [
    '   ##   ',
    '  ###   ',
    ' ####   ',
    '   ##   ',
    '   ##   ',
    '   ##   ',
    '   ##   ',
    '   ##   ',
    '   ##   ',
    '   ##   ',
    ' ###### ',
    '        ',
  ],
  2: [
    '  ####  ',
    ' ##  ## ',
    '##    ##',
    '      ##',
    '     ## ',
    '    ##  ',
    '   ##   ',
    '  ##    ',
    ' ##     ',
    '##      ',
    '########',
    '        ',
  ],
  3: [
    '  ####  ',
    ' ##  ## ',
    '##    ##',
    '      ##',
    '   ###  ',
    '   ###  ',
    '      ##',
    '      ##',
    '##    ##',
    ' ##  ## ',
    '  ####  ',
    '        ',
  ],
  4: [
    '     ## ',
    '    ### ',
    '   #### ',
    '  ## ## ',
    ' ##  ## ',
    '##   ## ',
    '########',
    '     ## ',
    '     ## ',
    '     ## ',
    '     ## ',
    '        ',
  ],
  5: [
    '########',
    '##      ',
    '##      ',
    '##      ',
    '######  ',
    '     ## ',
    '      ##',
    '      ##',
    '##    ##',
    ' ##  ## ',
    '  ####  ',
    '        ',
  ],
  6: [
    '  ####  ',
    ' ##  ## ',
    '##      ',
    '##      ',
    '######  ',
    '##   ## ',
    '##    ##',
    '##    ##',
    '##    ##',
    ' ##  ## ',
    '  ####  ',
    '        ',
  ],
  7: [
    '########',
    '      ##',
    '     ## ',
    '     ## ',
    '    ##  ',
    '    ##  ',
    '   ##   ',
    '   ##   ',
    '  ##    ',
    '  ##    ',
    '  ##    ',
    '        ',
  ],
  8: [
    '  ####  ',
    ' ##  ## ',
    '##    ##',
    ' ##  ## ',
    '  ####  ',
    ' ##  ## ',
    '##    ##',
    '##    ##',
    '##    ##',
    ' ##  ## ',
    '  ####  ',
    '        ',
  ],
  9: [
    '  ####  ',
    ' ##  ## ',
    '##    ##',
    '##    ##',
    '##    ##',
    ' ##  ###',
    '  ### ##',
    '      ##',
    '      ##',
    ' ##  ## ',
    '  ####  ',
    '        ',
  ],
  ':': [
    '        ',
    '        ',
    '   ##   ',
    '   ##   ',
    '        ',
    '        ',
    '        ',
    '   ##   ',
    '   ##   ',
    '        ',
    '        ',
    '        ',
  ],
  '>': [
    '        ',
    '##      ',
    ' ##     ',
    '  ##    ',
    '   ##   ',
    '    ##  ',
    '   ##   ',
    '  ##    ',
    ' ##     ',
    '##      ',
    '        ',
    '        ',
  ],
  ' ': [
    '        ',
    '        ',
    '        ',
    '        ',
    '        ',
    '        ',
    '        ',
    '        ',
    '        ',
    '        ',
    '        ',
    '        ',
  ],
}
const GLYPH_W = 8
const GLYPH_H = 12
const GLYPH_GAP = 1
const SCALE = 2
const PAD = 8
// Canvas must be constant across frames; size it for the widest text.
const MAX_TEXT = '>> 88:88'
export const TIMER_W =
  (MAX_TEXT.length * GLYPH_W + (MAX_TEXT.length - 1) * GLYPH_GAP) * SCALE + PAD * 2 // 158
export const TIMER_H = GLYPH_H * SCALE + PAD * 2 // 40

/** mm:ss for a non-negative second count (hours wrap into minutes). */
export function formatElapsed(totalSeconds) {
  const m = Math.floor(totalSeconds / 60)
  const s = totalSeconds % 60
  return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`
}

function textPixelWidth(text) {
  return (text.length * GLYPH_W + (text.length - 1) * GLYPH_GAP) * SCALE
}

/** Render one RGBA timer frame: right-aligned text on a translucent pill. */
export function renderTimerFrame(text) {
  const buf = Buffer.alloc(TIMER_W * TIMER_H * 4) // transparent
  const textW = textPixelWidth(text)
  const textX = TIMER_W - PAD - textW
  // Pill hugs the text, not the canvas, so the box doesn't show dead space.
  for (let y = 0; y < TIMER_H; y++) {
    for (let x = textX - PAD; x < TIMER_W; x++) {
      const o = (y * TIMER_W + x) * 4
      buf[o] = 0
      buf[o + 1] = 0
      buf[o + 2] = 0
      buf[o + 3] = 150
    }
  }
  for (let c = 0; c < text.length; c++) {
    const glyph = GLYPHS[text[c]] ?? GLYPHS[' ']
    const gx = textX + c * (GLYPH_W + GLYPH_GAP) * SCALE
    const gy = PAD
    for (let row = 0; row < GLYPH_H; row++) {
      for (let col = 0; col < GLYPH_W; col++) {
        if (glyph[row][col] !== '#') continue
        for (let sy = 0; sy < SCALE; sy++) {
          for (let sx = 0; sx < SCALE; sx++) {
            const o = ((gy + row * SCALE + sy) * TIMER_W + gx + col * SCALE + sx) * 4
            buf[o] = 255
            buf[o + 1] = 255
            buf[o + 2] = 255
            buf[o + 3] = 255
          }
        }
      }
    }
  }
  return buf
}

/**
 * One RGBA frame per second of original video, ">>" marker during sped-up
 * stretches. Fed to ffmpeg as a 1fps rawvideo input.
 * @param {number} totalSeconds @param {Set<number>} fastSecs
 * @returns {Buffer}
 */
export function renderTimerStrip(totalSeconds, fastSecs) {
  const frames = []
  for (let sec = 0; sec <= totalSeconds; sec++) {
    const prefix = fastSecs.has(sec) ? '>> ' : ''
    frames.push(renderTimerFrame(`${prefix}${formatElapsed(sec)}`))
  }
  return Buffer.concat(frames)
}

// ── Pure: video crop helpers ─────────────────────────────────────────────────

/**
 * Returns the even-rounded crop height to apply, or `null` when no crop should
 * happen. H264 requires even-pixel dimensions.
 * @param {number|null|undefined} cropHeight raw measured content height (px)
 * @param {number} recordedHeight viewport height the video was recorded at (px)
 * @returns {number|null}
 */
export function evenCropHeight(cropHeight, recordedHeight) {
  if (cropHeight == null || !Number.isFinite(cropHeight) || cropHeight <= 0) return null
  const even = cropHeight - (cropHeight % 2)
  if (even <= 0) return null
  if (even >= recordedHeight) return null
  return even
}

/**
 * Cap the trim so the visible video never gets shorter than half its width — a
 * 600px-wide clip keeps at least 300px of height, so it stays easy to watch.
 * Raises a too-short measured crop up to the floor, never exceeds the recorded
 * height, and even-rounds for H264. Returns null when no crop should happen
 * (floor unreachable within the recorded frame).
 * @param {number|null|undefined} cropHeight measured content height (px)
 * @param {number} recordedWidth viewport width the video was recorded at (px)
 * @param {number} recordedHeight viewport height the video was recorded at (px)
 * @returns {number|null}
 */
export function applyMinHeightRatio(cropHeight, recordedWidth, recordedHeight) {
  if (cropHeight == null || !Number.isFinite(cropHeight) || cropHeight <= 0) return null
  const floor = Math.ceil(0.5 * recordedWidth)
  const raised = Math.max(cropHeight, floor)
  // even-round (H264) and reuse evenCropHeight's >= recordedHeight → null guard
  return evenCropHeight(raised, recordedHeight)
}

/**
 * Crop height to feed the encoder. Explicit measured crops keep the existing
 * behavior; recordings with no crop still need an even height for H264.
 * @param {number|null|undefined} cropHeight measured content height (px)
 * @param {number} recordedHeight viewport height the video was recorded at (px)
 * @returns {number|null}
 */
export function encoderCropHeight(cropHeight, recordedHeight) {
  const explicit = evenCropHeight(cropHeight, recordedHeight)
  if (explicit !== null) return explicit
  if (!Number.isFinite(recordedHeight) || recordedHeight <= 0) return null
  if (recordedHeight % 2 === 0) return null
  return recordedHeight - 1
}

/**
 * Build the pass-1 base-chain string `[0:v]...[base]` for the filtergraph.
 * Applies, in order: optional leading trim (when trimSec > 0), optional crop
 * (when cropHeight is a positive integer), then fps. Crop comes before fps so
 * the frame-rate conversion works on the already-cropped geometry.
 * @param {{ trimSec: number, cropHeight: number|null|undefined, fps: number }} opts
 * @returns {string}
 */
export function buildBaseChain({ trimSec, endSec, cropHeight, fps }) {
  const parts = []
  const hasEnd = Number.isFinite(endSec) && endSec > (trimSec > 0 ? trimSec : 0)
  if (trimSec > 0 || hasEnd) {
    // CFR-normalize BEFORE trimming (BOS-251): agg webms are sparse VFR — one
    // frame per TUI repaint, seconds apart. `trim=start=X` drops any frame
    // timestamped before X even though it visually spans past X, and
    // `setpts=PTS-STARTPTS` then rebases the clock to the first SURVIVING
    // frame — which can sit many seconds after X. Every burned overlay (timer,
    // captions) and the retime plan assume the rebase lands exactly at X, so
    // any run with a leading trim played its content shifted early against
    // its overlays and lost its tail to the end cut. fps-first materializes a
    // frame on every 1/fps tick, so a frame always exists at X.
    parts.push(`fps=${fps}`)
    const start = `start=${(trimSec > 0 ? trimSec : 0).toFixed(3)}`
    const end = hasEnd ? `:end=${endSec.toFixed(3)}` : ''
    parts.push(`trim=${start}${end},setpts=PTS-STARTPTS`)
  }
  if (cropHeight != null && Number.isInteger(cropHeight) && cropHeight > 0)
    parts.push(`crop=in_w:${cropHeight}:0:0`)
  if (!parts.includes(`fps=${fps}`)) parts.push(`fps=${fps}`)
  return `[0:v]${parts.join(',')}[base]`
}

// ── Pure: caption-window planning ────────────────────────────────────────────

/**
 * Turn per-step caption timings (captured from the asciinema `.cast` clock) into
 * a sequence of non-overlapping display windows on the SAME (untrimmed) cast
 * timeline. Each window runs from its caption's `startMs` to the next NON-BLANK
 * caption's `startMs`; the last window extends to `totalMs`. Blank/whitespace-only
 * captions are dropped (the preceding caption stays on screen across them), and a
 * zero/negative-width window (e.g. a caption that starts at the very end) is
 * skipped. Caption text is normalised with `formatCaption` (the same single-line
 * bound the stills use), so video captions can never drift from the stills.
 *
 * Pure: the trim offset and seconds conversion for the ffmpeg `enable` expression
 * are applied later (in postprocessProofVideo), on the same clock as the timer.
 *
 * @param {Array<{seq?: number, caption: string, startMs: number}>} timings
 * @param {number} totalMs full (untrimmed) cast/video duration in ms
 * @returns {Array<{text: string, startMs: number, endMs: number}>}
 */
export function planCaptionWindows(timings, totalMs) {
  if (!Array.isArray(timings) || timings.length === 0) return []
  const entries = timings
    .map((t) => ({ text: formatCaption(t?.caption), startMs: Number(t?.startMs) }))
    .filter((t) => Number.isFinite(t.startMs))
    .sort((a, b) => a.startMs - b.startMs)
  // STRICT per-screen windows (BOS-251): a caption narrates exactly the screen
  // it was recorded against and ends when the NEXT screen settles — blank or
  // not. Captions used to be retained across later un-narrated screens, which
  // burned stale text over screens it never described ("pressing Down…" shown
  // over a screen several actions later). A screen with no narration shows an
  // empty strip — truthful beats decorated. Readability is guaranteed by the
  // caption floor in postprocessProofVideo, not by stretching a caption across
  // foreign screens.
  const windows = []
  for (let i = 0; i < entries.length; i++) {
    if (!entries[i].text) continue
    const startMs = Math.max(0, entries[i].startMs)
    const endMs = i + 1 < entries.length ? entries[i + 1].startMs : totalMs
    if (endMs > startMs) windows.push({ text: entries[i].text, startMs, endMs })
  }
  return windows
}

/** ffmpeg `enable` expression for a caption's [startSec, endSec) display window. */
function captionEnableExpr(startSec, endSec) {
  return `enable='between(t,${startSec.toFixed(3)},${endSec.toFixed(3)})'`
}

/**
 * Pass-1 filtergraph: overlay zero or more per-step caption strips along the
 * BOTTOM edge (`y=main_h-overlay_h`, gated by an `enable` time window — the
 * bottom is dead space in every TUI screen, so the strip never obscures the UI
 * under proof; BOS-251) and, when enabled, the elapsed timer pill at the
 * top-right (`y=44`). With no overlays at all the base chain (trim/crop/fps)
 * is the whole graph. Pure string builder.
 *
 * Captions are overlaid first and the timer last; the final overlay's output
 * is `[v]`. Each caption's `inputIndex` is the ffmpeg `-i` index of its
 * rendered strip; the caller assigns those indices and feeds the matching
 * inputs in the same order.
 *
 * @param {string} baseChain e.g. '[0:v]crop=…,fps=30[base]'
 * @param {{timer:boolean, captions?: Array<{inputIndex:number, startSec:number, endSec:number}>, timerInputIndex?: number}} opts
 * @returns {string}
 */
export function buildTimerOverlayFilter(baseChain, { timer, captions = [], timerInputIndex = 1 }) {
  const ops = captions.map(
    (c) =>
      `[${c.inputIndex}:v]overlay=x=0:y=main_h-overlay_h:${captionEnableExpr(c.startSec, c.endSec)}:eof_action=repeat`,
  )
  if (timer) ops.push(`[${timerInputIndex}:v]overlay=x=main_w-overlay_w-14:y=44:eof_action=repeat`)
  if (ops.length === 0) return `${baseChain}`
  const parts = [baseChain]
  let cur = '[base]'
  ops.forEach((op, i) => {
    const out = i === ops.length - 1 ? '[v]' : `[ov${i}]`
    parts.push(`${cur}${op}${out}`)
    cur = out
  })
  return parts.join(';')
}

/**
 * ffmpeg `-i` input args for the per-step caption strips: each strip is fed as a
 * SINGLE-frame image, NEVER `-loop 1`.
 *
 * A `-loop 1` image is an INFINITE input. With `overlay …:eof_action=repeat` an
 * infinite secondary input drives the overlay output far past the (finite) base
 * clip — one looped strip stretched an 80s clip to ~1338s, and seven stacked
 * strips ran ffmpeg for 30+ minutes producing an ever-growing file before this
 * was found. As a single frame, `eof_action=repeat` already holds the caption on
 * screen for the whole clip while the per-window `enable` expression gates when it
 * is drawn, so the output stays bounded to the base duration. Pure + exported so
 * the "no -loop" invariant is unit-tested without spawning ffmpeg.
 *
 * @param {string[]} pngPaths
 * @returns {string[]}
 */
export function captionInputArgs(pngPaths) {
  return pngPaths.flatMap((png) => ['-i', png])
}

// ── Impure: ffmpeg orchestration ─────────────────────────────────────────────

const ENCODE_ARGS = [
  '-c:v',
  'libx264',
  '-preset',
  'veryfast',
  '-crf',
  '23',
  '-pix_fmt',
  'yuv420p',
  '-movflags',
  '+faststart',
  '-an',
]

// Apple-Silicon hardware H.264 encode. VideoToolbox ignores -crf, so quality is
// set via -q:v (1..100, higher = better; ~60 ≈ libx264 crf 23). Output is plain
// H.264 in mp4, byte-different from libx264 but equally playable.
const HW_ENCODE_ARGS = [
  '-c:v',
  'h264_videotoolbox',
  '-q:v',
  '60',
  '-pix_fmt',
  'yuv420p',
  '-movflags',
  '+faststart',
  '-an',
]

/**
 * Pure encoder-arg selector. Hardware (VideoToolbox) encode is OPT-IN
 * (`BOSS_PROOF_HW_ENCODE`) AND only used when actually available, so the default
 * — and every non-macOS environment such as Linux CI, which has no VideoToolbox
 * — stays on portable libx264. Exported for unit testing.
 *
 * @param {{ hwRequested: boolean, hwAvailable: boolean }} opts
 * @returns {string[]}
 */
export function selectEncoderArgs({ hwRequested, hwAvailable }) {
  return hwRequested && hwAvailable ? HW_ENCODE_ARGS : ENCODE_ARGS
}

let cachedHwAvailable
/** True when ffmpeg advertises the h264_videotoolbox encoder (cached per run). */
function videotoolboxAvailable() {
  if (cachedHwAvailable === undefined) {
    const res = spawnSync('ffmpeg', ['-hide_banner', '-encoders'], { encoding: 'utf8' })
    cachedHwAvailable = res.status === 0 && /h264_videotoolbox/.test(res.stdout ?? '')
  }
  return cachedHwAvailable
}

/** Resolve the encode args for this run: libx264 by default; VideoToolbox only
 * when explicitly opted in via BOSS_PROOF_HW_ENCODE=1 AND available. */
function encoderArgs() {
  const flag = process.env.BOSS_PROOF_HW_ENCODE
  const hwRequested = flag === '1' || flag === 'true'
  return selectEncoderArgs({ hwRequested, hwAvailable: hwRequested && videotoolboxAvailable() })
}

// Hard ceiling for a single ffmpeg encode (BOS-138 P1c). The ffmpeg `-loop 1`
// hang produced an ever-growing file for 30+ minutes before it was noticed;
// a bounded timeout kills the encode so callers see a non-zero result (status
// null + signal SIGKILL) and fail fast instead of hanging the whole run.
const FFMPEG_TIMEOUT_MS = Number(process.env.BOSS_PROOF_FFMPEG_TIMEOUT_MS) || 5 * 60_000

function ffmpeg(args) {
  return spawnSync('ffmpeg', ['-y', '-loglevel', 'error', ...args], {
    stdio: ['ignore', 'inherit', 'inherit'],
    timeout: FFMPEG_TIMEOUT_MS,
    killSignal: 'SIGKILL',
  })
}

function ffprobeDurationMs(videoPath) {
  const res = spawnSync(
    'ffprobe',
    [
      '-v',
      'error',
      '-show_entries',
      'format=duration',
      '-of',
      'default=noprint_wrappers=1:nokey=1',
      videoPath,
    ],
    { encoding: 'utf-8' },
  )
  const seconds = parseFloat(String(res.stdout).trim())
  if (res.status !== 0 || !Number.isFinite(seconds) || seconds <= 0) return null
  return Math.round(seconds * 1000)
}

/** [width, height] of a video via ffprobe, or null. */
export function probeDimensions(videoPath) {
  const res = spawnSync(
    'ffprobe',
    [
      '-v',
      'error',
      '-select_streams',
      'v:0',
      '-show_entries',
      'stream=width,height',
      '-of',
      'csv=p=0:s=x',
      videoPath,
    ],
    { encoding: 'utf-8' },
  )
  const m = String(res.stdout)
    .trim()
    .match(/^(\d+)x(\d+)$/)
  if (res.status !== 0 || !m) return null
  return { width: parseInt(m[1], 10), height: parseInt(m[2], 10) }
}

/**
 * True when `videoPath` holds a decodable video stream of positive duration.
 * A zero-frame mp4 (e.g. a concat that failed on a SAR mismatch) still parses
 * as a valid container but has no dimensions and no duration — this catches it
 * so callers never upload an empty artifact.
 * @param {string} videoPath
 * @returns {boolean}
 */
export function isDecodableVideo(videoPath) {
  return probeDimensions(videoPath) !== null && ffprobeDurationMs(videoPath) !== null
}

/**
 * Extract downscaled grayscale frames once and derive BOTH per-pair diffs (for
 * idle-stretch detection) and per-frame mean luma (for leading white-flash
 * detection) from the same buffer — no second decode. Returns null on failure.
 * @returns {{ diffs: number[], luma: number[] } | null}
 */
function analyzeDiffs(videoPath) {
  const res = spawnSync(
    'ffmpeg',
    [
      '-loglevel',
      'error',
      '-i',
      videoPath,
      '-vf',
      `fps=${ANALYZE_FPS},scale=${ANALYZE_W}:${ANALYZE_H},format=gray`,
      '-f',
      'rawvideo',
      '-',
    ],
    { maxBuffer: 256 * 1024 * 1024 },
  )
  if (res.status !== 0 || !res.stdout || res.stdout.length === 0) return null
  const frameBytes = ANALYZE_W * ANALYZE_H
  return {
    diffs: computeFrameDiffs(res.stdout, frameBytes),
    luma: computeFrameLuma(res.stdout, frameBytes),
  }
}

/**
 * Plan + render the per-step caption overlays for pass 1 (impure: renders strips
 * and writes PNGs). Returns the overlay descriptors for buildTimerOverlayFilter
 * and pushes each rendered strip's path onto `pngPaths` (so the caller can both
 * feed them as single-frame `-i` inputs — NOT `-loop 1`, which is infinite and
 * unbounds the overlay output — and clean them up). Returns [] — degrading to
 * a no-caption video — on any missing prerequisite or per-strip render failure;
 * captions never fail the proof.
 *
 * Window times are planned on the untrimmed cast clock (`fullDurationMs`) then
 * offset by `trimMs` so they line up with the in-filtergraph leading trim that
 * the timer/retime share, and clamped to the trimmed `durationMs`.
 *
 * Exported for unit tests (with a stub `renderCaptionStrip`); the real call
 * happens inside postprocessProofVideo.
 */
export function renderCaptionOverlays({
  captionTimings,
  renderCaptionStrip,
  width,
  fullDurationMs,
  trimMs,
  durationMs,
  inputBase,
  dir,
  pngPaths,
}) {
  if (
    typeof renderCaptionStrip !== 'function' ||
    !Array.isArray(captionTimings) ||
    captionTimings.length === 0 ||
    !Number.isInteger(width) ||
    width <= 0
  ) {
    return []
  }
  try {
    const windows = planCaptionWindows(captionTimings, fullDurationMs)
    const overlays = []
    for (const w of windows) {
      const startSec = Math.max(0, (w.startMs - trimMs) / 1000)
      const endSec = Math.min(durationMs, Math.max(0, w.endMs - trimMs)) / 1000
      if (endSec <= startSec) continue // entirely inside the trimmed-away head
      const output = path.join(dir, `caption-strip-${overlays.length}.png`)
      const ok = renderCaptionStrip({ text: w.text, width, output })
      if (ok === false || !existsSync(output)) continue
      pngPaths.push(output)
      overlays.push({ inputIndex: inputBase + overlays.length, startSec, endSec })
    }
    return overlays
  } catch {
    // Strip render / planning failure → no captions, full video still ships.
    for (const png of pngPaths.splice(0)) rmSync(png, { force: true })
    return []
  }
}

/**
 * Full pipeline: source webm → `timedPath` (original speed, timer burned)
 * → `outPath` (idle stretches compressed). Never throws; on any tool
 * failure returns { ok: false, warning } so callers can fall back to a
 * plain conversion. `scratchPath` holds the rawvideo timer strip and is
 * removed afterwards. When `introPngPath` is provided, a branded intro clip
 * is rendered from it and prepended to the finished video (best-effort: any
 * ffmpeg failure leaves the card-less output intact).
 *
 * Per-step caption strips are additive and best-effort: when `captionTimings`
 * and a `renderCaptionStrip` callback are supplied, each step's narration is
 * burned into the top of the video for its time window (the same clock as the
 * timer, before the idle-retime, so it rides through trim + speed-up). Any
 * failure — no timings, a strip-render error, or an unreadable recording size —
 * silently degrades to the existing no-caption video. Captions never fail the
 * proof.
 *
 * On success (BOS-140/P3b), the result carries `timeline` — the inputs
 * `mapSourceToOutputMs` needs to map a source-clock scene marker to the final
 * mp4's clock: the computed leading trim, the retime plan (identity when
 * idleSpeedup is off or nothing qualified), and the intro card's duration
 * (0 unless the concatenated intro was actually adopted). Absent on failure.
 *
 * @param {{ webmPath: string, timedPath: string, outPath: string, scratchPath: string, introPngPath?: string, cropHeight?: number|null, timer?: boolean, idleSpeedup?: boolean, trimLeadingBlank?: boolean, captionTimings?: Array<{caption:string,startMs:number}>, renderCaptionStrip?: (arg:{text:string,width:number,output:string})=>boolean|void }} io
 * @returns {{ ok: boolean, warning?: string, condensed: boolean, originalMs?: number, outputMs?: number, fastSegments?: number, cropHeight?: number|null, timeline?: {trimMs:number, segments:Array<{startMs:number,endMs:number,speed:number}>, introMs:number} }}
 */
export function postprocessProofVideo({
  webmPath,
  timedPath,
  outPath,
  scratchPath,
  introPngPath,
  cropHeight,
  timer = true,
  idleSpeedup = true,
  trimLeadingBlank = true,
  captionTimings,
  renderCaptionStrip,
  sceneStartsMs,
  endCutMs,
}) {
  const fullDurationMs = ffprobeDurationMs(webmPath)
  if (fullDurationMs === null)
    return {
      ok: false,
      condensed: false,
      warning: 'ffprobe could not read the screencast duration',
    }

  const recorded = probeDimensions(webmPath)
  const effectiveCrop = recorded ? encoderCropHeight(cropHeight, recorded.height) : null

  const analysis = analyzeDiffs(webmPath)
  if (analysis === null)
    return {
      ok: false,
      condensed: false,
      warning: 'frame analysis failed (ffmpeg rawvideo extraction)',
    }

  // Detect + trim the leading pre-roll. Everything downstream (diffs, retime
  // plan, timer clock) runs on the SAME trimmed timeline so 0:00 = the first
  // real app frame. The bright-flash detector fires first, so the dark-app
  // browser path (where it trips) is byte-for-byte unchanged. The static
  // fallback engages only when it returns 0 — covering the TUI dark/blank boot
  // lead-in AND a light/all-bright surface (e.g. marketing) whose static
  // Playwright pre-roll the luma heuristic deliberately declines to trim; the
  // frame-diff detector trims only the leading static run (the page-paint
  // transition ends it), so real content is never gutted. The trim is clamped
  // to LEADING_BLANK_CAP_MS rather than over-trimming, so a boot lead-in longer
  // than the cap keeps a short residual static head (left at 1x, or — once over
  // MIN_STATIC_RUN_MS — folded into the normal idle-speedup pass like any other
  // static stretch).
  const trimMs = trimLeadingBlank
    ? detectLeadingBlankMs(analysis.luma) || detectLeadingStaticMs(analysis.diffs)
    : 0
  const trimSec = trimMs / 1000
  const dropFrames = Math.round((trimMs / 1000) * ANALYZE_FPS)
  let diffs = trimMs > 0 ? analysis.diffs.slice(dropFrames) : analysis.diffs
  let durationMs = Math.max(0, fullDurationMs - trimMs)
  // Trailing dead-air cut (BOS-251): the cast keeps recording after the last
  // settled content screen (bridge quit → blank PTY; a crashed leg → dead
  // terminal), which used to end every video — and its extracted poster — on a
  // caption-only blank frame. `endCutMs` is the source-clock timestamp of the
  // last content the caller wants kept; everything past it (+ a settle pad) is
  // dropped from the output.
  let endSec
  if (Number.isFinite(endCutMs)) {
    const cut = Math.min(durationMs, Math.max(0, endCutMs + TRAILING_CUT_PAD_MS - trimMs))
    if (cut > 0 && cut < durationMs) {
      console.error(
        `[proof-video] trailing cut: dropping ${Math.round(durationMs - cut)}ms of post-content tail ` +
          `(endCutMs=${Math.round(endCutMs)}, trim=${Math.round(trimMs)}, duration=${Math.round(durationMs)})`,
      )
      durationMs = cut
      endSec = (trimMs + cut) / 1000
      const keepFrames = Math.ceil((durationMs / 1000) * ANALYZE_FPS)
      if (diffs.length > keepFrames) diffs = diffs.slice(0, keepFrames)
    } else {
      console.error(
        `[proof-video] trailing cut skipped (endCutMs=${Math.round(endCutMs)} + pad lands at ` +
          `${Math.round(cut)}ms of ${Math.round(durationMs)}ms) — the cast and video clocks should ` +
          'match; a large gap suggests idle-time compression upstream',
      )
    }
  }

  const staticRuns = findStaticRuns(diffs)
  // With idleSpeedup off the speedup plan must not leak into pass 2 (floors may
  // still force a retime pass below), so start from an identity plan instead.
  let segments = idleSpeedup
    ? planRetime(staticRuns, durationMs)
    : durationMs > 0
      ? [{ startMs: 0, endMs: durationMs, speed: 1 }]
      : []
  // Caption/screen-readability floor (BOS-251): every settled screen (each
  // captionTimings entry, narrated or not, is one settled screen) must hold
  // long enough to watch, and every burned caption long enough to read.
  // Boundaries arrive on the source clock; shift onto the trimmed clock.
  const captionStarts = (captionTimings ?? [])
    .filter((c) => Number.isFinite(c.startMs))
    .map((c) => c.startMs - trimMs)
  if (captionStarts.length > 0) {
    segments = applySceneFloors(segments, captionStarts, durationMs, CAPTION_MIN_OUTPUT_MS)
  }
  // Per-scene watchability floor (BOS-251): scene boundaries arrive on the
  // source clock; shift them onto the trimmed clock the plan uses.
  if (Array.isArray(sceneStartsMs) && sceneStartsMs.length > 0) {
    segments = applySceneFloors(
      segments,
      sceneStartsMs.filter((b) => Number.isFinite(b)).map((b) => b - trimMs),
      durationMs,
    )
  }
  const fastSegments = segments.filter((s) => s.speed > 1)
  const slowSegments = segments.filter((s) => s.speed < 1)

  // Pass 1: optionally burn the elapsed timer on the (trimmed) original timeline
  // (so it shows real elapsed time and visibly fast-forwards once stretches are
  // retimed). The leading trim is applied in-filtergraph — NOT via -ss — so the
  // diff analysis, overlay timer, and retime plan all share one clock.
  const baseChain = buildBaseChain({ trimSec, endSec, cropHeight: effectiveCrop, fps: OUTPUT_FPS })
  const pass1Inputs = ['-i', webmPath]
  if (timer) {
    const strip = renderTimerStrip(Math.ceil(durationMs / 1000), fastForwardSeconds(segments))
    writeFileSync(scratchPath, strip)
    pass1Inputs.push(
      '-f',
      'rawvideo',
      '-pix_fmt',
      'rgba',
      '-video_size',
      `${TIMER_W}x${TIMER_H}`,
      '-framerate',
      '1',
      '-i',
      scratchPath,
    )
  }

  // Per-step captions (best-effort, additive). Plan the display windows on the
  // cast clock, render one strip per window, then offset each by the SAME
  // in-filtergraph leading trim the timer/retime use so the burned caption rides
  // the trimmed timeline. The strips overlay BEFORE the idle-retime (pass 2), so
  // a caption spanning a compressed idle stretch simply plays faster. Caption
  // inputs follow the timer input, so the timer keeps input index 1.
  const captionPngPaths = []
  const captionOverlays = renderCaptionOverlays({
    captionTimings,
    renderCaptionStrip,
    width: recorded?.width,
    fullDurationMs,
    trimMs,
    durationMs,
    inputBase: timer ? 2 : 1,
    dir: path.dirname(scratchPath),
    pngPaths: captionPngPaths,
  })
  pass1Inputs.push(...captionInputArgs(captionPngPaths))

  const pass1Filter = buildTimerOverlayFilter(baseChain, { timer, captions: captionOverlays })
  // When timer is off and there are no captions the base chain ends in [base],
  // not [v]; map whichever the filtergraph produced.
  const mapLabel = timer || captionOverlays.length > 0 ? '[v]' : '[base]'
  const pass1 = ffmpeg([
    ...pass1Inputs,
    '-filter_complex',
    pass1Filter,
    '-map',
    mapLabel,
    ...encoderArgs(),
    timedPath,
  ])
  if (timer) rmSync(scratchPath, { force: true })
  for (const png of captionPngPaths) rmSync(png, { force: true })
  if (pass1.status !== 0)
    return { ok: false, condensed: false, warning: 'timer overlay pass failed' }

  // Pass 2: compress the static stretches. Nothing to compress (or idleSpeedup
  // disabled) → the timed video IS the final video.
  let result
  if (fastSegments.length === 0 && slowSegments.length === 0) {
    copyFileSync(timedPath, outPath)
    result = {
      ok: true,
      condensed: false,
      originalMs: durationMs,
      outputMs: durationMs,
      fastSegments: 0,
      leadingBlankMs: trimMs,
      cropHeight: effectiveCrop,
      // Chapter-timestamp mapping input (BOS-140/P3b) — introMs starts 0 and is
      // set below only if the intro card is actually adopted.
      timeline: { trimMs, segments, introMs: 0 },
    }
  } else {
    const pass2 = ffmpeg([
      '-i',
      timedPath,
      '-filter_complex',
      buildRetimeFilter(segments),
      '-map',
      '[v]',
      ...encoderArgs(),
      outPath,
    ])
    if (pass2.status !== 0)
      return { ok: false, condensed: false, warning: 'idle-speedup pass failed' }

    result = {
      ok: true,
      condensed: true,
      originalMs: durationMs,
      outputMs: retimedDurationMs(segments),
      fastSegments: fastSegments.length,
      leadingBlankMs: trimMs,
      cropHeight: effectiveCrop,
      timeline: { trimMs, segments, introMs: 0 },
    }
  }

  // Optional branded intro card prepended to the finished video.
  if (introPngPath) {
    const dims = probeDimensions(outPath)
    if (dims) {
      const introClip = path.join(path.dirname(scratchPath), 'intro-clip.mp4')
      const withIntro = path.join(path.dirname(outPath), 'with-intro.mp4')
      const clip = ffmpeg(
        buildIntroClipArgs({
          pngPath: introPngPath,
          width: dims.width,
          height: dims.height,
          fps: OUTPUT_FPS,
          outPath: introClip,
        }),
      )
      if (clip.status === 0) {
        const cat = ffmpeg(
          buildIntroConcatArgs({ introPath: introClip, mainPath: outPath, outPath: withIntro }),
        )
        // Only adopt the concatenated file if it is actually decodable. The
        // concat filter can exit 0 yet write a zero-frame container when its
        // inputs disagree (e.g. SAR mismatch); clobbering outPath with that
        // would ship an empty video.
        if (cat.status === 0 && isDecodableVideo(withIntro)) {
          copyFileSync(withIntro, outPath)
          result.timeline.introMs = INTRO_SEC * 1000
        }
        rmSync(withIntro, { force: true })
      }
      rmSync(introClip, { force: true })
      // Intro is additive: any ffmpeg failure leaves the card-less outPath intact.
    }
  }

  // Final guard: never report success for an empty/undecodable artifact. A
  // truthy result here means the caller skips its plain-conversion fallback, so
  // an invalid outPath must flip ok=false instead.
  if (!isDecodableVideo(outPath)) {
    return {
      ok: false,
      condensed: false,
      warning: 'post-processed video has no decodable video stream',
    }
  }

  return result
}
