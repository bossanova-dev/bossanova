// Unit tests for the pure arg builders and HTML builder in proof-video-intro.mjs.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  INTRO_SEC,
  buildIntroClipArgs,
  buildIntroConcatArgs,
  buildIntroCardHtml,
} from './proof-video-intro.mjs';

// ── buildIntroClipArgs ───────────────────────────────────────────────────────

test('buildIntroClipArgs: loops the png for INTRO_SEC at the video geometry/fps', () => {
  const a = buildIntroClipArgs({
    pngPath: '/p/intro.png',
    width: 1280,
    height: 720,
    fps: 30,
    outPath: '/p/intro.mp4',
  });
  assert.ok(a.includes('-loop'), 'should contain -loop');
  assert.ok(a.includes('1'), 'should contain 1 (loop count)');
  assert.ok(a.includes('-t'), 'should contain -t');
  assert.ok(a.includes(String(INTRO_SEC)), `should contain INTRO_SEC duration (${INTRO_SEC})`);
  assert.ok(a.join(' ').includes('scale=1280:720'), 'should include scale filter');
  assert.ok(a.join(' ').includes('fps=30'), 'should include fps filter');
  assert.ok(a.join(' ').includes('setsar=1'), 'should pin SAR to 1:1 to avoid concat mismatch');
  assert.equal(a[a.length - 1], '/p/intro.mp4');
});

test('buildIntroClipArgs: -loop 1 and -t 2 appear as consecutive pairs', () => {
  const a = buildIntroClipArgs({
    pngPath: '/in.png',
    width: 640,
    height: 360,
    fps: 24,
    outPath: '/out.mp4',
  });
  const loopIdx = a.indexOf('-loop');
  assert.ok(loopIdx >= 0, '-loop flag present');
  assert.equal(a[loopIdx + 1], '1', '-loop value is 1');
  const tIdx = a.indexOf('-t');
  assert.ok(tIdx >= 0, '-t flag present');
  assert.equal(a[tIdx + 1], String(INTRO_SEC), `-t value is INTRO_SEC (${INTRO_SEC})`);
});

// ── buildIntroConcatArgs ─────────────────────────────────────────────────────

test('buildIntroConcatArgs: concats intro then main into out', () => {
  const a = buildIntroConcatArgs({
    introPath: '/p/intro.mp4',
    mainPath: '/p/proof.mp4',
    outPath: '/p/final.mp4',
  });
  const s = a.join(' ');
  assert.ok(s.includes('/p/intro.mp4'), 'intro path present');
  assert.ok(s.includes('/p/proof.mp4'), 'main path present');
  assert.ok(s.includes('concat=n=2'), 'concat filter for 2 inputs');
  assert.equal(a[a.length - 1], '/p/final.mp4');
});

test('buildIntroConcatArgs: filter_complex contains concat=n=2:v=1:a=0', () => {
  const a = buildIntroConcatArgs({ introPath: '/i.mp4', mainPath: '/m.mp4', outPath: '/o.mp4' });
  const filterIdx = a.indexOf('-filter_complex');
  assert.ok(filterIdx >= 0, '-filter_complex present');
  const filter = a[filterIdx + 1];
  assert.ok(filter.includes('concat=n=2:v=1:a=0'), 'concat n=2 v=1 a=0 in filter');
});

test('buildIntroConcatArgs: normalizes both inputs to SAR 1:1 before concat', () => {
  const a = buildIntroConcatArgs({ introPath: '/i.mp4', mainPath: '/m.mp4', outPath: '/o.mp4' });
  const filter = a[a.indexOf('-filter_complex') + 1];
  // Both streams pass through setsar=1 so a sample-aspect mismatch can't make
  // concat write a zero-frame file.
  assert.ok(filter.includes('[0:v]setsar=1'), 'intro stream SAR-normalized');
  assert.ok(filter.includes('[1:v]setsar=1'), 'main stream SAR-normalized');
});

// ── buildIntroCardHtml ───────────────────────────────────────────────────────

test('buildIntroCardHtml: HTML-escapes < and & in label', () => {
  const html = buildIntroCardHtml({ label: 'repo<&>"test', title: 'normal title' });
  assert.ok(html.includes('repo&lt;&amp;&gt;&quot;test'), 'label chars escaped');
  assert.ok(!html.includes('repo<&>'), 'raw unescaped chars not present');
});

test('buildIntroCardHtml: HTML-escapes < and & in title', () => {
  const html = buildIntroCardHtml({ label: 'my-repo#42', title: '<script>alert("xss")</script>' });
  assert.ok(html.includes('&lt;script&gt;'), 'title angle brackets escaped');
  assert.ok(html.includes('&quot;xss&quot;'), 'title quotes escaped');
  assert.ok(!html.includes('<script>'), 'raw script tag not present');
});

test('buildIntroCardHtml: contains both the label and title text', () => {
  const label = 'bossanova#99';
  const title = 'Port proof video post-processing';
  const html = buildIntroCardHtml({ label, title });
  assert.ok(html.includes(label), 'label present in output');
  assert.ok(html.includes(title), 'title present in output');
});

test('buildIntroCardHtml: white-on-black card structure (label white, title grey)', () => {
  const html = buildIntroCardHtml({ label: 'L', title: 'T' });
  // Background is black
  assert.ok(html.includes('background:#000'), 'black background');
  // Label is white
  assert.ok(html.includes('color:#fff'), 'white label color');
  // Title is grey (clamped)
  assert.ok(html.includes('color:#b8b8b8'), 'grey title color');
});

test('buildIntroCardHtml: title is visually clamped to 2 lines', () => {
  const html = buildIntroCardHtml({ label: 'L', title: 'T' });
  assert.ok(html.includes('-webkit-line-clamp:2'), 'line-clamp:2 present');
});
