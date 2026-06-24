// Unit tests for the pure arg builder in proof-poster.mjs.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildPosterArgs } from './proof-poster.mjs';

test('buildPosterArgs: two inputs (screenshot, play button), single PNG frame out', () => {
  const a = buildPosterArgs({
    base: '/p/02-shot.png',
    playButton: '/p/play.png',
    outPath: '/p/poster.png',
  });
  // both inputs present, in order
  const i0 = a.indexOf('/p/02-shot.png');
  const i1 = a.indexOf('/p/play.png');
  assert.ok(i0 > 0 && i1 > i0, 'screenshot is input 0, play button input 1');
  assert.ok(a.includes('-frames:v') && a.includes('1'), 'emits a single frame');
  assert.equal(a[a.length - 1], '/p/poster.png');
});

test('buildPosterArgs: default filtergraph has drawbox scrim and centered overlay', () => {
  const g = buildPosterArgs({ base: '/b.png', playButton: '/p.png', outPath: '/o.png' }).find((x) =>
    x.includes('overlay'),
  );
  assert.ok(g, 'filtergraph arg found');
  assert.match(g, /\[1:v\]scale=150:-1/, 'play button scaled to 150');
  assert.match(g, /drawbox=x=0:y=0:w=iw:h=ih:color=black@0\.16:t=fill/, 'drawbox scrim present');
  assert.match(
    g,
    /overlay=\(main_w-overlay_w\)\/2:\(main_h-overlay_h\)\/2\[v\]/,
    'centered overlay present',
  );
});

test('buildPosterArgs: custom scaleW and scrim are honored', () => {
  const g = buildPosterArgs({
    base: '/b.png',
    playButton: '/p.png',
    outPath: '/o.png',
    scaleW: 200,
    scrim: 0.3,
  }).find((x) => x.includes('overlay'));
  assert.match(g, /scale=200:-1/, 'custom scaleW=200 honored');
  assert.match(g, /black@0\.3:/, 'custom scrim=0.3 honored');
});

test('buildPosterArgs: maps the composed [v] label to the output', () => {
  const a = buildPosterArgs({ base: '/b.png', playButton: '/p.png', outPath: '/o.png' });
  const mi = a.indexOf('-map');
  assert.ok(mi >= 0, '-map flag present');
  assert.equal(a[mi + 1], '[v]', '-map [v]');
});
