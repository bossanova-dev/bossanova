// Unit tests for the pure planning/rendering half of proof-video.mjs.
// The ffmpeg orchestration (postprocessProofVideo) is exercised against a
// real recording manually — these tests pin down the analysis math.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  buildTimerOverlayFilter,
  computeFrameDiffs,
  computeFrameLuma,
  detectLeadingBlankMs,
  detectLeadingStaticMs,
  findStaticRuns,
  planRetime,
  retimedDurationMs,
  fastForwardSeconds,
  buildRetimeFilter,
  formatElapsed,
  renderTimerFrame,
  renderTimerStrip,
  evenCropHeight,
  encoderCropHeight,
  buildBaseChain,
  applyMinHeightRatio,
  LEADING_STATIC_DIFF,
  TIMER_W,
  TIMER_H,
} from './proof-video.mjs'

// ── computeFrameDiffs ────────────────────────────────────────────────────────

test('computeFrameDiffs: identical frames diff to 0, changed frames to the mean delta', () => {
  const frameBytes = 4
  const raw = Buffer.from([
    10,
    10,
    10,
    10, // frame 0
    10,
    10,
    10,
    10, // frame 1 (identical)
    10,
    10,
    10,
    50, // frame 2 (one pixel +40 → MAD 10)
  ])
  assert.deepEqual(computeFrameDiffs(raw, frameBytes), [0, 10])
})

test('computeFrameDiffs: ignores a trailing partial frame', () => {
  const raw = Buffer.from([1, 1, 2, 2, 9]) // 2 full frames of 2 bytes + 1 stray byte
  assert.deepEqual(computeFrameDiffs(raw, 2), [1])
})

// ── computeFrameLuma / detectLeadingBlankMs ──────────────────────────────────

test('computeFrameLuma: mean byte value per frame', () => {
  const raw = Buffer.from([10, 10, 10, 10, 200, 200, 200, 200])
  assert.deepEqual(computeFrameLuma(raw, 4), [10, 200])
})

test('detectLeadingBlankMs: leading near-white run → runLen + buffer', () => {
  // fps=4 → 250ms/frame. 2 white frames + dark → 2*250 + 150 buffer = 650.
  const luma = [234, 234, 25, 25, 33, 30]
  assert.equal(detectLeadingBlankMs(luma, { fps: 4 }), 650)
})

test('detectLeadingBlankMs: dark start (gen-AI) → 0, NOT trimmed (regression)', () => {
  const luma = [25, 30, 234, 28, 31] // a bright frame mid-stream must not trigger
  assert.equal(detectLeadingBlankMs(luma, { fps: 4 }), 0)
})

test('detectLeadingBlankMs: whole-video-bright → 0 (light app, not a flash)', () => {
  // Every frame bright → a light-themed surface, not a white pre-roll. Trimming
  // would gut real content (and empty a short clip), so trim nothing.
  const luma = Array(40).fill(234)
  assert.equal(detectLeadingBlankMs(luma, { fps: 4, capMs: 2000 }), 0)
})

test('detectLeadingBlankMs: bright run covering all-but-one frame still trims+caps', () => {
  // A genuine flash that gives way to one dark content frame is still trimmed
  // (and capped) — the all-bright guard must only fire when EVERY frame is bright.
  const luma = [...Array(40).fill(234), 25]
  assert.equal(detectLeadingBlankMs(luma, { fps: 4, capMs: 2000 }), 2000)
})

test('detectLeadingBlankMs: trimMs clamped to capMs', () => {
  const luma = [...Array(10).fill(234), 25] // 10*250+150 = 2650 → clamped 2000
  assert.equal(detectLeadingBlankMs(luma, { fps: 4, capMs: 2000 }), 2000)
})

test('detectLeadingBlankMs: empty luma → 0', () => {
  assert.equal(detectLeadingBlankMs([], { fps: 4 }), 0)
})

// ── detectLeadingStaticMs ────────────────────────────────────────────────────

test('detectLeadingStaticMs: VHS dark-boot → trims static boot run + buffer', () => {
  // 6 near-zero diffs (static blank terminal at boot) then active diffs.
  // fps=4 → 250ms/frame. 6*250 + 150 buffer = 1650ms.
  const diffs = [0.0, 0.01, 0.0, 0.01, 0.0, 0.01, 5.2, 8.3, 4.1]
  assert.equal(detectLeadingStaticMs(diffs, { fps: 4 }), 1650)
})

test('detectLeadingStaticMs: browser white-flash (also static) → trims it too', () => {
  // Browser pre-roll is also static (identical near-white frames). 3 static + active.
  // This proves one detector covers both surfaces — plan acceptance criterion.
  // fps=4 → 3*250 + 150 buffer = 900ms.
  const diffs = [0.02, 0.01, 0.03, 6.0, 5.5, 4.2]
  assert.equal(detectLeadingStaticMs(diffs, { fps: 4 }), 900)
})

test('detectLeadingStaticMs: no lead-in (active from frame 0) → 0', () => {
  const diffs = [5.2, 0.01, 0.01, 3.4]
  assert.equal(detectLeadingStaticMs(diffs, { fps: 4 }), 0)
})

test('detectLeadingStaticMs: all-static → 0 (guard: not a lead-in)', () => {
  // An all-static clip is not a lead-in — trimming it would gut real content.
  const diffs = Array(20).fill(0.01)
  assert.equal(detectLeadingStaticMs(diffs, { fps: 4 }), 0)
})

test('detectLeadingStaticMs: clamp to capMs', () => {
  // 10 static diffs at fps=4 → 10*250+150=2650ms → clamped to 2000ms.
  const diffs = [...Array(10).fill(0.01), 5.5]
  assert.equal(detectLeadingStaticMs(diffs, { fps: 4, capMs: 2000 }), 2000)
})

test('detectLeadingStaticMs: eps regression pin — terminal-static frames are below LEADING_STATIC_DIFF', () => {
  // Pins the threshold so a future drift cannot silently skip TUI boot detection.
  // VHS terminal boot: diff ≈ 0 (no change at all). VHS cursor blink: ~0.05 on
  // 96×54 grayscale (1 blinking pixel / 5184 total ≈ 0.05). Any real action
  // (typing, spawning a process) produces diff ≥ 5. LEADING_STATIC_DIFF=1.5
  // absorbs the blink while staying far below real-action diffs.
  const terminalBootDiffs = [0.0, 0.05, 0.0, 0.03, 0.0, 0.05] // static boot
  const withActiveStart = [...terminalBootDiffs, 5.5] // then app paints
  assert.ok(
    terminalBootDiffs.every((d) => d < LEADING_STATIC_DIFF),
    `LEADING_STATIC_DIFF=${LEADING_STATIC_DIFF} must exceed all terminal-static diffs`,
  )
  assert.ok(
    detectLeadingStaticMs(withActiveStart, { fps: 4 }) > 0,
    'static terminal boot sequence must be detected and trimmed',
  )
})

test('leading-trim composition: bright detector wins on a dark app (browser unchanged)', () => {
  // postprocessProofVideo trims with `detectLeadingBlankMs(luma) || detectLeadingStaticMs(diffs)`.
  // A dark web app has a near-white flash giving way to dark content: the bright
  // detector fires, so its value is used verbatim and the static fallback never
  // runs — the established browser path is byte-for-byte unchanged.
  const luma = [234, 234, 25, 25, 33, 30] // white flash → dark app
  const diffs = [0.1, 9, 0.2, 0.3, 0.2] // flash static, then paint, then app
  const bright = detectLeadingBlankMs(luma, { fps: 4 })
  assert.equal(bright, 650)
  assert.equal(bright || detectLeadingStaticMs(diffs, { fps: 4 }), 650)
})

test('leading-trim composition: light surface falls back to the static detector', () => {
  // A light/all-bright surface (e.g. marketing) stays bright throughout, so the
  // brightness heuristic deliberately returns 0 (it cannot tell a bright pre-roll
  // from a bright app). The static fallback then trims ONLY the leading static
  // pre-roll — the page-paint diff spike ends the run, so real content survives.
  const luma = Array(8).fill(234) // bright pre-roll AND bright app
  const bright = detectLeadingBlankMs(luma, { fps: 4, capMs: 2000 })
  assert.equal(bright, 0, 'all-bright → brightness heuristic declines to trim')
  const diffs = [0.1, 0.2, 0.1, 8, 6, 5, 7] // 3 static pre-roll frames, then paint+scroll
  const fallback = detectLeadingStaticMs(diffs, { fps: 4 })
  assert.ok(fallback > 0, 'static pre-roll on a light surface must be trimmed')
  assert.equal(bright || fallback, fallback, 'composition uses the static fallback here')
})

// ── findStaticRuns ───────────────────────────────────────────────────────────

test('findStaticRuns: reports runs meeting minRunMs and drops short ones', () => {
  // 4 fps → each diff covers 250ms. 40 static diffs ≈ 10.25s span.
  const diffs = [9, ...Array(40).fill(0.1), 9, 9, ...Array(4).fill(0.1), 9]
  const runs = findStaticRuns(diffs, { fps: 4, threshold: 1.5, minRunMs: 8000 })
  assert.equal(runs.length, 1)
  // run over diffs[1..40] → frames 1..41 → 250ms..10250ms
  assert.deepEqual(runs[0], { startMs: 250, endMs: 10250 })
})

test('findStaticRuns: a run reaching the end of the video is flushed', () => {
  const diffs = [9, ...Array(40).fill(0.1)]
  const runs = findStaticRuns(diffs, { fps: 4, threshold: 1.5, minRunMs: 8000 })
  assert.equal(runs.length, 1)
  assert.equal(runs[0].endMs, 10250)
})

test('findStaticRuns: empty and all-active inputs produce no runs', () => {
  assert.deepEqual(findStaticRuns([], {}), [])
  assert.deepEqual(findStaticRuns(Array(100).fill(9), { fps: 4 }), [])
})

// ── planRetime ───────────────────────────────────────────────────────────────

test('planRetime: pads the run, compresses the middle, covers the full duration', () => {
  const segments = planRetime([{ startMs: 10_000, endMs: 30_000 }], 60_000, {
    padMs: 1000,
    targetMs: 4000,
  })
  assert.deepEqual(segments, [
    { startMs: 0, endMs: 11_000, speed: 1 },
    { startMs: 11_000, endMs: 29_000, speed: 4.5 }, // 18s → 4s
    { startMs: 29_000, endMs: 60_000, speed: 1 },
  ])
  assert.equal(retimedDurationMs(segments), 11_000 + 4_000 + 31_000)
})

test('planRetime: skips runs whose padded middle would not shrink', () => {
  // 6s run − 2s pad = 4s middle = target → nothing to compress.
  const segments = planRetime([{ startMs: 0, endMs: 6000 }], 20_000, {
    padMs: 1000,
    targetMs: 4000,
  })
  assert.deepEqual(segments, [{ startMs: 0, endMs: 20_000, speed: 1 }])
})

test('planRetime: run touching the start emits no empty leading segment', () => {
  const segments = planRetime([{ startMs: 0, endMs: 20_000 }], 30_000, {
    padMs: 0,
    targetMs: 4000,
  })
  assert.deepEqual(segments, [
    { startMs: 0, endMs: 20_000, speed: 5 },
    { startMs: 20_000, endMs: 30_000, speed: 1 },
  ])
})

test('planRetime: no static runs → single 1x segment', () => {
  assert.deepEqual(planRetime([], 5000, {}), [{ startMs: 0, endMs: 5000, speed: 1 }])
})

// ── fastForwardSeconds ───────────────────────────────────────────────────────

test('fastForwardSeconds: marks every original-timeline second inside fast segments', () => {
  const secs = fastForwardSeconds([
    { startMs: 0, endMs: 2000, speed: 1 },
    { startMs: 2000, endMs: 5500, speed: 3 },
    { startMs: 5500, endMs: 8000, speed: 1 },
  ])
  assert.deepEqual(
    [...secs].sort((a, b) => a - b),
    [2, 3, 4, 5],
  )
})

// ── buildRetimeFilter ────────────────────────────────────────────────────────

test('buildRetimeFilter: one trim/setpts chain per segment plus a concat', () => {
  const filter = buildRetimeFilter([
    { startMs: 0, endMs: 11_000, speed: 1 },
    { startMs: 11_000, endMs: 29_000, speed: 4.5 },
  ])
  assert.match(filter, /\[0:v\]trim=start=0\.000:end=11\.000,setpts=\(PTS-STARTPTS\)\/1\[s0\]/)
  assert.match(filter, /\[0:v\]trim=start=11\.000:end=29\.000,setpts=\(PTS-STARTPTS\)\/4\.5\[s1\]/)
  assert.match(filter, /\[s0\]\[s1\]concat=n=2:v=1:a=0,fps=30\[v\]/)
})

// ── timer rendering ──────────────────────────────────────────────────────────

test('formatElapsed: zero-pads mm:ss', () => {
  assert.equal(formatElapsed(0), '00:00')
  assert.equal(formatElapsed(65), '01:05')
  assert.equal(formatElapsed(659), '10:59')
})

test('renderTimerFrame: emits a full RGBA frame with white glyph pixels and a translucent pill', () => {
  const buf = renderTimerFrame('00:05')
  assert.equal(buf.length, TIMER_W * TIMER_H * 4)
  let white = 0
  let pill = 0
  let transparent = 0
  for (let o = 0; o < buf.length; o += 4) {
    if (buf[o + 3] === 255 && buf[o] === 255) white++
    else if (buf[o + 3] === 150) pill++
    else if (buf[o + 3] === 0) transparent++
  }
  assert.ok(white > 100, `expected glyph pixels, got ${white}`)
  assert.ok(pill > 500, `expected pill background pixels, got ${pill}`)
  // The pill hugs the text — the left side of the canvas stays transparent
  // when the ">>" prefix is absent.
  assert.ok(transparent > 500, `expected transparent margin pixels, got ${transparent}`)
})

test('renderTimerFrame: the ">>" variant fills more of the canvas than the plain one', () => {
  const plain = renderTimerFrame('00:05')
  const fast = renderTimerFrame('>> 00:05')
  const opaque = (buf) => {
    let n = 0
    for (let o = 3; o < buf.length; o += 4) if (buf[o] > 0) n++
    return n
  }
  assert.ok(opaque(fast) > opaque(plain))
})

test('renderTimerStrip: one frame per second, inclusive of second 0 and the final second', () => {
  const strip = renderTimerStrip(3, new Set([2]))
  assert.equal(strip.length, 4 * TIMER_W * TIMER_H * 4)
})

// ── evenCropHeight ───────────────────────────────────────────────────────────

test('evenCropHeight: odd 613 rounds down to even 612', () => {
  assert.equal(evenCropHeight(613, 1000), 612)
})

test('evenCropHeight: already-even 612 stays 612', () => {
  assert.equal(evenCropHeight(612, 1000), 612)
})

test('evenCropHeight: null → null', () => {
  assert.equal(evenCropHeight(null, 1000), null)
})

test('evenCropHeight: undefined → null', () => {
  assert.equal(evenCropHeight(undefined, 1000), null)
})

test('evenCropHeight: 0 → null', () => {
  assert.equal(evenCropHeight(0, 1000), null)
})

test('evenCropHeight: negative → null', () => {
  assert.equal(evenCropHeight(-5, 1000), null)
})

test('evenCropHeight: fills viewport (equal) → null', () => {
  assert.equal(evenCropHeight(1000, 1000), null)
})

test('evenCropHeight: exceeds viewport → null', () => {
  assert.equal(evenCropHeight(1001, 1000), null)
})

test('evenCropHeight: 998 < 1000 → 998', () => {
  assert.equal(evenCropHeight(998, 1000), 998)
})

test('encoderCropHeight: odd full-height recording crops one pixel for H264', () => {
  assert.equal(encoderCropHeight(null, 829), 828)
})

test('encoderCropHeight: even full-height recording stays uncropped', () => {
  assert.equal(encoderCropHeight(null, 830), null)
})

test('encoderCropHeight: explicit measured crop still wins', () => {
  assert.equal(encoderCropHeight(613, 829), 612)
})

// ── buildBaseChain ───────────────────────────────────────────────────────────

test('buildBaseChain: no trim, no crop → fps only', () => {
  assert.equal(buildBaseChain({ trimSec: 0, cropHeight: null, fps: 30 }), '[0:v]fps=30[base]')
})

test('buildBaseChain: trim only → trim+setpts+fps', () => {
  assert.equal(
    buildBaseChain({ trimSec: 1.2, cropHeight: null, fps: 30 }),
    '[0:v]trim=start=1.200,setpts=PTS-STARTPTS,fps=30[base]',
  )
})

test('buildBaseChain: crop only → crop+fps, no trim', () => {
  const chain = buildBaseChain({ trimSec: 0, cropHeight: 612, fps: 30 })
  assert.equal(chain, '[0:v]crop=in_w:612:0:0,fps=30[base]')
})

test('buildBaseChain: trim+crop → trim+setpts+crop+fps', () => {
  const chain = buildBaseChain({ trimSec: 1.2, cropHeight: 612, fps: 30 })
  assert.equal(chain, '[0:v]trim=start=1.200,setpts=PTS-STARTPTS,crop=in_w:612:0:0,fps=30[base]')
})

test('buildBaseChain: crop appears after setpts and before fps', () => {
  const chain = buildBaseChain({ trimSec: 1.2, cropHeight: 612, fps: 30 })
  const cropIdx = chain.indexOf('crop=in_w:612:0:0')
  const setptsIdx = chain.indexOf('setpts=PTS-STARTPTS')
  const fpsIdx = chain.indexOf('fps=30')
  assert.ok(cropIdx > setptsIdx, 'crop must appear after setpts')
  assert.ok(cropIdx < fpsIdx, 'crop must appear before fps')
})

test('buildBaseChain: no crop= when cropHeight is null', () => {
  const chain = buildBaseChain({ trimSec: 0, cropHeight: null, fps: 30 })
  assert.ok(!chain.includes('crop='), 'should not include crop= when cropHeight is null')
})

// ── applyMinHeightRatio ──────────────────────────────────────────────────────

test('applyMinHeightRatio: crop already >= 50% width is preserved (even-rounded)', () => {
  // 600 wide, floor = 300; 420 >= 300 → keep, even
  assert.equal(applyMinHeightRatio(420, 600, 1000), 420)
})

test('applyMinHeightRatio: crop below 50% width is raised to the floor', () => {
  // 600 wide, floor = 300; measured 180 → raise to 300
  assert.equal(applyMinHeightRatio(180, 600, 1000), 300)
})

test('applyMinHeightRatio: floor above recorded height clamps to recorded height (no crop if == h)', () => {
  // 600 wide, floor = 300, recorded height 250 → cannot reach floor → null (no crop)
  assert.equal(applyMinHeightRatio(120, 600, 250), null)
})

test('applyMinHeightRatio: odd floor is rounded down to even', () => {
  // 601 wide, floor = ceil(300.5)=301 → even-round to 300
  assert.equal(applyMinHeightRatio(100, 601, 1000), 300)
})

test('applyMinHeightRatio: null/zero crop returns null', () => {
  assert.equal(applyMinHeightRatio(null, 600, 1000), null)
  assert.equal(applyMinHeightRatio(0, 600, 1000), null)
})

// ── buildTimerOverlayFilter ──────────────────────────────────────────────────

test('buildTimerOverlayFilter: timer on appends the overlay', () => {
  const f = buildTimerOverlayFilter('[0:v]crop=in_w:300:0:0,fps=30[base]', { timer: true })
  assert.match(f, /\[base\]\[1:v\]overlay=/)
})

test('buildTimerOverlayFilter: timer off maps the base chain only', () => {
  const f = buildTimerOverlayFilter('[0:v]crop=in_w:300:0:0,fps=30[base]', { timer: false })
  assert.doesNotMatch(f, /overlay=/)
  assert.match(f, /\[base\]/)
})
