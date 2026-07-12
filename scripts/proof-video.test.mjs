// Unit tests for the pure planning/rendering half of proof-video.mjs.
// Most of the ffmpeg orchestration (postprocessProofVideo) is exercised
// against a real recording manually — these tests pin down the analysis
// math. One postprocessProofVideo test below (ffmpeg-gated) pins the
// `timeline` shape on the no-condense success branch.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import {
  buildTimerOverlayFilter,
  captionInputArgs,
  selectEncoderArgs,
  renderCaptionOverlays,
  computeFrameDiffs,
  computeFrameLuma,
  detectLeadingBlankMs,
  detectLeadingStaticMs,
  findStaticRuns,
  planRetime,
  retimedDurationMs,
  mapSourceToOutputMs,
  postprocessProofVideo,
  fastForwardSeconds,
  buildRetimeFilter,
  formatElapsed,
  renderTimerFrame,
  renderTimerStrip,
  evenCropHeight,
  encoderCropHeight,
  buildBaseChain,
  applyMinHeightRatio,
  planCaptionWindows,
  LEADING_STATIC_DIFF,
  TIMER_W,
  TIMER_H,
} from './proof-video.mjs'

function requireFfmpeg(t) {
  const check = spawnSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  if (check.status !== 0) {
    t.skip('ffmpeg is required for this test')
    return false
  }
  return true
}

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

// ── mapSourceToOutputMs (BOS-140/P3b) ───────────────────────────────────────

test('mapSourceToOutputMs: null timeline → null (degrade: chapter without a link)', () => {
  assert.equal(mapSourceToOutputMs(null, 12345), null)
})

test('mapSourceToOutputMs: non-finite sourceMs → null', () => {
  const identity = { trimMs: 0, segments: [], introMs: 0 }
  assert.equal(mapSourceToOutputMs(identity, undefined), null)
  assert.equal(mapSourceToOutputMs(identity, Number.NaN), null)
})

test('mapSourceToOutputMs: identity timeline (no trim, no segments, no intro) → passthrough', () => {
  assert.equal(mapSourceToOutputMs({ trimMs: 0, segments: [], introMs: 0 }, 4200), 4200)
})

test('mapSourceToOutputMs: trim-only → sourceMs - trimMs, clamped to 0', () => {
  const tl = { trimMs: 1000, segments: [], introMs: 0 }
  assert.equal(mapSourceToOutputMs(tl, 5000), 4000)
  assert.equal(mapSourceToOutputMs(tl, 500), 0, 'source before the trimmed head clamps to 0')
})

test('mapSourceToOutputMs: 3-segment retime plan (1x/10x/1x) maps into the fast segment', () => {
  const segments = [
    { startMs: 0, endMs: 10_000, speed: 1 },
    { startMs: 10_000, endMs: 50_000, speed: 10 },
    { startMs: 50_000, endMs: 60_000, speed: 1 },
  ]
  const tl = { trimMs: 0, segments, introMs: 0 }
  // 10_000ms of 1x + 20_000ms into the 10x segment (2_000ms output).
  assert.equal(mapSourceToOutputMs(tl, 30_000), 12_000)
})

test('mapSourceToOutputMs: a point past the fast segment adds the fully-retimed prefix', () => {
  const segments = [
    { startMs: 0, endMs: 10_000, speed: 1 },
    { startMs: 10_000, endMs: 50_000, speed: 10 },
    { startMs: 50_000, endMs: 60_000, speed: 1 },
  ]
  const tl = { trimMs: 0, segments, introMs: 0 }
  // 10_000 (1x) + 4_000 (the whole 40_000ms fast segment / 10) + 5_000 (1x tail).
  assert.equal(mapSourceToOutputMs(tl, 55_000), 10_000 + 4_000 + 5_000)
})

test('mapSourceToOutputMs: introMs adds a flat offset on top of the retimed value', () => {
  const tl = { trimMs: 0, segments: [{ startMs: 0, endMs: 10_000, speed: 1 }], introMs: 2000 }
  assert.equal(mapSourceToOutputMs(tl, 4000), 6000)
})

test('mapSourceToOutputMs: cross-check against retimedDurationMs — mapping the full duration equals introMs + retimedDurationMs(segments)', () => {
  const segments = [
    { startMs: 0, endMs: 10_000, speed: 1 },
    { startMs: 10_000, endMs: 50_000, speed: 10 },
    { startMs: 50_000, endMs: 60_000, speed: 1 },
  ]
  const durationMs = 60_000
  const introMs = 2000
  const tl = { trimMs: 0, segments, introMs }
  assert.equal(mapSourceToOutputMs(tl, durationMs), introMs + retimedDurationMs(segments))
})

// ── postprocessProofVideo: timeline shape (BOS-140/P3b) ─────────────────────

test('postprocessProofVideo: no-condense branch returns timeline {trimMs, segments, introMs:0}', (t) => {
  if (!requireFfmpeg(t)) return
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-video-timeline-'))
  try {
    const webmPath = path.join(dir, 'in.webm')
    // A short, constant-color clip: too brief for any static run to clear
    // MIN_STATIC_RUN_MS, so planRetime degenerates to a single identity
    // segment (the no-condense branch) and neither leading-trim detector fires
    // (an all-static/all-bright clip is "not a lead-in" by design).
    const gen = spawnSync('ffmpeg', [
      '-y',
      '-loglevel',
      'error',
      '-f',
      'lavfi',
      '-i',
      'color=c=blue:s=64x64:d=0.5',
      '-c:v',
      'libvpx',
      '-pix_fmt',
      'yuv420p',
      webmPath,
    ])
    assert.equal(gen.status, 0, 'synthetic webm fixture must be created')

    const result = postprocessProofVideo({
      webmPath,
      timedPath: path.join(dir, 'timed.mp4'),
      outPath: path.join(dir, 'out.mp4'),
      scratchPath: path.join(dir, 'scratch.raw'),
    })

    assert.equal(result.ok, true)
    assert.equal(result.condensed, false)
    assert.ok(result.timeline, 'a successful result must carry a timeline')
    assert.equal(result.timeline.trimMs, 0)
    assert.equal(result.timeline.introMs, 0, 'no intro card was supplied')
    assert.deepEqual(result.timeline.segments, [{ startMs: 0, endMs: result.outputMs, speed: 1 }])
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
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

test('buildBaseChain: trim only → CFR-normalize FIRST, then trim+setpts (BOS-251)', () => {
  // fps must come BEFORE trim: agg webms are sparse VFR, and trimming them
  // first drops the frame spanning the boundary, rebasing the clock onto the
  // next repaint (content desyncs from every burned overlay).
  assert.equal(
    buildBaseChain({ trimSec: 1.2, cropHeight: null, fps: 30 }),
    '[0:v]fps=30,trim=start=1.200,setpts=PTS-STARTPTS[base]',
  )
})

test('buildBaseChain: crop only → crop+fps, no trim', () => {
  const chain = buildBaseChain({ trimSec: 0, cropHeight: 612, fps: 30 })
  assert.equal(chain, '[0:v]crop=in_w:612:0:0,fps=30[base]')
})

test('buildBaseChain: trim+crop → fps+trim+setpts+crop', () => {
  const chain = buildBaseChain({ trimSec: 1.2, cropHeight: 612, fps: 30 })
  assert.equal(chain, '[0:v]fps=30,trim=start=1.200,setpts=PTS-STARTPTS,crop=in_w:612:0:0[base]')
})

test('buildBaseChain: crop appears after setpts; fps leads the chain', () => {
  const chain = buildBaseChain({ trimSec: 1.2, cropHeight: 612, fps: 30 })
  assert.ok(chain.startsWith('[0:v]fps=30,'))
  assert.ok(chain.indexOf('setpts') < chain.indexOf('crop='))
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

// ── planCaptionWindows ───────────────────────────────────────────────────────

test('planCaptionWindows: sequential captions; each window ends at the next start', () => {
  const windows = planCaptionWindows(
    [
      { seq: 1, caption: 'Open settings', startMs: 0 },
      { seq: 2, caption: 'Toggle the flag', startMs: 2000 },
    ],
    5000,
  )
  assert.deepEqual(windows, [
    { text: 'Open settings', startMs: 0, endMs: 2000 },
    { text: 'Toggle the flag', startMs: 2000, endMs: 5000 },
  ])
})

test('planCaptionWindows: the last caption extends to the total duration', () => {
  const windows = planCaptionWindows([{ seq: 1, caption: 'Only step', startMs: 500 }], 9000)
  assert.deepEqual(windows, [{ text: 'Only step', startMs: 500, endMs: 9000 }])
})

test('planCaptionWindows: a blank (un-narrated) screen ENDS the prior caption window (BOS-251)', () => {
  const windows = planCaptionWindows(
    [
      { seq: 1, caption: 'First', startMs: 0 },
      { seq: 2, caption: '   ', startMs: 2000 }, // un-narrated screen → strip clears
      { seq: 3, caption: 'Second', startMs: 3000 },
    ],
    5000,
  )
  assert.deepEqual(windows, [
    { text: 'First', startMs: 0, endMs: 2000 }, // never burned over the foreign screen
    { text: 'Second', startMs: 3000, endMs: 5000 },
  ])
})

test('planCaptionWindows: a trailing blank screen ends the prior caption (no stale strip)', () => {
  const windows = planCaptionWindows(
    [
      { seq: 1, caption: 'First', startMs: 0 },
      { seq: 2, caption: '', startMs: 2000 },
    ],
    5000,
  )
  assert.deepEqual(windows, [{ text: 'First', startMs: 0, endMs: 2000 }])
})

test('planCaptionWindows: collapses whitespace via formatCaption', () => {
  const windows = planCaptionWindows([{ seq: 1, caption: 'a\n\n  b\tc ', startMs: 0 }], 1000)
  assert.deepEqual(windows, [{ text: 'a b c', startMs: 0, endMs: 1000 }])
})

test('planCaptionWindows: empty input and all-blank input → []', () => {
  assert.deepEqual(planCaptionWindows([], 5000), [])
  assert.deepEqual(planCaptionWindows([{ seq: 1, caption: '  ', startMs: 0 }], 5000), [])
})

test('planCaptionWindows: unsorted timings are sorted by startMs', () => {
  const windows = planCaptionWindows(
    [
      { seq: 2, caption: 'B', startMs: 2000 },
      { seq: 1, caption: 'A', startMs: 0 },
    ],
    4000,
  )
  assert.deepEqual(windows, [
    { text: 'A', startMs: 0, endMs: 2000 },
    { text: 'B', startMs: 2000, endMs: 4000 },
  ])
})

test('planCaptionWindows: a zero-width final window (start == total) is dropped', () => {
  const windows = planCaptionWindows(
    [
      { seq: 1, caption: 'A', startMs: 0 },
      { seq: 2, caption: 'B', startMs: 5000 },
    ],
    5000,
  )
  // B starts exactly at the end → no time to show → dropped; A still spans to B's start.
  assert.deepEqual(windows, [{ text: 'A', startMs: 0, endMs: 5000 }])
})

// ── renderCaptionOverlays (clock offset + degradation, BOS-121) ───────────────

function withTmpDir(fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'caption-overlays-'))
  try {
    return fn(dir)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
}

// A stub strip renderer that actually writes the PNG (postprocess checks existsSync).
function fakeStripWriter() {
  const calls = []
  const fn = ({ text, width, output }) => {
    calls.push({ text, width, output })
    fs.writeFileSync(output, 'fake-strip-png')
    return true
  }
  fn.calls = calls
  return fn
}

test('renderCaptionOverlays: degrades to [] without a renderer / timings / width', () => {
  const base = {
    captionTimings: [{ caption: 'A', startMs: 0 }],
    width: 1120,
    fullDurationMs: 5000,
    trimMs: 0,
    durationMs: 5000,
    inputBase: 2,
    dir: '/tmp',
    pngPaths: [],
  }
  assert.deepEqual(renderCaptionOverlays({ ...base, renderCaptionStrip: undefined }), [])
  assert.deepEqual(
    renderCaptionOverlays({ ...base, renderCaptionStrip: fakeStripWriter(), captionTimings: [] }),
    [],
  )
  assert.deepEqual(
    renderCaptionOverlays({ ...base, renderCaptionStrip: fakeStripWriter(), width: 0 }),
    [],
  )
})

test('renderCaptionOverlays: assigns input indices from inputBase and offsets windows by the trim', () => {
  withTmpDir((dir) => {
    const renderCaptionStrip = fakeStripWriter()
    const pngPaths = []
    const overlays = renderCaptionOverlays({
      captionTimings: [
        { seq: 1, caption: 'First', startMs: 1000 },
        { seq: 2, caption: 'Second', startMs: 3000 },
      ],
      renderCaptionStrip,
      width: 1120,
      fullDurationMs: 6000,
      trimMs: 1000, // leading trim — every window shifts earlier by 1s
      durationMs: 5000,
      inputBase: 2,
      dir,
      pngPaths,
    })
    assert.deepEqual(overlays, [
      { inputIndex: 2, startSec: 0, endSec: 2 }, // 1000→0, 3000→2000 (minus 1000 trim)
      { inputIndex: 3, startSec: 2, endSec: 5 }, // 3000→2000, end 6000→5000
    ])
    assert.equal(pngPaths.length, 2)
    assert.equal(renderCaptionStrip.calls[0].width, 1120)
    for (const p of pngPaths) assert.ok(fs.existsSync(p), 'strip png written')
  })
})

test('renderCaptionOverlays: drops a window fully inside the trimmed-away head', () => {
  withTmpDir((dir) => {
    const pngPaths = []
    const overlays = renderCaptionOverlays({
      captionTimings: [
        { seq: 1, caption: 'Boot noise', startMs: 0 }, // window [0,800) — entirely before the 1000ms trim
        { seq: 2, caption: 'Real step', startMs: 800 },
      ],
      renderCaptionStrip: fakeStripWriter(),
      width: 800,
      fullDurationMs: 5000,
      trimMs: 1000,
      durationMs: 4000,
      inputBase: 2,
      dir,
      pngPaths,
    })
    // First window [0,800) → after −1000 trim, endSec ≤ 0 → dropped. The second
    // window is kept and gets the FIRST input index (contiguous from inputBase).
    assert.deepEqual(overlays, [{ inputIndex: 2, startSec: 0, endSec: 4 }])
    assert.equal(pngPaths.length, 1)
  })
})

test('renderCaptionOverlays: a thrown render error degrades to [] and cleans up partial PNGs', () => {
  withTmpDir((dir) => {
    const pngPaths = []
    // First strip writes a PNG then a later one throws → catch path must clean up
    // the already-written PNG and return [] (no captions, full video still ships).
    let n = 0
    const renderCaptionStrip = ({ output }) => {
      n += 1
      if (n === 1) {
        fs.writeFileSync(output, 'partial')
        return true
      }
      throw new Error('boom')
    }
    const overlays = renderCaptionOverlays({
      captionTimings: [
        { seq: 1, caption: 'First', startMs: 0 },
        { seq: 2, caption: 'Second', startMs: 2000 },
      ],
      renderCaptionStrip,
      width: 640,
      fullDurationMs: 4000,
      trimMs: 0,
      durationMs: 4000,
      inputBase: 2,
      dir,
      pngPaths,
    })
    assert.deepEqual(overlays, [])
    assert.deepEqual(pngPaths, [], 'caller-visible pngPaths emptied on the catch path')
    // No leaked strip files remain in the dir.
    assert.deepEqual(
      fs.readdirSync(dir).filter((f) => f.startsWith('caption-strip-')),
      [],
    )
  })
})

test('renderCaptionOverlays: a failed strip render is skipped, indices stay contiguous', () => {
  withTmpDir((dir) => {
    const pngPaths = []
    // Renderer fails (returns false) for the first caption, succeeds for the second.
    let n = 0
    const renderCaptionStrip = ({ output }) => {
      n += 1
      if (n === 1) return false
      fs.writeFileSync(output, 'ok')
      return true
    }
    const overlays = renderCaptionOverlays({
      captionTimings: [
        { seq: 1, caption: 'Fails to render', startMs: 0 },
        { seq: 2, caption: 'Renders fine', startMs: 2000 },
      ],
      renderCaptionStrip,
      width: 640,
      fullDurationMs: 4000,
      trimMs: 0,
      durationMs: 4000,
      inputBase: 1,
      dir,
      pngPaths,
    })
    assert.deepEqual(overlays, [{ inputIndex: 1, startSec: 2, endSec: 4 }])
    assert.equal(pngPaths.length, 1)
  })
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

test('buildTimerOverlayFilter: a caption overlays along the bottom edge with an enable window before the timer', () => {
  const f = buildTimerOverlayFilter('[0:v]fps=30[base]', {
    timer: true,
    captions: [{ inputIndex: 2, startSec: 0, endSec: 2 }],
  })
  // caption overlaid onto [base] at top-left, gated by enable, output to an intermediate label
  assert.match(f, /\[base\]\[2:v\]overlay=x=0:y=main_h-overlay_h:enable='between\(t,0\.000,2\.000\)'[^;]*\[ov0\]/)
  // timer overlaid last onto the caption result, output [v], at the top-right corner (y=44)
  assert.match(f, /\[ov0\]\[1:v\]overlay=x=main_w-overlay_w-14:y=44:eof_action=repeat\[v\]/)
})

test('buildTimerOverlayFilter: captions without a timer still terminate in [v]', () => {
  const f = buildTimerOverlayFilter('[0:v]fps=30[base]', {
    timer: false,
    captions: [
      { inputIndex: 1, startSec: 0, endSec: 1.5 },
      { inputIndex: 2, startSec: 1.5, endSec: 4 },
    ],
  })
  assert.match(f, /\[base\]\[1:v\]overlay=x=0:y=main_h-overlay_h:enable='between\(t,0\.000,1\.500\)'[^;]*\[ov0\]/)
  assert.match(f, /\[ov0\]\[2:v\]overlay=x=0:y=main_h-overlay_h:enable='between\(t,1\.500,4\.000\)'[^;]*\[v\]/)
})

test('buildTimerOverlayFilter: no captions + timer is byte-identical to the timer-only graph', () => {
  const base = '[0:v]crop=in_w:300:0:0,fps=30[base]'
  assert.equal(
    buildTimerOverlayFilter(base, { timer: true, captions: [] }),
    buildTimerOverlayFilter(base, { timer: true }),
  )
})

test('buildTimerOverlayFilter: multiple caption inputs use the passed-in input indices', () => {
  const f = buildTimerOverlayFilter('[0:v]fps=30[base]', {
    timer: true,
    captions: [
      { inputIndex: 2, startSec: 0, endSec: 1 },
      { inputIndex: 3, startSec: 1, endSec: 2 },
    ],
  })
  assert.match(f, /\[2:v\]overlay=/)
  assert.match(f, /\[3:v\]overlay=/)
  assert.match(f, /\[1:v\]overlay=x=main_w-overlay_w-14/) // timer still input 1
})

// ── captionInputArgs ─────────────────────────────────────────────────────────

test('captionInputArgs: each strip is a single-frame -i input, NEVER -loop 1', () => {
  // Regression: a `-loop 1` image is an INFINITE input; with overlay
  // eof_action=repeat it unbounds the output past the base clip (one looped strip
  // stretched an 80s clip to ~1338s; seven ran ffmpeg 30+ min producing an
  // ever-growing file). Strips must be fed as single frames so the output stays
  // bounded — eof_action=repeat still holds the caption, enable gates its window.
  const args = captionInputArgs(['/tmp/cap-0.png', '/tmp/cap-1.png'])
  assert.deepEqual(args, ['-i', '/tmp/cap-0.png', '-i', '/tmp/cap-1.png'])
  assert.ok(
    !args.includes('-loop'),
    'caption strips must not use -loop (infinite input unbounds the overlay)',
  )
})

test('captionInputArgs: no strips → no inputs', () => {
  assert.deepEqual(captionInputArgs([]), [])
})

// ── selectEncoderArgs (opt-in hardware encode) ───────────────────────────────

test('selectEncoderArgs: VideoToolbox only when requested AND available', () => {
  const hw = selectEncoderArgs({ hwRequested: true, hwAvailable: true })
  assert.deepEqual(hw.slice(0, 2), ['-c:v', 'h264_videotoolbox'])
  // VideoToolbox ignores -crf; quality is via -q:v.
  assert.ok(hw.includes('-q:v') && !hw.includes('-crf'))
})

test('selectEncoderArgs: defaults to libx264 when not requested or unavailable', () => {
  for (const opts of [
    { hwRequested: false, hwAvailable: true },
    { hwRequested: true, hwAvailable: false },
    { hwRequested: false, hwAvailable: false },
  ]) {
    const args = selectEncoderArgs(opts)
    assert.deepEqual(args.slice(0, 2), ['-c:v', 'libx264'], JSON.stringify(opts))
    assert.ok(args.includes('-crf'))
  }
})

// ── applySceneFloors (BOS-251 per-scene watchability floor) ──────────────────

test('applySceneFloors: a short scene window is slowed to the floor', async () => {
  const { applySceneFloors, retimedDurationMs } = await import('./proof-video.mjs')
  // Two scenes: [0,1000) and [1000,10000). Scene 1 outputs 1000ms < 3000ms floor.
  const segments = [{ startMs: 0, endMs: 10_000, speed: 1 }]
  const out = applySceneFloors(segments, [0, 1000], 10_000)
  // Plan still tiles [0, durationMs].
  assert.equal(out[0].startMs, 0)
  assert.equal(out[out.length - 1].endMs, 10_000)
  for (let i = 1; i < out.length; i++) assert.equal(out[i].startMs, out[i - 1].endMs)
  // Scene 1 (first 1000ms) plays at ~1/3 speed → ~3000ms of output.
  const scene1 = out.filter((s) => s.endMs <= 1000)
  const scene1Out = scene1.reduce((t, s) => t + (s.endMs - s.startMs) / s.speed, 0)
  assert.ok(scene1Out >= 2900, `scene 1 output ${scene1Out}ms should be near the 3000ms floor`)
  // Scene 2 (9000ms at speed 1) is untouched.
  const scene2 = out.filter((s) => s.startMs >= 1000)
  assert.ok(scene2.every((s) => s.speed === 1))
  assert.ok(retimedDurationMs(out) >= 11_900)
})

test('applySceneFloors: windows already at/over the floor are unchanged', async () => {
  const { applySceneFloors } = await import('./proof-video.mjs')
  const segments = [{ startMs: 0, endMs: 12_000, speed: 1 }]
  const out = applySceneFloors(segments, [0, 4000, 8000], 12_000)
  assert.deepEqual(
    out.map((s) => s.speed),
    [1, 1, 1],
  )
})

test('applySceneFloors: non-finite boundaries (null cast reads) are dropped', async () => {
  const { applySceneFloors } = await import('./proof-video.mjs')
  const segments = [{ startMs: 0, endMs: 10_000, speed: 1 }]
  const out = applySceneFloors(segments, [0, null, NaN, undefined], 10_000)
  assert.deepEqual(out, [{ startMs: 0, endMs: 10_000, speed: 1 }])
})

test('applySceneFloors: floors compose with existing speedup segments', async () => {
  const { applySceneFloors } = await import('./proof-video.mjs')
  // Scene 2 window [1000, 21000) contains a squeezed static run at speed 4 —
  // output 1000+ (20000/4)=6000ms ≥ floor, so untouched; scene 1 window is 500ms → slowed.
  const segments = [
    { startMs: 0, endMs: 1000, speed: 1 },
    { startMs: 1000, endMs: 21_000, speed: 4 },
  ]
  const out = applySceneFloors(segments, [0, 500], 21_000)
  const scene1 = out.filter((s) => s.endMs <= 500)
  assert.ok(scene1.every((s) => s.speed < 1))
  const post = out.filter((s) => s.startMs >= 1000)
  assert.ok(post.every((s) => s.speed === 4))
})

test('applySceneFloors: empty boundaries or zero duration are no-ops', async () => {
  const { applySceneFloors } = await import('./proof-video.mjs')
  const segments = [{ startMs: 0, endMs: 5000, speed: 1 }]
  assert.deepEqual(applySceneFloors(segments, [], 5000), segments)
  assert.deepEqual(applySceneFloors(segments, [0], 0), segments)
})

// ── BOS-251: trailing dead-air cut + caption readability floor ────────────────

test('buildBaseChain: endSec adds a trim end on the same chain', async () => {
  const { buildBaseChain } = await import('./proof-video.mjs')
  assert.equal(
    buildBaseChain({ trimSec: 1.5, endSec: 10.25, cropHeight: null, fps: 30 }),
    '[0:v]fps=30,trim=start=1.500:end=10.250,setpts=PTS-STARTPTS[base]',
  )
  assert.equal(
    buildBaseChain({ trimSec: 0, endSec: 8, cropHeight: null, fps: 30 }),
    '[0:v]fps=30,trim=start=0.000:end=8.000,setpts=PTS-STARTPTS[base]',
  )
  // No end, no trim → byte-identical to the historical chain.
  assert.equal(buildBaseChain({ trimSec: 0, cropHeight: null, fps: 30 }), '[0:v]fps=30[base]')
  // A nonsense end (before the trim) is ignored.
  assert.equal(
    buildBaseChain({ trimSec: 5, endSec: 3, cropHeight: null, fps: 30 }),
    '[0:v]fps=30,trim=start=5.000,setpts=PTS-STARTPTS[base]',
  )
})

test('applySceneFloors: caption-window boundaries stretch short caption windows', async () => {
  const { applySceneFloors, CAPTION_MIN_OUTPUT_MS } = await import('./proof-video.mjs')
  // Three captions 500ms apart on a 10s timeline: each window is under the 2s
  // floor and gets slowed; the post-caption remainder (8.5s) is untouched.
  const segments = [{ startMs: 0, endMs: 10_000, speed: 1 }]
  const out = applySceneFloors(segments, [0, 500, 1000], 10_000, CAPTION_MIN_OUTPUT_MS)
  const w1 = out.filter((s) => s.endMs <= 500)
  const w2 = out.filter((s) => s.startMs >= 500 && s.endMs <= 1000)
  const rest = out.filter((s) => s.startMs >= 1000)
  assert.ok(w1.every((s) => s.speed < 1))
  assert.ok(w2.every((s) => s.speed < 1))
  assert.ok(rest.every((s) => s.speed === 1))
})

test('planCaptionWindows: each caption covers ONLY its own screen — strict windows', async () => {
  const { planCaptionWindows } = await import('./proof-video.mjs')
  const windows = planCaptionWindows(
    [
      { caption: 'Pressing Down to highlight the session.', startMs: 1000 },
      { caption: '', startMs: 1400 }, // esc in the same turn: strip clears
      { caption: '', startMs: 6000 },
      { caption: 'Opening Settings.', startMs: 9000 },
    ],
    12_000,
  )
  assert.deepEqual(windows, [
    { text: 'Pressing Down to highlight the session.', startMs: 1000, endMs: 1400 },
    { text: 'Opening Settings.', startMs: 9000, endMs: 12_000 },
  ])
})
