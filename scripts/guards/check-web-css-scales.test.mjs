import assert from 'node:assert/strict'
import test from 'node:test'
import { compareBaseline, scaleLiterals } from './check-web-css-scales.mjs'

const categories = (css) =>
  scaleLiterals(css).map((literal) => `${literal.category} ${literal.text}`)

for (const [name, css, expected] of [
  ['a radius', '.a { border-radius: 6px; }', ['radius 6px']],
  ['a corner radius', '.a { border-top-left-radius: 0 4px; }', ['radius 4px']],
  [
    'type',
    '.a { font-size: 13px; font-weight: 500; line-height: 1.45; letter-spacing: -0.1px; }',
    ['font-size 13px', 'font-weight 500', 'line-height 1.45', 'letter-spacing -0.1px'],
  ],
  [
    'a font shorthand',
    '.a { font: 500 11px/1.5 var(--mono); }',
    ['font-weight 500', 'font-size 11px', 'line-height 1.5'],
  ],
  [
    'spacing',
    '.a { padding: 8px 0; margin: -4px auto; gap: 6px; }',
    ['space 8px', 'space -4px', 'space 6px'],
  ],
  ['a size', '.a { width: 28px; max-width: calc(100% - 16px); }', ['size 28px', 'size 16px']],
  [
    'a stroke',
    '.a { border: 1px solid var(--border); outline-offset: 2px; }',
    ['stroke 1px', 'stroke 2px'],
  ],
  ['a shadow length', '.a { box-shadow: inset 0 0 0 1px var(--red); }', ['shadow 1px']],
  [
    'motion',
    '.a { transition: color 120ms ease-out, transform 0.2s cubic-bezier(0.22, 1, 0.36, 1); }',
    ['duration 120ms', 'duration 0.2s', 'easing cubic-bezier(0.22, 1, 0.36, 1)'],
  ],
  ['a layer', '.a { z-index: 80; }', ['z-index 80']],
  ['a fallback literal', '.a { padding: var(--sp-8, 8px); }', ['space 8px']],
  [
    'a component-scoped custom property',
    '.a { --pad-y: 4px; --status-font: 11px; }',
    ['space 4px', 'font-size 11px'],
  ],
  [
    'tokens only',
    '.a { border-radius: var(--r-md); padding: var(--sp-8) calc(-1 * var(--sp-4)); z-index: var(--z-dialog); transition: color var(--dur-fast) var(--ease-out-quint); }',
    [],
  ],
  [
    'layout values',
    '.a { margin: 0 auto; width: 100%; height: 100dvh; inset: 0; flex: 1 1 0; grid-template-columns: minmax(0, 1fr); z-index: auto; }',
    [],
  ],
  [
    'easing keywords',
    '.a { transition: opacity var(--dur-fast) ease-out; animation: spin var(--dur-loop-sm) linear infinite; }',
    [],
  ],
  ['keywords', '.a { line-height: normal; font-weight: inherit; letter-spacing: normal; }', []],
  [
    'the token layer',
    ':root { --r-md: 8px; }\n@media (max-width: 640px) { :root { --sp-x: 4px; } }',
    [],
  ],
  ['a non-token declaration on :root', ':root { font-size: 15px; }', ['font-size 15px']],
  [
    'colours inside a shadow',
    '.a { box-shadow: lch(0 0 0 / 0.3) 0 0.5px 1px 1px; }',
    ['shadow 0.5px', 'shadow 1px', 'shadow 1px'],
  ],
  ['keyframes', '@keyframes pulse { from { width: 4px; } }', []],
  ['comments and strings', '/* padding: 8px */ .a { content: "12px"; }', []],
  ['token names with digits', '.a { padding: var(--sp-12) var(--fs-14); }', []],
]) {
  test(`classifier: ${name}`, () => assert.deepEqual(categories(css), expected))
}

test('reports the line each literal sits on', () => {
  assert.deepEqual(
    scaleLiterals('.a {\n  padding: var(--sp-4);\n  border-radius: 6px;\n}').map((l) => l.line),
    [3],
  )
})

test('ratchet accepts exact counts', () => {
  assert.deepEqual(compareBaseline({ 'a.css': { size: 2 } }, { 'a.css': { size: 2 } }), [])
})
for (const [name, counts, baseline, kind] of [
  ['new file', { 'a.css': { radius: 1 } }, {}, 'new-instance'],
  [
    'new category in a baselined file',
    { 'a.css': { size: 2, radius: 1 } },
    { 'a.css': { size: 2 } },
    'new-instance',
  ],
  [
    'new literal in a baselined category',
    { 'a.css': { size: 3 } },
    { 'a.css': { size: 2 } },
    'new-instance',
  ],
  ['reduced count', { 'a.css': { size: 1 } }, { 'a.css': { size: 2 } }, 'stale-entry'],
  ['cleaned file', {}, { 'a.css': { size: 2 } }, 'stale-entry'],
  ['unsorted baseline', {}, { 'z.css': { size: 1 }, 'a.css': { size: 1 } }, 'invalid-baseline'],
  ['invalid count', {}, { 'a.css': { size: 0 } }, 'invalid-baseline'],
  ['flat count', {}, { 'a.css': 2 }, 'invalid-baseline'],
]) {
  test(`ratchet rejects ${name}`, () => {
    assert.ok(compareBaseline(counts, baseline).some((failure) => failure.kind === kind))
  })
}

test('a hard-coded radius, layer or duration in a web stylesheet fails the ratchet', () => {
  // The frame header's round button shipped a pill radius as 9999px, the drawer a raw layer
  // number and the link a raw fade before the scales existed.
  const css =
    '.frame-hd-btn {\n  border-radius: 9999px;\n  z-index: 70;\n  transition: color 120ms ease-out;\n}\n'
  const file = 'services/web/src/app/frame/FrameHeader.css'
  const found = scaleLiterals(css)
  const counts = {}
  for (const { category } of found) counts[category] = (counts[category] ?? 0) + 1
  const failures = compareBaseline({ [file]: counts }, {})
  assert.deepEqual(
    failures.map((failure) => [failure.kind, failure.file, failure.category]).sort(),
    [
      ['new-instance', file, 'duration'],
      ['new-instance', file, 'radius'],
      ['new-instance', file, 'z-index'],
    ],
  )
  const tokenised =
    '.frame-hd-btn {\n  border-radius: var(--r-pill);\n  z-index: var(--z-drawer);\n  transition: color var(--dur-brisk) ease-out;\n}\n'
  assert.deepEqual(scaleLiterals(tokenised), [])
})
