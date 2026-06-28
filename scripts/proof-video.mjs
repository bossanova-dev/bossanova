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

import { spawnSync } from 'node:child_process';
import { copyFileSync, rmSync, writeFileSync } from 'node:fs';
import path from 'node:path';

// Intro-card ffmpeg arg builders live in proof-video-intro.mjs.
import { buildIntroClipArgs, buildIntroConcatArgs } from './proof-video-intro.mjs';

// ── Analysis constants (tuned on a real 11-min gen-AI proof recording) ──────
export const ANALYZE_FPS = 4; // diff sampling rate — 250ms resolution
export const ANALYZE_W = 96;
export const ANALYZE_H = 54;
/** Mean-abs-diff (0-255 scale) below which two sampled frames count as "the
 * same". Loose enough to absorb spinners/cursor blinks, far below the diff
 * any real action (typing, spawn, pan) produces. */
export const STATIC_DIFF_THRESHOLD = 1.5;
/** Only stretches at least this long are compressed. */
export const MIN_STATIC_RUN_MS = 8_000;
/** Real-time breathing room kept at each end of a compressed stretch. */
export const RETIME_PAD_MS = 1_000;
/** Playback duration each compressed stretch is squeezed into. */
export const RETIME_TARGET_MS = 4_000;
export const OUTPUT_FPS = 30;

// ── Leading white-flash trim ────────────────────────────────────────────────
// Chrome/Playwright paint a blank white page for a few hundred ms before the
// app mounts, leaving a white flash at the START of the recording. The app is
// dark (canvas YAVG ~25-33) and the flash is near-white (YAVG ~234) — a clean
// ~200-point gap — so a leading run of bright frames is unambiguous for THIS
// app and cannot false-trigger on dark gen-AI content.
/** Mean-luma (0-255) above which a sampled frame counts as the browser's white pre-roll. */
export const LEADING_BLANK_LUMA = 200;
/** Safety buffer trimmed past the last detected white frame. */
export const LEADING_BLANK_BUFFER_MS = 150;
/** Hard cap so a pathological all-bright start can never gut real content. */
export const LEADING_BLANK_CAP_MS = 2_000;

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
export const LEADING_STATIC_DIFF = 1.5;

// ── Pure: frame differencing ─────────────────────────────────────────────────

/**
 * Mean absolute difference between each consecutive pair of same-size
 * grayscale frames packed in `raw`.
 * @param {Buffer|Uint8Array} raw @param {number} frameBytes
 * @returns {number[]} length = frameCount - 1
 */
export function computeFrameDiffs(raw, frameBytes) {
  const frames = Math.floor(raw.length / frameBytes);
  const diffs = [];
  for (let f = 1; f < frames; f++) {
    const a = (f - 1) * frameBytes;
    const b = f * frameBytes;
    let sum = 0;
    for (let i = 0; i < frameBytes; i++) sum += Math.abs(raw[b + i] - raw[a + i]);
    diffs.push(sum / frameBytes);
  }
  return diffs;
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
  const fps = opts.fps ?? ANALYZE_FPS;
  const threshold = opts.threshold ?? STATIC_DIFF_THRESHOLD;
  const minRunMs = opts.minRunMs ?? MIN_STATIC_RUN_MS;
  const runs = [];
  let runStart = -1;
  const flush = (endIdx) => {
    if (runStart < 0) return;
    const startMs = (runStart / fps) * 1000;
    const endMs = ((endIdx + 1) / fps) * 1000;
    if (endMs - startMs >= minRunMs)
      runs.push({ startMs: Math.round(startMs), endMs: Math.round(endMs) });
    runStart = -1;
  };
  for (let k = 0; k < diffs.length; k++) {
    if (diffs[k] < threshold) {
      if (runStart < 0) runStart = k;
    } else {
      flush(k - 1);
    }
  }
  flush(diffs.length - 1);
  return runs;
}

/**
 * Mean byte value of each same-size frame packed in `raw` (grayscale → mean
 * luminance). Pairs with computeFrameDiffs over the same extracted buffer.
 * @param {Buffer|Uint8Array} raw @param {number} frameBytes
 * @returns {number[]} length = frameCount
 */
export function computeFrameLuma(raw, frameBytes) {
  const frames = Math.floor(raw.length / frameBytes);
  const luma = [];
  for (let f = 0; f < frames; f++) {
    const start = f * frameBytes;
    let sum = 0;
    for (let i = 0; i < frameBytes; i++) sum += raw[start + i];
    luma.push(sum / frameBytes);
  }
  return luma;
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
  const fps = opts.fps ?? ANALYZE_FPS;
  const threshold = opts.threshold ?? LEADING_BLANK_LUMA;
  const bufferMs = opts.bufferMs ?? LEADING_BLANK_BUFFER_MS;
  const capMs = opts.capMs ?? LEADING_BLANK_CAP_MS;
  let n = 0;
  while (n < luma.length && luma[n] > threshold) n++;
  if (n === 0) return 0;
  if (n === luma.length) return 0; // all-bright → light app, not a flash
  const trimMs = Math.round((n / fps) * 1000 + bufferMs);
  return Math.min(trimMs, capMs);
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
  const fps = opts.fps ?? ANALYZE_FPS;
  const eps = opts.eps ?? LEADING_STATIC_DIFF;
  const bufferMs = opts.bufferMs ?? LEADING_BLANK_BUFFER_MS;
  const capMs = opts.capMs ?? LEADING_BLANK_CAP_MS;
  let n = 0;
  while (n < diffs.length && diffs[n] < eps) n++;
  if (n === 0) return 0;
  if (n === diffs.length) return 0; // all-static → not a lead-in
  const trimMs = Math.round((n / fps) * 1000 + bufferMs);
  return Math.min(trimMs, capMs);
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
  const padMs = opts.padMs ?? RETIME_PAD_MS;
  const targetMs = opts.targetMs ?? RETIME_TARGET_MS;
  const segments = [];
  let cursor = 0;
  for (const run of staticRuns) {
    const fastStart = Math.max(run.startMs + padMs, cursor);
    const fastEnd = Math.min(run.endMs - padMs, durationMs);
    const fastLen = fastEnd - fastStart;
    if (fastLen <= targetMs) continue;
    if (fastStart > cursor) segments.push({ startMs: cursor, endMs: fastStart, speed: 1 });
    segments.push({
      startMs: fastStart,
      endMs: fastEnd,
      speed: Math.round((fastLen / targetMs) * 100) / 100,
    });
    cursor = fastEnd;
  }
  if (cursor < durationMs) segments.push({ startMs: cursor, endMs: durationMs, speed: 1 });
  return segments;
}

/** Output duration (ms) of a retime plan. */
export function retimedDurationMs(segments) {
  return Math.round(segments.reduce((t, s) => t + (s.endMs - s.startMs) / s.speed, 0));
}

/**
 * Seconds (original timeline) that fall inside a sped-up segment — these
 * timer frames get the ">>" fast-forward marker.
 * @param {Array<{startMs: number, endMs: number, speed: number}>} segments
 * @returns {Set<number>}
 */
export function fastForwardSeconds(segments) {
  const out = new Set();
  for (const s of segments) {
    if (s.speed <= 1) continue;
    for (let sec = Math.floor(s.startMs / 1000); sec * 1000 < s.endMs; sec++) out.add(sec);
  }
  return out;
}

/**
 * filter_complex that plays each segment at its speed and concatenates.
 * Input timestamps are seconds on the source (timed) video.
 * @param {Array<{startMs: number, endMs: number, speed: number}>} segments
 * @returns {string}
 */
export function buildRetimeFilter(segments) {
  const parts = [];
  const labels = [];
  segments.forEach((s, i) => {
    const a = (s.startMs / 1000).toFixed(3);
    const b = (s.endMs / 1000).toFixed(3);
    parts.push(`[0:v]trim=start=${a}:end=${b},setpts=(PTS-STARTPTS)/${s.speed}[s${i}]`);
    labels.push(`[s${i}]`);
  });
  parts.push(`${labels.join('')}concat=n=${segments.length}:v=1:a=0,fps=${OUTPUT_FPS}[v]`);
  return parts.join(';');
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
};
const GLYPH_W = 8;
const GLYPH_H = 12;
const GLYPH_GAP = 1;
const SCALE = 2;
const PAD = 8;
// Canvas must be constant across frames; size it for the widest text.
const MAX_TEXT = '>> 88:88';
export const TIMER_W =
  (MAX_TEXT.length * GLYPH_W + (MAX_TEXT.length - 1) * GLYPH_GAP) * SCALE + PAD * 2; // 158
export const TIMER_H = GLYPH_H * SCALE + PAD * 2; // 40

/** mm:ss for a non-negative second count (hours wrap into minutes). */
export function formatElapsed(totalSeconds) {
  const m = Math.floor(totalSeconds / 60);
  const s = totalSeconds % 60;
  return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
}

function textPixelWidth(text) {
  return (text.length * GLYPH_W + (text.length - 1) * GLYPH_GAP) * SCALE;
}

/** Render one RGBA timer frame: right-aligned text on a translucent pill. */
export function renderTimerFrame(text) {
  const buf = Buffer.alloc(TIMER_W * TIMER_H * 4); // transparent
  const textW = textPixelWidth(text);
  const textX = TIMER_W - PAD - textW;
  // Pill hugs the text, not the canvas, so the box doesn't show dead space.
  for (let y = 0; y < TIMER_H; y++) {
    for (let x = textX - PAD; x < TIMER_W; x++) {
      const o = (y * TIMER_W + x) * 4;
      buf[o] = 0;
      buf[o + 1] = 0;
      buf[o + 2] = 0;
      buf[o + 3] = 150;
    }
  }
  for (let c = 0; c < text.length; c++) {
    const glyph = GLYPHS[text[c]] ?? GLYPHS[' '];
    const gx = textX + c * (GLYPH_W + GLYPH_GAP) * SCALE;
    const gy = PAD;
    for (let row = 0; row < GLYPH_H; row++) {
      for (let col = 0; col < GLYPH_W; col++) {
        if (glyph[row][col] !== '#') continue;
        for (let sy = 0; sy < SCALE; sy++) {
          for (let sx = 0; sx < SCALE; sx++) {
            const o = ((gy + row * SCALE + sy) * TIMER_W + gx + col * SCALE + sx) * 4;
            buf[o] = 255;
            buf[o + 1] = 255;
            buf[o + 2] = 255;
            buf[o + 3] = 255;
          }
        }
      }
    }
  }
  return buf;
}

/**
 * One RGBA frame per second of original video, ">>" marker during sped-up
 * stretches. Fed to ffmpeg as a 1fps rawvideo input.
 * @param {number} totalSeconds @param {Set<number>} fastSecs
 * @returns {Buffer}
 */
export function renderTimerStrip(totalSeconds, fastSecs) {
  const frames = [];
  for (let sec = 0; sec <= totalSeconds; sec++) {
    const prefix = fastSecs.has(sec) ? '>> ' : '';
    frames.push(renderTimerFrame(`${prefix}${formatElapsed(sec)}`));
  }
  return Buffer.concat(frames);
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
  if (cropHeight == null || !Number.isFinite(cropHeight) || cropHeight <= 0) return null;
  const even = cropHeight - (cropHeight % 2);
  if (even <= 0) return null;
  if (even >= recordedHeight) return null;
  return even;
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
  if (cropHeight == null || !Number.isFinite(cropHeight) || cropHeight <= 0) return null;
  const floor = Math.ceil(0.5 * recordedWidth);
  const raised = Math.max(cropHeight, floor);
  // even-round (H264) and reuse evenCropHeight's >= recordedHeight → null guard
  return evenCropHeight(raised, recordedHeight);
}

/**
 * Crop height to feed the encoder. Explicit measured crops keep the existing
 * behavior; recordings with no crop still need an even height for H264.
 * @param {number|null|undefined} cropHeight measured content height (px)
 * @param {number} recordedHeight viewport height the video was recorded at (px)
 * @returns {number|null}
 */
export function encoderCropHeight(cropHeight, recordedHeight) {
  const explicit = evenCropHeight(cropHeight, recordedHeight);
  if (explicit !== null) return explicit;
  if (!Number.isFinite(recordedHeight) || recordedHeight <= 0) return null;
  if (recordedHeight % 2 === 0) return null;
  return recordedHeight - 1;
}

/**
 * Build the pass-1 base-chain string `[0:v]...[base]` for the filtergraph.
 * Applies, in order: optional leading trim (when trimSec > 0), optional crop
 * (when cropHeight is a positive integer), then fps. Crop comes before fps so
 * the frame-rate conversion works on the already-cropped geometry.
 * @param {{ trimSec: number, cropHeight: number|null|undefined, fps: number }} opts
 * @returns {string}
 */
export function buildBaseChain({ trimSec, cropHeight, fps }) {
  const parts = [];
  if (trimSec > 0) parts.push(`trim=start=${trimSec.toFixed(3)},setpts=PTS-STARTPTS`);
  if (cropHeight != null && Number.isInteger(cropHeight) && cropHeight > 0)
    parts.push(`crop=in_w:${cropHeight}:0:0`);
  parts.push(`fps=${fps}`);
  return `[0:v]${parts.join(',')}[base]`;
}

/**
 * Pass-1 filtergraph: with a timer, overlay the strip on the top-right; without
 * one, the base chain (trim/crop/fps) is the whole graph. Pure string builder.
 * @param {string} baseChain e.g. '[0:v]crop=…,fps=30[base]'
 * @param {{timer:boolean}} opts
 * @returns {string}
 */
export function buildTimerOverlayFilter(baseChain, { timer }) {
  if (!timer) return `${baseChain}`;
  return `${baseChain};[base][1:v]overlay=x=main_w-overlay_w-14:y=44:eof_action=repeat[v]`;
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
];

function ffmpeg(args) {
  return spawnSync('ffmpeg', ['-y', '-loglevel', 'error', ...args], {
    stdio: ['ignore', 'inherit', 'inherit'],
  });
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
  );
  const seconds = parseFloat(String(res.stdout).trim());
  if (res.status !== 0 || !Number.isFinite(seconds) || seconds <= 0) return null;
  return Math.round(seconds * 1000);
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
  );
  const m = String(res.stdout)
    .trim()
    .match(/^(\d+)x(\d+)$/);
  if (res.status !== 0 || !m) return null;
  return { width: parseInt(m[1], 10), height: parseInt(m[2], 10) };
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
  return probeDimensions(videoPath) !== null && ffprobeDurationMs(videoPath) !== null;
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
  );
  if (res.status !== 0 || !res.stdout || res.stdout.length === 0) return null;
  const frameBytes = ANALYZE_W * ANALYZE_H;
  return {
    diffs: computeFrameDiffs(res.stdout, frameBytes),
    luma: computeFrameLuma(res.stdout, frameBytes),
  };
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
 * @param {{ webmPath: string, timedPath: string, outPath: string, scratchPath: string, introPngPath?: string, cropHeight?: number|null, timer?: boolean, idleSpeedup?: boolean, trimLeadingBlank?: boolean }} io
 * @returns {{ ok: boolean, warning?: string, condensed: boolean, originalMs?: number, outputMs?: number, fastSegments?: number, cropHeight?: number|null }}
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
}) {
  const fullDurationMs = ffprobeDurationMs(webmPath);
  if (fullDurationMs === null)
    return {
      ok: false,
      condensed: false,
      warning: 'ffprobe could not read the screencast duration',
    };

  const recorded = probeDimensions(webmPath);
  const effectiveCrop = recorded ? encoderCropHeight(cropHeight, recorded.height) : null;

  const analysis = analyzeDiffs(webmPath);
  if (analysis === null)
    return {
      ok: false,
      condensed: false,
      warning: 'frame analysis failed (ffmpeg rawvideo extraction)',
    };

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
    : 0;
  const trimSec = trimMs / 1000;
  const dropFrames = Math.round((trimMs / 1000) * ANALYZE_FPS);
  const diffs = trimMs > 0 ? analysis.diffs.slice(dropFrames) : analysis.diffs;
  const durationMs = Math.max(0, fullDurationMs - trimMs);

  const staticRuns = findStaticRuns(diffs);
  const segments = planRetime(staticRuns, durationMs);
  const fastSegments = segments.filter((s) => s.speed > 1);

  // Pass 1: optionally burn the elapsed timer on the (trimmed) original timeline
  // (so it shows real elapsed time and visibly fast-forwards once stretches are
  // retimed). The leading trim is applied in-filtergraph — NOT via -ss — so the
  // diff analysis, overlay timer, and retime plan all share one clock.
  const baseChain = buildBaseChain({ trimSec, cropHeight: effectiveCrop, fps: OUTPUT_FPS });
  const pass1Filter = buildTimerOverlayFilter(baseChain, { timer });
  const pass1Inputs = ['-i', webmPath];
  if (timer) {
    const strip = renderTimerStrip(Math.ceil(durationMs / 1000), fastForwardSeconds(segments));
    writeFileSync(scratchPath, strip);
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
    );
  }
  // When timer is off the base chain ends in [base], not [v]; map whichever exists.
  const mapLabel = timer ? '[v]' : '[base]';
  const pass1 = ffmpeg([
    ...pass1Inputs,
    '-filter_complex',
    pass1Filter,
    '-map',
    mapLabel,
    ...ENCODE_ARGS,
    timedPath,
  ]);
  if (timer) rmSync(scratchPath, { force: true });
  if (pass1.status !== 0)
    return { ok: false, condensed: false, warning: 'timer overlay pass failed' };

  // Pass 2: compress the static stretches. Nothing to compress (or idleSpeedup
  // disabled) → the timed video IS the final video.
  let result;
  if (!idleSpeedup || fastSegments.length === 0) {
    copyFileSync(timedPath, outPath);
    result = {
      ok: true,
      condensed: false,
      originalMs: durationMs,
      outputMs: durationMs,
      fastSegments: 0,
      leadingBlankMs: trimMs,
      cropHeight: effectiveCrop,
    };
  } else {
    const pass2 = ffmpeg([
      '-i',
      timedPath,
      '-filter_complex',
      buildRetimeFilter(segments),
      '-map',
      '[v]',
      ...ENCODE_ARGS,
      outPath,
    ]);
    if (pass2.status !== 0)
      return { ok: false, condensed: false, warning: 'idle-speedup pass failed' };

    result = {
      ok: true,
      condensed: true,
      originalMs: durationMs,
      outputMs: retimedDurationMs(segments),
      fastSegments: fastSegments.length,
      leadingBlankMs: trimMs,
      cropHeight: effectiveCrop,
    };
  }

  // Optional branded intro card prepended to the finished video.
  if (introPngPath) {
    const dims = probeDimensions(outPath);
    if (dims) {
      const introClip = path.join(path.dirname(scratchPath), 'intro-clip.mp4');
      const withIntro = path.join(path.dirname(outPath), 'with-intro.mp4');
      const clip = ffmpeg(
        buildIntroClipArgs({
          pngPath: introPngPath,
          width: dims.width,
          height: dims.height,
          fps: OUTPUT_FPS,
          outPath: introClip,
        }),
      );
      if (clip.status === 0) {
        const cat = ffmpeg(
          buildIntroConcatArgs({ introPath: introClip, mainPath: outPath, outPath: withIntro }),
        );
        // Only adopt the concatenated file if it is actually decodable. The
        // concat filter can exit 0 yet write a zero-frame container when its
        // inputs disagree (e.g. SAR mismatch); clobbering outPath with that
        // would ship an empty video.
        if (cat.status === 0 && isDecodableVideo(withIntro)) copyFileSync(withIntro, outPath);
        rmSync(withIntro, { force: true });
      }
      rmSync(introClip, { force: true });
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
    };
  }

  return result;
}
