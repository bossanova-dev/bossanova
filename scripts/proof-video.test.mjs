// Unit tests for the pure planning/rendering half of proof-video.mjs.
// The ffmpeg orchestration (postprocessProofVideo) is exercised against a
// real recording manually — these tests pin down the analysis math.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  computeFrameDiffs,
  computeFrameLuma,
  detectLeadingBlankMs,
  findStaticRuns,
  planRetime,
  retimedDurationMs,
  fastForwardSeconds,
  buildRetimeFilter,
  formatElapsed,
  renderTimerFrame,
  renderTimerStrip,
  TIMER_W,
  TIMER_H,
} from './proof-video.mjs';

// ── computeFrameDiffs ────────────────────────────────────────────────────────

test('computeFrameDiffs: identical frames diff to 0, changed frames to the mean delta', () => {
  const frameBytes = 4;
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
  ]);
  assert.deepEqual(computeFrameDiffs(raw, frameBytes), [0, 10]);
});

test('computeFrameDiffs: ignores a trailing partial frame', () => {
  const raw = Buffer.from([1, 1, 2, 2, 9]); // 2 full frames of 2 bytes + 1 stray byte
  assert.deepEqual(computeFrameDiffs(raw, 2), [1]);
});

// ── computeFrameLuma / detectLeadingBlankMs ──────────────────────────────────

test('computeFrameLuma: mean byte value per frame', () => {
  const raw = Buffer.from([10, 10, 10, 10, 200, 200, 200, 200]);
  assert.deepEqual(computeFrameLuma(raw, 4), [10, 200]);
});

test('detectLeadingBlankMs: leading near-white run → runLen + buffer', () => {
  // fps=4 → 250ms/frame. 2 white frames + dark → 2*250 + 150 buffer = 650.
  const luma = [234, 234, 25, 25, 33, 30];
  assert.equal(detectLeadingBlankMs(luma, { fps: 4 }), 650);
});

test('detectLeadingBlankMs: dark start (gen-AI) → 0, NOT trimmed (regression)', () => {
  const luma = [25, 30, 234, 28, 31]; // a bright frame mid-stream must not trigger
  assert.equal(detectLeadingBlankMs(luma, { fps: 4 }), 0);
});

test('detectLeadingBlankMs: whole-video-bright is capped, never guts content', () => {
  const luma = Array(40).fill(234); // 40*250+150 = 10150ms → clamped to cap
  assert.equal(detectLeadingBlankMs(luma, { fps: 4, capMs: 2000 }), 2000);
});

test('detectLeadingBlankMs: trimMs clamped to capMs', () => {
  const luma = [...Array(10).fill(234), 25]; // 10*250+150 = 2650 → clamped 2000
  assert.equal(detectLeadingBlankMs(luma, { fps: 4, capMs: 2000 }), 2000);
});

test('detectLeadingBlankMs: empty luma → 0', () => {
  assert.equal(detectLeadingBlankMs([], { fps: 4 }), 0);
});

// ── findStaticRuns ───────────────────────────────────────────────────────────

test('findStaticRuns: reports runs meeting minRunMs and drops short ones', () => {
  // 4 fps → each diff covers 250ms. 40 static diffs ≈ 10.25s span.
  const diffs = [9, ...Array(40).fill(0.1), 9, 9, ...Array(4).fill(0.1), 9];
  const runs = findStaticRuns(diffs, { fps: 4, threshold: 1.5, minRunMs: 8000 });
  assert.equal(runs.length, 1);
  // run over diffs[1..40] → frames 1..41 → 250ms..10250ms
  assert.deepEqual(runs[0], { startMs: 250, endMs: 10250 });
});

test('findStaticRuns: a run reaching the end of the video is flushed', () => {
  const diffs = [9, ...Array(40).fill(0.1)];
  const runs = findStaticRuns(diffs, { fps: 4, threshold: 1.5, minRunMs: 8000 });
  assert.equal(runs.length, 1);
  assert.equal(runs[0].endMs, 10250);
});

test('findStaticRuns: empty and all-active inputs produce no runs', () => {
  assert.deepEqual(findStaticRuns([], {}), []);
  assert.deepEqual(findStaticRuns(Array(100).fill(9), { fps: 4 }), []);
});

// ── planRetime ───────────────────────────────────────────────────────────────

test('planRetime: pads the run, compresses the middle, covers the full duration', () => {
  const segments = planRetime([{ startMs: 10_000, endMs: 30_000 }], 60_000, {
    padMs: 1000,
    targetMs: 4000,
  });
  assert.deepEqual(segments, [
    { startMs: 0, endMs: 11_000, speed: 1 },
    { startMs: 11_000, endMs: 29_000, speed: 4.5 }, // 18s → 4s
    { startMs: 29_000, endMs: 60_000, speed: 1 },
  ]);
  assert.equal(retimedDurationMs(segments), 11_000 + 4_000 + 31_000);
});

test('planRetime: skips runs whose padded middle would not shrink', () => {
  // 6s run − 2s pad = 4s middle = target → nothing to compress.
  const segments = planRetime([{ startMs: 0, endMs: 6000 }], 20_000, {
    padMs: 1000,
    targetMs: 4000,
  });
  assert.deepEqual(segments, [{ startMs: 0, endMs: 20_000, speed: 1 }]);
});

test('planRetime: run touching the start emits no empty leading segment', () => {
  const segments = planRetime([{ startMs: 0, endMs: 20_000 }], 30_000, {
    padMs: 0,
    targetMs: 4000,
  });
  assert.deepEqual(segments, [
    { startMs: 0, endMs: 20_000, speed: 5 },
    { startMs: 20_000, endMs: 30_000, speed: 1 },
  ]);
});

test('planRetime: no static runs → single 1x segment', () => {
  assert.deepEqual(planRetime([], 5000, {}), [{ startMs: 0, endMs: 5000, speed: 1 }]);
});

// ── fastForwardSeconds ───────────────────────────────────────────────────────

test('fastForwardSeconds: marks every original-timeline second inside fast segments', () => {
  const secs = fastForwardSeconds([
    { startMs: 0, endMs: 2000, speed: 1 },
    { startMs: 2000, endMs: 5500, speed: 3 },
    { startMs: 5500, endMs: 8000, speed: 1 },
  ]);
  assert.deepEqual(
    [...secs].sort((a, b) => a - b),
    [2, 3, 4, 5],
  );
});

// ── buildRetimeFilter ────────────────────────────────────────────────────────

test('buildRetimeFilter: one trim/setpts chain per segment plus a concat', () => {
  const filter = buildRetimeFilter([
    { startMs: 0, endMs: 11_000, speed: 1 },
    { startMs: 11_000, endMs: 29_000, speed: 4.5 },
  ]);
  assert.match(filter, /\[0:v\]trim=start=0\.000:end=11\.000,setpts=\(PTS-STARTPTS\)\/1\[s0\]/);
  assert.match(filter, /\[0:v\]trim=start=11\.000:end=29\.000,setpts=\(PTS-STARTPTS\)\/4\.5\[s1\]/);
  assert.match(filter, /\[s0\]\[s1\]concat=n=2:v=1:a=0,fps=30\[v\]/);
});

// ── timer rendering ──────────────────────────────────────────────────────────

test('formatElapsed: zero-pads mm:ss', () => {
  assert.equal(formatElapsed(0), '00:00');
  assert.equal(formatElapsed(65), '01:05');
  assert.equal(formatElapsed(659), '10:59');
});

test('renderTimerFrame: emits a full RGBA frame with white glyph pixels and a translucent pill', () => {
  const buf = renderTimerFrame('00:05');
  assert.equal(buf.length, TIMER_W * TIMER_H * 4);
  let white = 0;
  let pill = 0;
  let transparent = 0;
  for (let o = 0; o < buf.length; o += 4) {
    if (buf[o + 3] === 255 && buf[o] === 255) white++;
    else if (buf[o + 3] === 150) pill++;
    else if (buf[o + 3] === 0) transparent++;
  }
  assert.ok(white > 100, `expected glyph pixels, got ${white}`);
  assert.ok(pill > 500, `expected pill background pixels, got ${pill}`);
  // The pill hugs the text — the left side of the canvas stays transparent
  // when the ">>" prefix is absent.
  assert.ok(transparent > 500, `expected transparent margin pixels, got ${transparent}`);
});

test('renderTimerFrame: the ">>" variant fills more of the canvas than the plain one', () => {
  const plain = renderTimerFrame('00:05');
  const fast = renderTimerFrame('>> 00:05');
  const opaque = (buf) => {
    let n = 0;
    for (let o = 3; o < buf.length; o += 4) if (buf[o] > 0) n++;
    return n;
  };
  assert.ok(opaque(fast) > opaque(plain));
});

test('renderTimerStrip: one frame per second, inclusive of second 0 and the final second', () => {
  const strip = renderTimerStrip(3, new Set([2]));
  assert.equal(strip.length, 4 * TIMER_W * TIMER_H * 4);
});
